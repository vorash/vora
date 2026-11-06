package api

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	v1 "github.com/vorash/vora/api/proto/v1"
)

// heldCreate holds the first CreatePool before the pool exists, which is the window
// in which the runner answers NotFound for it. Later calls go straight through.
type heldCreate struct {
	*fakeRunner
	held    atomic.Bool
	entered chan struct{}
	release chan struct{}
}

func newHeldCreate() *heldCreate {
	return &heldCreate{fakeRunner: &fakeRunner{}, entered: make(chan struct{}), release: make(chan struct{})}
}

func (h *heldCreate) CreatePool(ctx context.Context, req *v1.CreatePoolRequest) (*v1.Pool, error) {
	if h.held.CompareAndSwap(false, true) {
		close(h.entered)
		<-h.release
	}
	return h.fakeRunner.CreatePool(ctx, req)
}

// twoRunners serves run_a and run_b, with placement preferring run_a until
// preferRunB is called.
type twoRunners struct {
	s    *Server
	ts   *httptest.Server
	key  string
	runA *heldCreate
	runB *fakeRunner
	logs *bytes.Buffer

	releaseOnce sync.Once
}

func newTwoRunners(t *testing.T) *twoRunners {
	t.Helper()
	logs := &bytes.Buffer{}
	f := &twoRunners{runA: newHeldCreate(), runB: &fakeRunner{}, logs: logs}
	f.s = New(Options{
		Store: NewMemStore(), JoinToken: joinToken, BootstrapKey: bootstrap, Now: time.Now,
		Logger: slog.New(slog.NewTextHandler(logs, nil)),
		DialRunner: func(addr string) (RunnerClient, error) {
			if addr == "10.0.0.1:9200" {
				return f.runA, nil
			}
			return f.runB, nil
		},
	})
	for id, addr := range map[string]string{"run_a": "10.0.0.1:9200", "run_b": "10.0.0.2:9200"} {
		if _, err := f.s.Register(metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Bearer "+joinToken)), &v1.RegisterRunnerRequest{
			RunnerId: id, Address: addr, Backends: []string{"bwrap"}, NetworkModes: []string{"none"},
		}); err != nil {
			t.Fatal(err)
		}
	}
	f.occupy(t, "x", "run_b", 50)
	f.ts = httptest.NewServer(f.s.Handler())
	t.Cleanup(f.ts.Close)
	// Before the server closes, which waits for a declare still held: a test that
	// fails with one held must report that, not hang until the test binary times out.
	t.Cleanup(f.releaseCreate)
	f.key = tenantKey(t, f.s, "tenant-a")
	return f
}

func (f *twoRunners) releaseCreate() {
	f.releaseOnce.Do(func() { close(f.runA.release) })
}

func (f *twoRunners) occupy(t *testing.T, name, runner string, cpu int64) {
	t.Helper()
	if err := f.s.store.InsertPoolRoute(context.Background(), PoolRoute{TenantID: "other", Name: name, PoolID: "pool_" + name, RunnerID: runner, MaxSize: 1, CPUMillis: cpu}); err != nil {
		t.Fatal(err)
	}
}

func (f *twoRunners) preferRunB(t *testing.T) {
	t.Helper()
	f.occupy(t, "y", "run_a", 500)
}

// declareHeld starts a declare that stops inside run_a's first CreatePool.
func (f *twoRunners) declareHeld(t *testing.T) <-chan answer {
	t.Helper()
	done := make(chan answer, 1)
	go func() { done <- call(t, http.MethodPut, f.ts.URL+"/v1/pools/py", f.key, poolBody(1, 1)) }()
	<-f.runA.entered
	return done
}

// A read that lands while a create is in flight must not release the route: the
// next declare would place the name on another runner, and one of the two pools
// would then have no route that could reach or delete it.
func TestReadsDuringACreateKeepTheRoute(t *testing.T) {
	f := newTwoRunners(t)
	first := f.declareHeld(t)

	got := call(t, http.MethodGet, f.ts.URL+"/v1/pools/py", f.key, nil)
	if got.code != http.StatusOK || decodePool(t, got.body).GetState() != v1.PoolState_POOL_NOT_ON_RUNNER {
		t.Fatalf("get mid-create = %d %s, want 200 POOL_NOT_ON_RUNNER", got.code, got.body)
	}
	if list := call(t, http.MethodGet, f.ts.URL+"/v1/pools", f.key, nil); !strings.Contains(list.body, "POOL_NOT_ON_RUNNER") {
		t.Fatalf("list mid-create = %d %s", list.code, list.body)
	}
	if run := call(t, http.MethodPost, f.ts.URL+"/v1/pools/py/run", f.key, map[string]any{"command": "true", "timeoutSeconds": "5"}); run.code != http.StatusConflict {
		t.Fatalf("run mid-create = %d %s, want 409", run.code, run.body)
	}
	if route, ok := routeNamed(t, f.s, "tenant-a", "py"); !ok || route.RunnerID != "run_a" {
		t.Fatalf("after reads mid-create the route is %+v, %v", route, ok)
	}

	// With the route kept, a second declare goes to the same runner however busy it
	// looks, and the runner's own idempotence makes the two declares one pool.
	f.preferRunB(t)
	second := call(t, http.MethodPut, f.ts.URL+"/v1/pools/py", f.key, poolBody(1, 1))
	if second.code != http.StatusOK {
		t.Fatalf("second declare = %d %s, want 200", second.code, second.body)
	}
	f.releaseCreate()
	firstAnswer := <-first
	if firstAnswer.code != http.StatusCreated {
		t.Fatalf("first declare = %d %s, want 201", firstAnswer.code, firstAnswer.body)
	}
	id := decodePool(t, second.body).GetId()
	if decodePool(t, firstAnswer.body).GetId() != id {
		t.Fatalf("the two declares named %s and %s", decodePool(t, firstAnswer.body).GetId(), id)
	}
	if f.runA.poolCount() != 1 || f.runB.poolCount() != 0 {
		t.Fatalf("run_a holds %d pools, run_b %d, want 1 and 0", f.runA.poolCount(), f.runB.poolCount())
	}
	if route, ok := routeNamed(t, f.s, "tenant-a", "py"); !ok || route.PoolID != id || route.RunnerID != "run_a" {
		t.Fatalf("route = %+v, %v, want pool %s on run_a", route, ok, id)
	}
}

// A DELETE still releases the route on NotFound, so it can overtake a create. The
// create's late answer must then remove what it made.
func TestDeclareThatLosesItsRouteRemovesItsPool(t *testing.T) {
	t.Run("a new declare holds the name elsewhere", func(t *testing.T) {
		f := newTwoRunners(t)
		first := f.declareHeld(t)
		if got := call(t, http.MethodDelete, f.ts.URL+"/v1/pools/py", f.key, nil); got.code != http.StatusNoContent {
			t.Fatalf("delete mid-create = %d %s, want 204", got.code, got.body)
		}
		f.preferRunB(t)
		second := declare(t, f.ts, f.key, "py", poolBody(1, 1))
		if f.runB.pool(second.GetId()) == nil {
			t.Fatalf("the second declare did not land on run_b: run_a %d pools, run_b %d", f.runA.poolCount(), f.runB.poolCount())
		}

		f.releaseCreate()
		late := <-first
		if late.code != http.StatusServiceUnavailable || !strings.Contains(late.body, "retry") {
			t.Fatalf("late declare = %d %s, want 503 asking for a retry", late.code, late.body)
		}
		if f.runA.poolCount() != 0 {
			t.Fatalf("the late declare left %d pools on run_a, which no route names", f.runA.poolCount())
		}
		if route, ok := routeNamed(t, f.s, "tenant-a", "py"); !ok || route.PoolID != second.GetId() || route.RunnerID != "run_b" {
			t.Fatalf("route = %+v, %v, want the second declare's", route, ok)
		}
		if f.runB.pool(second.GetId()) == nil {
			t.Fatal("the late declare removed the second declare's pool")
		}
	})
	t.Run("nothing holds the name", func(t *testing.T) {
		f := newTwoRunners(t)
		first := f.declareHeld(t)
		if got := call(t, http.MethodDelete, f.ts.URL+"/v1/pools/py", f.key, nil); got.code != http.StatusNoContent {
			t.Fatalf("delete mid-create = %d %s, want 204", got.code, got.body)
		}
		f.releaseCreate()
		if late := <-first; late.code != http.StatusServiceUnavailable {
			t.Fatalf("late declare = %d %s, want 503", late.code, late.body)
		}
		if _, ok := routeNamed(t, f.s, "tenant-a", "py"); ok || f.runA.poolCount() != 0 {
			t.Fatalf("after a delete won: run_a holds %d pools, route exists %v", f.runA.poolCount(), ok)
		}
	})
	t.Run("a runner that refuses the delete leaves the pool named in the log", func(t *testing.T) {
		f := newTwoRunners(t)
		first := f.declareHeld(t)
		if got := call(t, http.MethodDelete, f.ts.URL+"/v1/pools/py", f.key, nil); got.code != http.StatusNoContent {
			t.Fatalf("delete mid-create = %d %s, want 204", got.code, got.body)
		}
		f.runA.mu.Lock()
		f.runA.deleteErr = status.Error(codes.Unavailable, "gone")
		f.runA.mu.Unlock()
		f.releaseCreate()
		if late := <-first; late.code != http.StatusServiceUnavailable {
			t.Fatalf("late declare = %d %s, want 503", late.code, late.body)
		}
		var id string
		f.runA.mu.Lock()
		for pid := range f.runA.pools {
			id = pid
		}
		f.runA.mu.Unlock()
		if id == "" || !strings.Contains(f.logs.String(), "a pool no route names was not removed") || !strings.Contains(f.logs.String(), id) {
			t.Fatalf("pool %q on run_a, logs:\n%s", id, f.logs)
		}
	})
}

// vanishingRouteStore has a route beat the insert and be gone by the read after it.
type vanishingRouteStore struct{ Store }

func (vanishingRouteStore) InsertPoolRoute(context.Context, PoolRoute) error {
	return ErrPoolRouteExists
}

func (vanishingRouteStore) PoolRoute(context.Context, string, string) (PoolRoute, error) {
	return PoolRoute{}, ErrNotFound
}

// Losing the insert to a route that a delete then released is a race, not a failure:
// the same declare a moment later finds the name free.
func TestDeclareThatFindsTheWinningRouteGoneAsksForARetry(t *testing.T) {
	fr := &fakeRunner{}
	s, ts := newTestServer(t, fr)
	key := tenantKey(t, s, "tenant-a")
	real := s.store
	s.store = vanishingRouteStore{real}
	resp := do(t, http.MethodPut, ts.URL+"/v1/pools/py", key, poolBody(1, 1))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("declare = %d, Retry-After %q, want 503 with Retry-After", resp.StatusCode, resp.Header.Get("Retry-After"))
	}
	if fr.callCount() != 0 {
		t.Fatalf("%d runner calls with no route", fr.callCount())
	}
	s.store = real
	declare(t, ts, key, "py", poolBody(1, 1))
}

// changedRouteStore has every final write find the route changed.
type changedRouteStore struct{ Store }

func (changedRouteStore) SetPoolRoute(context.Context, PoolRoute, string) error {
	return ErrPoolRouteChanged
}

// A pool the runner already had belongs to the declare that created it. A declare
// that only found it must not drain a pool that others may be running on.
func TestDeclareThatLosesItsRouteLeavesAPoolItDidNotCreate(t *testing.T) {
	fr := &fakeRunner{}
	s, ts := newTestServer(t, fr)
	key := tenantKey(t, s, "tenant-a")
	p := declare(t, ts, key, "py", poolBody(1, 1))
	s.store = changedRouteStore{s.store}
	got := call(t, http.MethodPut, ts.URL+"/v1/pools/py", key, poolBody(1, 1))
	if got.code != http.StatusServiceUnavailable {
		t.Fatalf("redeclare = %d %s, want 503", got.code, got.body)
	}
	if fr.pool(p.GetId()) == nil {
		t.Fatal("a declare that created nothing deleted the pool it found")
	}
}

// Two declares after a runner lost the pool both read the old id, and the runner
// answers both with the one pool the first made. The second's write finds the row
// already naming that pool, which is success: reading it as a lost race would
// delete the live pool.
func TestRedeclaresOfALostPoolAgreeOnOnePool(t *testing.T) {
	f := newTwoRunners(t)
	f.runA.held.Store(true)
	lost := declare(t, f.ts, f.key, "py", poolBody(1, 1))
	f.runA.mu.Lock()
	delete(f.runA.pools, lost.GetId())
	f.runA.mu.Unlock()

	f.runA.held.Store(false)
	first := f.declareHeld(t)
	second := call(t, http.MethodPut, f.ts.URL+"/v1/pools/py", f.key, poolBody(1, 1))
	if second.code != http.StatusOK {
		t.Fatalf("second redeclare = %d %s, want 200", second.code, second.body)
	}
	f.releaseCreate()
	firstAnswer := <-first
	if firstAnswer.code != http.StatusOK {
		t.Fatalf("first redeclare = %d %s, want 200", firstAnswer.code, firstAnswer.body)
	}
	id := decodePool(t, second.body).GetId()
	if id == lost.GetId() || decodePool(t, firstAnswer.body).GetId() != id {
		t.Fatalf("lost %s, redeclares named %s and %s", lost.GetId(), decodePool(t, firstAnswer.body).GetId(), id)
	}
	if f.runA.pool(id) == nil || f.runA.poolCount() != 1 {
		t.Fatalf("run_a holds %d pools, has %s: %v", f.runA.poolCount(), id, f.runA.pool(id) != nil)
	}
	if route, ok := routeNamed(t, f.s, "tenant-a", "py"); !ok || route.PoolID != id {
		t.Fatalf("route = %+v, %v, want pool %s", route, ok, id)
	}
}

// The final write is what stops a late declare. Both stores must keep to it; this
// is the memory store's half, and tests/integration/api_postgres_test.go is the
// other's.
func TestSetPoolRouteComparesThePoolID(t *testing.T) {
	store := NewMemStore()
	ctx := context.Background()
	route := PoolRoute{TenantID: "t", Name: "py", PoolID: "pool_a", RunnerID: "run_a"}
	moved := route
	moved.PoolID = "pool_b"
	if err := store.SetPoolRoute(ctx, moved, "pool_a"); err != ErrPoolRouteChanged {
		t.Fatalf("set with no route = %v, want ErrPoolRouteChanged", err)
	}
	if _, err := store.PoolRoute(ctx, "t", "py"); err != ErrNotFound {
		t.Fatalf("a failed set wrote a route: %v", err)
	}
	if err := store.InsertPoolRoute(ctx, route); err != nil {
		t.Fatal(err)
	}
	if err := store.SetPoolRoute(ctx, moved, "pool_c"); err != ErrPoolRouteChanged {
		t.Fatalf("set expecting another pool = %v, want ErrPoolRouteChanged", err)
	}
	if got, _ := store.PoolRoute(ctx, "t", "py"); got != route {
		t.Fatalf("a failed set changed the route to %+v", got)
	}
	if err := store.SetPoolRoute(ctx, moved, "pool_a"); err != nil {
		t.Fatalf("set expecting the current pool: %v", err)
	}
	if err := store.SetPoolRoute(ctx, moved, "pool_a"); err != nil {
		t.Fatalf("set of the pool the route already names: %v", err)
	}
	if got, _ := store.PoolRoute(ctx, "t", "py"); got != moved {
		t.Fatalf("route = %+v, want %+v", got, moved)
	}
}
