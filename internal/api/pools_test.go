package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	"google.golang.org/protobuf/encoding/protojson"

	v1 "github.com/vorash/vora/api/proto/v1"
	"github.com/vorash/vora/internal/ulid"
)

func poolBody(size, maxSize uint32) map[string]any {
	return map[string]any{
		"sandbox": map[string]any{
			"image": "vora/base", "backend": "bwrap",
			"resources": map[string]any{"cpuMillis": "100", "memoryBytes": "1048576", "pids": "32"},
			"network":   map[string]any{"mode": "NETWORK_NONE"},
		},
		"size": size, "maxSize": maxSize,
	}
}

func mintKey(t *testing.T, s *Server, tenant, agent string, scopes ...string) string {
	t.Helper()
	plaintext, hash, err := NewAPIKey()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.store.CreateKey(context.Background(), APIKey{
		ID: ulid.NewAPIKeyID(), TenantID: tenant, AgentID: agent, KeyHash: hash, Scopes: scopes, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	return plaintext
}

func tenantKey(t *testing.T, s *Server, tenant string) string {
	t.Helper()
	return mintKey(t, s, tenant, "", ScopeSandbox)
}

type answer struct {
	code int
	body string
}

func call(t *testing.T, method, url, key string, body any) answer {
	t.Helper()
	resp := do(t, method, url, key, body)
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return answer{resp.StatusCode, string(data)}
}

func decodePool(t *testing.T, body string) *v1.Pool {
	t.Helper()
	p := &v1.Pool{}
	if err := protojson.Unmarshal([]byte(body), p); err != nil {
		t.Fatalf("decode pool %q: %v", body, err)
	}
	return p
}

func declare(t *testing.T, ts *httptest.Server, key, name string, body any) *v1.Pool {
	t.Helper()
	got := call(t, http.MethodPut, ts.URL+"/v1/pools/"+name, key, body)
	if got.code != http.StatusCreated {
		t.Fatalf("declare %s = %d %s, want 201", name, got.code, got.body)
	}
	return decodePool(t, got.body)
}

// committed is placement's view of the runner: the books a leak would show in.
func committed(s *Server) sandboxResources {
	return s.committedByRunner(context.Background())["run_test"]
}

func routeNamed(t *testing.T, s *Server, tenant, name string) (PoolRoute, bool) {
	t.Helper()
	route, err := s.store.PoolRoute(context.Background(), tenant, name)
	if errors.Is(err, ErrNotFound) {
		return PoolRoute{}, false
	}
	if err != nil {
		t.Fatal(err)
	}
	return route, true
}

func TestPoolDeclareFillsIdentityAndActor(t *testing.T) {
	fr := &fakeRunner{}
	s, ts := newTestServer(t, fr)
	key := tenantKey(t, s, "tenant-a")
	stored, err := s.store.KeyByHash(context.Background(), HashKey(key))
	if err != nil {
		t.Fatal(err)
	}

	p := declare(t, ts, key, "py", poolBody(1, 2))
	if p.GetRunnerId() != "" {
		t.Fatalf("a tenant saw its pool's runner: %q", p.GetRunnerId())
	}
	req := fr.creates[0]
	if req.GetSpec().GetTenantId() != "tenant-a" || req.GetSpec().GetName() != "py" {
		t.Fatalf("CreatePool spec = %v", req.GetSpec())
	}
	if req.GetActor().GetKeyId() != stored.ID {
		t.Fatalf("CreatePool actor = %q, want %q", req.GetActor().GetKeyId(), stored.ID)
	}

	got := call(t, http.MethodGet, ts.URL+"/v1/pools/py", key, nil)
	if got.code != http.StatusOK || fr.gets[0].GetTenantId() != "tenant-a" || fr.gets[0].GetPoolId() != p.GetId() {
		t.Fatalf("get = %d %s, GetPool %v", got.code, got.body, fr.gets[0])
	}

	fr.runChunks = []*v1.RunChunk{{Payload: &v1.RunChunk_RunDone{RunDone: &v1.PoolRunDone{}}}}
	if got := call(t, http.MethodPost, ts.URL+"/v1/pools/py/run", key, map[string]any{"command": "true", "timeoutSeconds": "5"}); got.code != http.StatusOK {
		t.Fatalf("run = %d %s", got.code, got.body)
	}
	if run := fr.runs[0]; run.GetTenantId() != "tenant-a" || run.GetPoolId() != p.GetId() || run.GetActor().GetKeyId() != stored.ID {
		t.Fatalf("RunInPool = %v", run)
	}

	if got := call(t, http.MethodDelete, ts.URL+"/v1/pools/py", key, nil); got.code != http.StatusNoContent {
		t.Fatalf("delete = %d %s", got.code, got.body)
	}
	del := fr.deletes[0]
	if del.GetTenantId() != "tenant-a" || del.GetPoolId() != p.GetId() || del.GetActor().GetKeyId() != stored.ID {
		t.Fatalf("DeletePool = %v", del)
	}
	// wait_ms must fit inside the call's deadline, or a slow drain's answer is lost.
	if del.GetWaitMs() <= 0 || del.GetWaitMs() >= poolDeleteDeadline.Milliseconds() {
		t.Fatalf("wait_ms = %d, want within (0, %d)", del.GetWaitMs(), poolDeleteDeadline.Milliseconds())
	}
}

func TestPoolDeclareRefusesWhatTheKeyMayNotSay(t *testing.T) {
	fr := &fakeRunner{}
	s, ts := newTestServer(t, fr)
	key := tenantKey(t, s, "tenant-a")

	other := poolBody(1, 1)
	other["tenantId"] = "tenant-b"
	if got := call(t, http.MethodPut, ts.URL+"/v1/pools/py", key, other); got.code != http.StatusForbidden {
		t.Fatalf("declare for another tenant = %d %s, want 403", got.code, got.body)
	}
	named := poolBody(1, 1)
	named["name"] = "not-py"
	if got := call(t, http.MethodPut, ts.URL+"/v1/pools/py", key, named); got.code != http.StatusBadRequest {
		t.Fatalf("body name differs from path = %d %s, want 400", got.code, got.body)
	}
	if got := call(t, http.MethodPut, ts.URL+"/v1/pools/py", bootstrap, poolBody(1, 1)); got.code != http.StatusBadRequest {
		t.Fatalf("global admin without tenant = %d %s, want 400", got.code, got.body)
	}
	if fr.callCount() != 0 {
		t.Fatalf("%d refused declares reached the runner", fr.callCount())
	}

	bound := mintKey(t, s, "tenant-a", "agent-1", ScopeSandbox)
	smuggled := poolBody(1, 1)
	smuggled["agentId"] = "agent-2"
	declare(t, ts, bound, "bound", smuggled)
	if got := fr.creates[0].GetSpec().GetAgentId(); got != "agent-1" {
		t.Fatalf("an agent-bound key declared for agent %q", got)
	}
	// Names are per tenant, so another agent's key cannot take this one over. It
	// gets the answer every other route gives it, and the runner is not asked: its
	// refusal names the pool.
	otherAgent := mintKey(t, s, "tenant-a", "agent-2", ScopeSandbox)
	want := call(t, http.MethodGet, ts.URL+"/v1/pools/nobody-has-this", otherAgent, nil)
	before := fr.callCount()
	got := call(t, http.MethodPut, ts.URL+"/v1/pools/bound", otherAgent, poolBody(1, 1))
	if got.code != http.StatusNotFound || got != want {
		t.Fatalf("another agent redeclaring the name = %d %s, want %d %s", got.code, got.body, want.code, want.body)
	}
	if fr.callCount() != before {
		t.Fatalf("another agent's declare made %d runner calls", fr.callCount()-before)
	}
	// The agent that owns it, and a key for the whole tenant, still reach the runner.
	if got := call(t, http.MethodPut, ts.URL+"/v1/pools/bound", bound, poolBody(1, 1)); got.code != http.StatusOK {
		t.Fatalf("the owner redeclaring = %d %s, want 200", got.code, got.body)
	}
	owned := poolBody(1, 1)
	owned["agentId"] = "agent-1"
	if got := call(t, http.MethodPut, ts.URL+"/v1/pools/bound", tenantKey(t, s, "tenant-a"), owned); got.code != http.StatusOK {
		t.Fatalf("a tenant key redeclaring = %d %s, want 200", got.code, got.body)
	}
}

// Every route a tenant can name a pool on, with the body a client would send.
func poolRoutes(name string) []struct {
	method, path string
	body         any
} {
	return []struct {
		method, path string
		body         any
	}{
		{http.MethodGet, "/v1/pools/" + name, nil},
		{http.MethodDelete, "/v1/pools/" + name, nil},
		{http.MethodPost, "/v1/pools/" + name + "/run", map[string]any{"command": "true", "timeoutSeconds": "5"}},
		{http.MethodGet, "/v1/pools/" + name + "/sandboxes/" + memberID + "/execs", nil},
		{http.MethodGet, "/v1/pools/" + name + "/sandboxes/" + memberID + "/events", nil},
	}
}

const memberID = "sbx_01JZZZZZZZZZZZZZZZZZZZZZZZ"

// A tenant must not be able to tell a pool of another tenant from a name nobody
// holds, at any route and however it addresses the pool.
func TestPoolInAnotherTenantIsIndistinguishableFromNone(t *testing.T) {
	fr := &fakeRunner{}
	s, ts := newTestServer(t, fr)
	owner := tenantKey(t, s, "tenant-a")
	p := declare(t, ts, owner, "py", poolBody(1, 1))
	fr.members = map[string]*v1.Sandbox{memberID: {Id: memberID, PoolId: p.GetId(), Spec: &v1.SandboxSpec{TenantId: "tenant-a"}}}
	before := fr.callCount()

	stranger := tenantKey(t, s, "tenant-b")
	otherAgent := mintKey(t, s, "tenant-a", "agent-2", ScopeSandbox)
	for _, route := range poolRoutes("py") {
		none := route
		none.path = strings.Replace(route.path, "/py", "/nobody-has-this", 1)
		want := call(t, none.method, ts.URL+none.path, stranger, none.body)
		if want.code != http.StatusNotFound {
			t.Fatalf("%s %s on no pool = %d %s", none.method, none.path, want.code, want.body)
		}
		for name, key := range map[string]string{"another tenant": stranger, "another agent": otherAgent} {
			if got := call(t, route.method, ts.URL+route.path, key, route.body); got != want {
				t.Errorf("%s %s by %s = %v, but on no pool %v", route.method, route.path, name, got, want)
			}
		}
		// Naming the owner on the query string gets a tenant key nothing.
		if got := call(t, route.method, ts.URL+route.path+"?tenant_id=tenant-a", stranger, route.body); got != want {
			t.Errorf("%s %s?tenant_id=tenant-a = %v, but on no pool %v", route.method, route.path, got, want)
		}
	}
	if fr.callCount() != before {
		t.Fatalf("a stranger's requests made %d runner calls", fr.callCount()-before)
	}
	if _, ok := routeNamed(t, s, "tenant-a", "py"); !ok || fr.pool(p.GetId()) == nil {
		t.Fatal("a stranger's delete touched the pool")
	}

	if list := call(t, http.MethodGet, ts.URL+"/v1/pools", stranger, nil); strings.Contains(list.body, p.GetId()) {
		t.Fatalf("a stranger's listing shows the pool: %s", list.body)
	}

	// The same name in another tenant is that tenant's own pool.
	theirs := declare(t, ts, stranger, "py", poolBody(1, 1))
	if theirs.GetId() == p.GetId() {
		t.Fatal("two tenants share a pool")
	}
}

func TestGlobalAdminNamesTheTenant(t *testing.T) {
	fr := &fakeRunner{}
	s, ts := newTestServer(t, fr)
	p := declare(t, ts, tenantKey(t, s, "tenant-a"), "py", poolBody(1, 1))

	if got := call(t, http.MethodGet, ts.URL+"/v1/pools/py", bootstrap, nil); got.code != http.StatusBadRequest {
		t.Fatalf("no tenant_id = %d %s, want 400", got.code, got.body)
	}
	got := call(t, http.MethodGet, ts.URL+"/v1/pools/py?tenant_id=tenant-a", bootstrap, nil)
	if got.code != http.StatusOK || decodePool(t, got.body).GetRunnerId() == "" {
		t.Fatalf("admin get = %d %s, want 200 with the runner", got.code, got.body)
	}
	if list := call(t, http.MethodGet, ts.URL+"/v1/pools", bootstrap, nil); !strings.Contains(list.body, p.GetId()) {
		t.Fatalf("admin list = %s", list.body)
	}
}

func TestPoolDeclareIsIdempotent(t *testing.T) {
	fr := &fakeRunner{}
	s, ts := newTestServer(t, fr)
	key := tenantKey(t, s, "tenant-a")
	p := declare(t, ts, key, "py", poolBody(1, 2))

	same := call(t, http.MethodPut, ts.URL+"/v1/pools/py", key, poolBody(1, 2))
	if same.code != http.StatusOK || decodePool(t, same.body).GetId() != p.GetId() {
		t.Fatalf("same spec = %d %s, want 200 and pool %s", same.code, same.body, p.GetId())
	}
	different := call(t, http.MethodPut, ts.URL+"/v1/pools/py", key, poolBody(2, 2))
	if different.code != http.StatusConflict {
		t.Fatalf("different spec = %d %s, want 409", different.code, different.body)
	}
	if route, ok := routeNamed(t, s, "tenant-a", "py"); !ok || route.PoolID != p.GetId() || route.MaxSize != 2 {
		t.Fatalf("route after the refused declare = %+v, %v", route, ok)
	}
}

// Racing declares must not leave two pools for a name, on one runner or two.
func TestRacingDeclaresConvergeOnOnePool(t *testing.T) {
	fr := &fakeRunner{}
	s, ts := newTestServer(t, fr)
	key := tenantKey(t, s, "tenant-a")
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		codes   = map[int]int{}
		seenIDs = map[string]bool{}
	)
	for range 16 {
		wg.Go(func() {
			got := call(t, http.MethodPut, ts.URL+"/v1/pools/py", key, poolBody(1, 2))
			mu.Lock()
			defer mu.Unlock()
			codes[got.code]++
			if got.code < 300 {
				seenIDs[decodePool(t, got.body).GetId()] = true
			}
		})
	}
	wg.Wait()
	if codes[http.StatusCreated] != 1 || codes[http.StatusOK] != 15 {
		t.Fatalf("status counts = %v, want one 201 and fifteen 200", codes)
	}
	if len(fr.pools) != 1 || len(seenIDs) != 1 {
		t.Fatalf("%d pools on the runner, answers named %v", len(fr.pools), seenIDs)
	}
	route, ok := routeNamed(t, s, "tenant-a", "py")
	if !ok || !seenIDs[route.PoolID] {
		t.Fatalf("route = %+v, answers named %v", route, seenIDs)
	}
}

func TestUnsafeReuseGate(t *testing.T) {
	unsafe := func() map[string]any {
		body := poolBody(1, 1)
		body["reuse"] = "POOL_REUSE_UNSAFE_NO_RESET"
		return body
	}
	t.Run("a sandbox key is refused and nothing is reserved", func(t *testing.T) {
		fr := &fakeRunner{}
		s, ts := newTestServer(t, fr)
		got := call(t, http.MethodPut, ts.URL+"/v1/pools/py", tenantKey(t, s, "tenant-a"), unsafe())
		if got.code != http.StatusForbidden {
			t.Fatalf("declare = %d %s, want 403", got.code, got.body)
		}
		if fr.callCount() != 0 || committed(s).count != 0 {
			t.Fatalf("refused declare made %d runner calls, committed %+v", fr.callCount(), committed(s))
		}
	})
	t.Run("the runner's refusal is explained and reserves nothing", func(t *testing.T) {
		fr := &fakeRunner{}
		fr.poolCreateErr = status.Error(codes.FailedPrecondition, "pool: unsafe reuse is not allowed on this runner")
		s, ts := newTestServer(t, fr)
		got := call(t, http.MethodPut, ts.URL+"/v1/pools/py", mintKey(t, s, "tenant-a", "", ScopeAdmin), unsafe())
		if got.code != http.StatusConflict || !strings.Contains(got.body, "unsafe reuse is not allowed") {
			t.Fatalf("declare = %d %s, want 409 saying why", got.code, got.body)
		}
		if _, ok := routeNamed(t, s, "tenant-a", "py"); ok || committed(s).count != 0 {
			t.Fatalf("refused pool kept a route, committed %+v", committed(s))
		}
	})
	t.Run("an allowed pool says it is unsafe to whoever uses it", func(t *testing.T) {
		fr := &fakeRunner{}
		fr.runChunks = []*v1.RunChunk{{Payload: &v1.RunChunk_Run{Run: &v1.PoolRun{Reuse: v1.PoolReuse_POOL_REUSE_UNSAFE_NO_RESET}}}}
		s, ts := newTestServer(t, fr)
		admin := mintKey(t, s, "tenant-a", "", ScopeAdmin)
		p := declare(t, ts, admin, "py", unsafe())
		if p.GetSpec().GetReuse() != v1.PoolReuse_POOL_REUSE_UNSAFE_NO_RESET {
			t.Fatalf("declare answer does not say unsafe: %v", p.GetSpec())
		}
		// A key that could not have declared it still sees what it is using.
		plain := tenantKey(t, s, "tenant-a")
		if got := call(t, http.MethodGet, ts.URL+"/v1/pools/py", plain, nil); !strings.Contains(got.body, "POOL_REUSE_UNSAFE_NO_RESET") {
			t.Fatalf("get does not say unsafe: %s", got.body)
		}
		if got := call(t, http.MethodPost, ts.URL+"/v1/pools/py/run", plain, map[string]any{"command": "true", "timeoutSeconds": "5"}); !strings.Contains(got.body, "POOL_REUSE_UNSAFE_NO_RESET") {
			t.Fatalf("run frame does not say unsafe: %s", got.body)
		}
		// A sandbox key cannot redeclare it idempotently either: the gate is on the request.
		if got := call(t, http.MethodPut, ts.URL+"/v1/pools/py", plain, unsafe()); got.code != http.StatusForbidden {
			t.Fatalf("redeclare by a sandbox key = %d %s, want 403", got.code, got.body)
		}
	})
}

func TestPoolReservationCountsInPlacement(t *testing.T) {
	fr := &fakeRunner{}
	s, ts := newTestServer(t, fr)
	if err := s.store.UpsertRunner(context.Background(), Runner{
		ID: "run_test", Address: "127.0.0.1:9200", Backends: []string{"bwrap"}, NetworkModes: []string{"none"},
		CPUMillis: 1000, MemoryBytes: 10 << 20, MaxSandboxes: 10, LastHeartbeat: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	key := tenantKey(t, s, "tenant-a")

	declare(t, ts, key, "big", poolBody(2, 8))
	want := sandboxResources{cpu: 800, mem: 8 << 20, count: 8}
	if got := committed(s); got != want {
		t.Fatalf("committed = %+v, want %+v: max_size members of the template", got, want)
	}

	// 8 of 10 are spoken for, though no member has started: a plain create still
	// fits, and a pool that needs more than what is left does not.
	if got := call(t, http.MethodPut, ts.URL+"/v1/pools/more", key, poolBody(1, 3)); got.code != http.StatusServiceUnavailable {
		t.Fatalf("declare past capacity = %d %s, want 503", got.code, got.body)
	}
	if _, ok := routeNamed(t, s, "tenant-a", "more"); ok {
		t.Fatal("a pool that could not be placed kept a route")
	}
	if got := call(t, http.MethodPost, ts.URL+"/v1/sandboxes", key, map[string]any{
		"image": "vora/base", "backend": "bwrap", "resources": map[string]any{"cpuMillis": "300", "memoryBytes": "1048576"},
	}); got.code != http.StatusServiceUnavailable {
		t.Fatalf("plain create past the reservation's CPU = %d %s, want 503", got.code, got.body)
	}

	if got := call(t, http.MethodDelete, ts.URL+"/v1/pools/big", key, nil); got.code != http.StatusNoContent {
		t.Fatalf("delete = %d %s", got.code, got.body)
	}
	if got := committed(s); got != (sandboxResources{}) {
		t.Fatalf("committed after delete = %+v, want none", got)
	}
	declare(t, ts, key, "more", poolBody(1, 3))
}

// A pool reserves max_size times its template's resources, so a template without
// them cannot be placed. It must be refused before it reserves anything at zero.
func TestDeclareRefusesATemplateWithNoResources(t *testing.T) {
	fr := &fakeRunner{}
	s, ts := newTestServer(t, fr)
	key := tenantKey(t, s, "tenant-a")
	sandbox := func(resources map[string]any) map[string]any {
		body := poolBody(1, 1)
		body["sandbox"].(map[string]any)["resources"] = resources
		return body
	}
	noSandbox := poolBody(1, 1)
	delete(noSandbox, "sandbox")
	for name, body := range map[string]map[string]any{
		"no sandbox":      noSandbox,
		"no resources":    sandbox(map[string]any{}),
		"no cpu":          sandbox(map[string]any{"memoryBytes": "1048576", "pids": "32"}),
		"no memory":       sandbox(map[string]any{"cpuMillis": "100", "pids": "32"}),
		"negative cpu":    sandbox(map[string]any{"cpuMillis": "-100", "memoryBytes": "1048576", "pids": "32"}),
		"negative memory": sandbox(map[string]any{"cpuMillis": "100", "memoryBytes": "-1", "pids": "32"}),
	} {
		got := call(t, http.MethodPut, ts.URL+"/v1/pools/py", key, body)
		if got.code != http.StatusBadRequest || !strings.Contains(got.body, "resources") {
			t.Errorf("%s: declare = %d %s, want 400 about resources", name, got.code, got.body)
		}
		if _, ok := routeNamed(t, s, "tenant-a", "py"); ok || fr.callCount() != 0 {
			t.Fatalf("%s: left a route %v, made %d runner calls", name, ok, fr.callCount())
		}
	}
	declare(t, ts, key, "py", poolBody(1, 1))
}

// A declare with no sizes reserves what the runner will actually give it.
func TestReservationUsesTheRunnersDefaults(t *testing.T) {
	fr := &fakeRunner{}
	s, ts := newTestServer(t, fr)
	body := poolBody(0, 0)
	delete(body, "size")
	delete(body, "maxSize")
	declare(t, ts, tenantKey(t, s, "tenant-a"), "py", body)
	if got := committed(s); got.count != 8 {
		t.Fatalf("committed = %+v, want 8 members from the default size", got)
	}
}

func TestPoolCreateFailurePaths(t *testing.T) {
	t.Run("a refused create frees its reservation", func(t *testing.T) {
		for _, err := range []error{
			status.Error(codes.InvalidArgument, "pool: invalid spec"),
			status.Error(codes.ResourceExhausted, "sandbox capacity reached"),
			status.Error(codes.FailedPrecondition, "pool: backend cannot reset sandboxes"),
		} {
			fr := &fakeRunner{}
			fr.poolCreateErr = err
			s, ts := newTestServer(t, fr)
			got := call(t, http.MethodPut, ts.URL+"/v1/pools/py", tenantKey(t, s, "tenant-a"), poolBody(1, 2))
			if got.code < 400 || got.code >= 500 {
				t.Fatalf("%v: status %d %s, want a 4xx", err, got.code, got.body)
			}
			if _, ok := routeNamed(t, s, "tenant-a", "py"); ok || committed(s).count != 0 {
				t.Fatalf("%v kept a route, committed %+v", err, committed(s))
			}
		}
	})
	t.Run("a create whose answer was lost keeps its route, and a repeat converges", func(t *testing.T) {
		fr := &fakeRunner{}
		fr.createdThenErr = status.Error(codes.Unavailable, "connection reset")
		s, ts := newTestServer(t, fr)
		key := tenantKey(t, s, "tenant-a")
		got := call(t, http.MethodPut, ts.URL+"/v1/pools/py", key, poolBody(1, 2))
		if got.code != http.StatusServiceUnavailable {
			t.Fatalf("declare = %d %s, want 503", got.code, got.body)
		}
		route, ok := routeNamed(t, s, "tenant-a", "py")
		if !ok || committed(s).count != 2 {
			t.Fatalf("the pool on the runner has no route: committed %+v", committed(s))
		}
		fr.mu.Lock()
		fr.createdThenErr = nil
		fr.mu.Unlock()
		again := call(t, http.MethodPut, ts.URL+"/v1/pools/py", key, poolBody(1, 2))
		if again.code != http.StatusOK || decodePool(t, again.body).GetId() != route.PoolID {
			t.Fatalf("repeat = %d %s, want 200 and pool %s", again.code, again.body, route.PoolID)
		}
		if len(fr.pools) != 1 {
			t.Fatalf("%d pools on the runner", len(fr.pools))
		}
	})
	t.Run("a create whose answer was lost can be deleted without a repeat", func(t *testing.T) {
		fr := &fakeRunner{}
		fr.createdThenErr = status.Error(codes.DeadlineExceeded, "timed out")
		s, ts := newTestServer(t, fr)
		key := tenantKey(t, s, "tenant-a")
		if got := call(t, http.MethodPut, ts.URL+"/v1/pools/py", key, poolBody(1, 2)); got.code != http.StatusGatewayTimeout {
			t.Fatalf("declare = %d %s, want 504", got.code, got.body)
		}
		if got := call(t, http.MethodDelete, ts.URL+"/v1/pools/py", key, nil); got.code != http.StatusNoContent {
			t.Fatalf("delete = %d %s", got.code, got.body)
		}
		if len(fr.pools) != 0 || committed(s).count != 0 {
			t.Fatalf("%d pools on the runner, committed %+v", len(fr.pools), committed(s))
		}
	})
	t.Run("a client leaving mid-declare still ends with a route", func(t *testing.T) {
		fr := &fakeRunner{}
		s, ts := newTestServer(t, fr)
		key := tenantKey(t, s, "tenant-a")
		leaving := &leavingStore{Store: s.store, leave: make(chan struct{})}
		s.store = leaving
		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			<-leaving.leave
			cancel()
		}()
		data, _ := json.Marshal(poolBody(1, 2))
		req, _ := http.NewRequestWithContext(ctx, http.MethodPut, ts.URL+"/v1/pools/py", strings.NewReader(string(data)))
		req.Header.Set("Authorization", "Bearer "+key)
		if resp, err := http.DefaultClient.Do(req); err == nil {
			resp.Body.Close()
		}
		deadline := time.Now().Add(5 * time.Second)
		for {
			route, ok := routeNamed(t, s, "tenant-a", "py")
			if ok && fr.pool(route.PoolID) != nil {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("route %+v, %v; the runner has %d pools", route, ok, fr.poolCount())
			}
			time.Sleep(5 * time.Millisecond)
		}
	})
	t.Run("a failed insert asks the runner for nothing", func(t *testing.T) {
		fr := &fakeRunner{}
		s, ts := newTestServer(t, fr)
		key := tenantKey(t, s, "tenant-a")
		s.store = &failingPoolRouteStore{Store: s.store, insertErr: errors.New("db down")}
		if got := call(t, http.MethodPut, ts.URL+"/v1/pools/py", key, poolBody(1, 2)); got.code != http.StatusInternalServerError {
			t.Fatalf("declare = %d %s, want 500", got.code, got.body)
		}
		if fr.callCount() != 0 {
			t.Fatalf("%d runner calls with no route written", fr.callCount())
		}
	})
	t.Run("a failed final write keeps the route, and a repeat converges", func(t *testing.T) {
		fr := &fakeRunner{}
		s, ts := newTestServer(t, fr)
		key := tenantKey(t, s, "tenant-a")
		failing := &failingPoolRouteStore{Store: s.store, setErr: errors.New("db down")}
		s.store = failing
		if got := call(t, http.MethodPut, ts.URL+"/v1/pools/py", key, poolBody(1, 2)); got.code != http.StatusInternalServerError {
			t.Fatalf("declare = %d %s, want 500", got.code, got.body)
		}
		route, ok := routeNamed(t, s, "tenant-a", "py")
		if !ok || fr.pool(route.PoolID) == nil {
			t.Fatalf("route %+v, %v, with %d pools on the runner", route, ok, len(fr.pools))
		}
		failing.setErr = nil
		if got := call(t, http.MethodPut, ts.URL+"/v1/pools/py", key, poolBody(1, 2)); got.code != http.StatusOK {
			t.Fatalf("repeat = %d %s", got.code, got.body)
		}
	})
	t.Run("a route whose runner lost the pool is declared again under a new id", func(t *testing.T) {
		fr := &fakeRunner{}
		s, ts := newTestServer(t, fr)
		key := tenantKey(t, s, "tenant-a")
		first := declare(t, ts, key, "py", poolBody(1, 2))
		delete(fr.pools, first.GetId())
		got := call(t, http.MethodPut, ts.URL+"/v1/pools/py", key, poolBody(1, 2))
		if got.code != http.StatusOK {
			t.Fatalf("declare = %d %s", got.code, got.body)
		}
		id := decodePool(t, got.body).GetId()
		route, _ := routeNamed(t, s, "tenant-a", "py")
		if id == first.GetId() || route.PoolID != id {
			t.Fatalf("new pool %s, route %s, old id %s", id, route.PoolID, first.GetId())
		}
	})
}

// A delete that began against the old pool must not remove the route of the one
// declared after it.
func TestDeleteRouteOnlyRemovesThePoolItNames(t *testing.T) {
	store := NewMemStore()
	ctx := context.Background()
	if err := store.InsertPoolRoute(ctx, PoolRoute{TenantID: "t", Name: "py", PoolID: "pool_new"}); err != nil {
		t.Fatal(err)
	}
	if err := store.DeletePoolRoute(ctx, "t", "py", "pool_old"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PoolRoute(ctx, "t", "py"); err != nil {
		t.Fatalf("a delete of the old pool removed the new route: %v", err)
	}
	if err := store.InsertPoolRoute(ctx, PoolRoute{TenantID: "t", Name: "py"}); !errors.Is(err, ErrPoolRouteExists) {
		t.Fatalf("insert over a route = %v, want ErrPoolRouteExists", err)
	}
}

type failingPoolRouteStore struct {
	Store
	insertErr, setErr error
}

func (f *failingPoolRouteStore) InsertPoolRoute(ctx context.Context, route PoolRoute) error {
	if f.insertErr != nil {
		return f.insertErr
	}
	return f.Store.InsertPoolRoute(ctx, route)
}

func (f *failingPoolRouteStore) SetPoolRoute(ctx context.Context, route PoolRoute, expectPoolID string) error {
	if f.setErr != nil {
		return f.setErr
	}
	return f.Store.SetPoolRoute(ctx, route, expectPoolID)
}

// leavingStore signals when the insert is done, which is when a client that leaves
// finds the declare mid-way.
type leavingStore struct {
	Store
	leave chan struct{}
	once  sync.Once
}

func (l *leavingStore) InsertPoolRoute(ctx context.Context, route PoolRoute) error {
	err := l.Store.InsertPoolRoute(ctx, route)
	l.once.Do(func() { close(l.leave) })
	time.Sleep(50 * time.Millisecond)
	return err
}

func TestPoolDeleteKeepsItsRouteUntilTheRunnerSaysGone(t *testing.T) {
	setup := func(t *testing.T) (*fakeRunner, *Server, *httptest.Server, string, *v1.Pool) {
		fr := &fakeRunner{}
		s, ts := newTestServer(t, fr)
		key := tenantKey(t, s, "tenant-a")
		return fr, s, ts, key, declare(t, ts, key, "py", poolBody(1, 2))
	}
	kept := func(t *testing.T, s *Server, p *v1.Pool) {
		t.Helper()
		route, ok := routeNamed(t, s, "tenant-a", "py")
		if !ok || route.PoolID != p.GetId() || committed(s).count != 2 {
			t.Fatalf("route %+v, %v, committed %+v: want the route and its reservation kept", route, ok, committed(s))
		}
	}
	gone := func(t *testing.T, s *Server) {
		t.Helper()
		if _, ok := routeNamed(t, s, "tenant-a", "py"); ok || committed(s).count != 0 {
			t.Fatalf("committed %+v: want the route and its reservation gone", committed(s))
		}
	}

	t.Run("busy members answer 202 until drained", func(t *testing.T) {
		fr, s, ts, key, p := setup(t)
		fr.deleteBusy = true
		if got := call(t, http.MethodDelete, ts.URL+"/v1/pools/py", key, nil); got.code != http.StatusAccepted {
			t.Fatalf("delete = %d %s, want 202", got.code, got.body)
		}
		kept(t, s, p)
		fr.deleteBusy = false
		if got := call(t, http.MethodDelete, ts.URL+"/v1/pools/py", key, nil); got.code != http.StatusNoContent {
			t.Fatalf("repeat = %d %s, want 204", got.code, got.body)
		}
		gone(t, s)
		if got := call(t, http.MethodDelete, ts.URL+"/v1/pools/py", key, nil); got.code != http.StatusNotFound {
			t.Fatalf("delete of a deleted pool = %d %s, want 404", got.code, got.body)
		}
	})
	t.Run("an unreachable runner keeps the route", func(t *testing.T) {
		fr, s, ts, key, p := setup(t)
		fr.deleteErr = status.Error(codes.Unavailable, "dial tcp 10.0.0.5:9200: connection refused")
		got := call(t, http.MethodDelete, ts.URL+"/v1/pools/py", key, nil)
		if got.code != http.StatusServiceUnavailable || strings.Contains(got.body, "10.0.0.5") {
			t.Fatalf("delete = %d %s, want 503 without the runner's address", got.code, got.body)
		}
		kept(t, s, p)
		fr.deleteErr = nil
		if got := call(t, http.MethodDelete, ts.URL+"/v1/pools/py", key, nil); got.code != http.StatusNoContent {
			t.Fatalf("after the runner came back = %d %s, want 204", got.code, got.body)
		}
		gone(t, s)
	})
	t.Run("a runner past its heartbeat keeps the route", func(t *testing.T) {
		fr, s, ts, key, p := setup(t)
		before := fr.callCount()
		stale, err := s.store.Runner(context.Background(), "run_test")
		if err != nil {
			t.Fatal(err)
		}
		stale.LastHeartbeat = time.Now().Add(-time.Hour)
		if err := s.store.UpsertRunner(context.Background(), stale); err != nil {
			t.Fatal(err)
		}
		for _, route := range poolRoutes("py") {
			if got := call(t, route.method, ts.URL+route.path, key, route.body); got.code != http.StatusServiceUnavailable {
				t.Errorf("%s %s = %d %s, want 503", route.method, route.path, got.code, got.body)
			}
		}
		if fr.callCount() != before {
			t.Fatalf("%d calls to a runner past its heartbeat", fr.callCount()-before)
		}
		kept(t, s, p)
		list := call(t, http.MethodGet, ts.URL+"/v1/pools", key, nil)
		if list.code != http.StatusOK || !strings.Contains(list.body, p.GetId()) || strings.Contains(list.body, "POOL_ACTIVE") {
			t.Fatalf("listing with the runner down = %d %s, want the pool with no state", list.code, list.body)
		}
	})
	t.Run("a runner that no longer has the pool frees the route", func(t *testing.T) {
		fr, s, ts, key, p := setup(t)
		delete(fr.pools, p.GetId())
		if got := call(t, http.MethodDelete, ts.URL+"/v1/pools/py", key, nil); got.code != http.StatusNoContent {
			t.Fatalf("delete = %d %s, want 204", got.code, got.body)
		}
		gone(t, s)
	})
	// Only a row removed by hand gets here, and the runner may still hold the pool and
	// register again, so dropping the route would orphan it.
	t.Run("a runner with no record left keeps the route", func(t *testing.T) {
		_, s, ts, key, p := setup(t)
		mem := s.store.(*MemStore)
		mem.mu.Lock()
		delete(mem.runners, "run_test")
		mem.mu.Unlock()
		for _, method := range []string{http.MethodGet, http.MethodDelete} {
			if got := call(t, method, ts.URL+"/v1/pools/py", key, nil); got.code != http.StatusServiceUnavailable {
				t.Fatalf("%s = %d %s, want 503", method, got.code, got.body)
			}
		}
		if route, ok := routeNamed(t, s, "tenant-a", "py"); !ok || route.PoolID != p.GetId() {
			t.Fatalf("route = %+v, %v, want it kept", route, ok)
		}
	})
	t.Run("a failed route removal repeats to the same end", func(t *testing.T) {
		fr, s, ts, key, p := setup(t)
		mem := s.store
		s.store = &failingDeleteStore{Store: mem}
		if got := call(t, http.MethodDelete, ts.URL+"/v1/pools/py", key, nil); got.code != http.StatusInternalServerError {
			t.Fatalf("delete = %d %s, want 500", got.code, got.body)
		}
		if fr.pool(p.GetId()) != nil {
			t.Fatal("the runner should have drained the pool")
		}
		s.store = mem
		if got := call(t, http.MethodDelete, ts.URL+"/v1/pools/py", key, nil); got.code != http.StatusNoContent {
			t.Fatalf("repeat = %d %s, want 204", got.code, got.body)
		}
		gone(t, s)
	})
	t.Run("no read drops a route", func(t *testing.T) {
		fr, s, ts, key, p := setup(t)
		for _, err := range []error{
			status.Error(codes.Unavailable, "pools are not loaded yet"),
			status.Error(codes.Internal, "boom"),
			status.Error(codes.DeadlineExceeded, "slow"),
			status.Error(codes.NotFound, "pool not found"),
		} {
			fr.getErr = err
			call(t, http.MethodGet, ts.URL+"/v1/pools/py", key, nil)
			call(t, http.MethodGet, ts.URL+"/v1/pools", key, nil)
			kept(t, s, p)
		}
	})
}

// Whether a pool resets its members is what a tenant most needs to know of it, so a
// runner being down or having lost the pool must not hide it.
func TestRouteSaysWhetherThePoolResets(t *testing.T) {
	unsafe := poolBody(1, 1)
	unsafe["reuse"] = "POOL_REUSE_UNSAFE_NO_RESET"
	setup := func(t *testing.T) (*fakeRunner, *Server, *httptest.Server, string, *v1.Pool) {
		t.Helper()
		fr := &fakeRunner{}
		s, ts := newTestServer(t, fr)
		key := mintKey(t, s, "tenant-a", "", ScopeAdmin)
		return fr, s, ts, key, declare(t, ts, key, "py", unsafe)
	}
	t.Run("a runner that does not answer", func(t *testing.T) {
		fr, _, ts, key, _ := setup(t)
		fr.getErr = status.Error(codes.Unavailable, "down")
		list := call(t, http.MethodGet, ts.URL+"/v1/pools", key, nil)
		if list.code != http.StatusOK || !strings.Contains(list.body, "POOL_REUSE_UNSAFE_NO_RESET") || strings.Contains(list.body, "POOL_ACTIVE") {
			t.Fatalf("list with the runner down = %d %s, want no state and the reuse", list.code, list.body)
		}
	})
	t.Run("a runner that lost the pool", func(t *testing.T) {
		fr, _, ts, key, p := setup(t)
		delete(fr.pools, p.GetId())
		got := call(t, http.MethodGet, ts.URL+"/v1/pools/py", key, nil)
		if read := decodePool(t, got.body); read.GetState() != v1.PoolState_POOL_NOT_ON_RUNNER || read.GetSpec().GetReuse() != v1.PoolReuse_POOL_REUSE_UNSAFE_NO_RESET {
			t.Fatalf("get of a lost pool = %v", read)
		}
	})
	t.Run("while the create is in flight", func(t *testing.T) {
		fr := &fakeRunner{}
		fr.createdThenErr = status.Error(codes.Unavailable, "connection reset")
		s, ts := newTestServer(t, fr)
		key := mintKey(t, s, "tenant-a", "", ScopeAdmin)
		call(t, http.MethodPut, ts.URL+"/v1/pools/py", key, unsafe)
		if route, ok := routeNamed(t, s, "tenant-a", "py"); !ok || route.Reuse != v1.PoolReuse_POOL_REUSE_UNSAFE_NO_RESET {
			t.Fatalf("route before the runner answered = %+v, %v", route, ok)
		}
	})
	t.Run("a redeclare changes it", func(t *testing.T) {
		fr, s, ts, key, p := setup(t)
		delete(fr.pools, p.GetId())
		reset := poolBody(1, 1)
		reset["reuse"] = "POOL_REUSE_RESET"
		if got := call(t, http.MethodPut, ts.URL+"/v1/pools/py", key, reset); got.code != http.StatusOK {
			t.Fatalf("redeclare = %d %s", got.code, got.body)
		}
		if route, _ := routeNamed(t, s, "tenant-a", "py"); route.Reuse != v1.PoolReuse_POOL_REUSE_RESET {
			t.Fatalf("route after the redeclare = %+v", route)
		}
	})
}

// A listing asks a runner about every pool. A runner that accepts the calls and
// never answers must not hold a tenant's listing past the server's WriteTimeout,
// and one tenant's listing must not open a call per pool at once.
func TestListingIsBoundedInTimeAndConcurrency(t *testing.T) {
	fr := &fakeRunner{}
	s, ts := newTestServer(t, fr)
	key := tenantKey(t, s, "tenant-a")
	names := make([]string, poolListConcurrency*2+1)
	for i := range names {
		names[i] = fmt.Sprintf("py-%02d", i)
		declare(t, ts, key, names[i], poolBody(0, 1))
	}
	s.poolListDeadline = 200 * time.Millisecond
	var inFlight, peak atomic.Int32
	fr.getHook = func(ctx context.Context) {
		now := inFlight.Add(1)
		defer inFlight.Add(-1)
		for {
			seen := peak.Load()
			if now <= seen || peak.CompareAndSwap(seen, now) {
				break
			}
		}
		<-ctx.Done()
	}

	start := time.Now()
	got := call(t, http.MethodGet, ts.URL+"/v1/pools", key, nil)
	took := time.Since(start)
	if got.code != http.StatusOK {
		t.Fatalf("list = %d %s", got.code, got.body)
	}
	// One call's own deadline is 10 s, so this holds only if the listing's cuts it off.
	if took > 5*time.Second {
		t.Fatalf("list took %s against a 200 ms deadline", took)
	}
	if p := peak.Load(); p > poolListConcurrency || p < 2 {
		t.Fatalf("peak concurrent calls = %d, want between 2 and %d", p, poolListConcurrency)
	}
	list := &v1.ListPoolsResponse{}
	if err := protojson.Unmarshal([]byte(got.body), list); err != nil {
		t.Fatal(err)
	}
	if len(list.GetPools()) != len(names) {
		t.Fatalf("listed %d pools, want %d", len(list.GetPools()), len(names))
	}
	for i, p := range list.GetPools() {
		if p == nil || p.GetSpec().GetName() != names[i] || p.GetState() != v1.PoolState_POOL_STATE_UNSPECIFIED || p.GetRunnerId() != "" {
			t.Fatalf("pool %d = %v, want %s with no state and no runner", i, p, names[i])
		}
	}
}

// Asking concurrently must not change what a listing says of pools that do answer.
func TestListingKeepsOrderAndAnswers(t *testing.T) {
	fr := &fakeRunner{}
	s, ts := newTestServer(t, fr)
	key := tenantKey(t, s, "tenant-a")
	names := make([]string, poolListConcurrency+3)
	for i := range names {
		names[i] = fmt.Sprintf("py-%02d", i)
		declare(t, ts, key, names[i], poolBody(0, 1))
	}
	bound := mintKey(t, s, "tenant-a", "agent-2", ScopeSandbox)
	declare(t, ts, bound, "agents", poolBody(0, 1))

	list := &v1.ListPoolsResponse{}
	got := call(t, http.MethodGet, ts.URL+"/v1/pools", bound, nil)
	if err := protojson.Unmarshal([]byte(got.body), list); err != nil || len(list.GetPools()) != 1 || list.GetPools()[0].GetSpec().GetName() != "agents" {
		t.Fatalf("an agent's listing = %d %s, %v, want its one pool", got.code, got.body, err)
	}
	got = call(t, http.MethodGet, ts.URL+"/v1/pools", key, nil)
	if err := protojson.Unmarshal([]byte(got.body), list); err != nil {
		t.Fatal(err)
	}
	want := append([]string{"agents"}, names...)
	if len(list.GetPools()) != len(want) {
		t.Fatalf("listed %d, want %d", len(list.GetPools()), len(want))
	}
	for i, p := range list.GetPools() {
		if p.GetSpec().GetName() != want[i] || p.GetState() != v1.PoolState_POOL_ACTIVE {
			t.Fatalf("pool %d = %v, want %s active", i, p, want[i])
		}
	}
}

// A runner that lost a pool's record leaves a route holding a reservation and a
// name. A tenant must be able to see that, and either way out must converge.
func TestPoolTheRunnerLost(t *testing.T) {
	setup := func(t *testing.T) (*fakeRunner, *Server, *httptest.Server, string, *v1.Pool) {
		t.Helper()
		fr := &fakeRunner{}
		s, ts := newTestServer(t, fr)
		key := tenantKey(t, s, "tenant-a")
		p := declare(t, ts, key, "py", poolBody(1, 2))
		delete(fr.pools, p.GetId())
		return fr, s, ts, key, p
	}
	t.Run("a read says so and holds on to the reservation", func(t *testing.T) {
		_, s, ts, key, p := setup(t)
		got := call(t, http.MethodGet, ts.URL+"/v1/pools/py", key, nil)
		if got.code != http.StatusOK {
			t.Fatalf("get = %d %s, want 200", got.code, got.body)
		}
		read := decodePool(t, got.body)
		if read.GetState() != v1.PoolState_POOL_NOT_ON_RUNNER || read.GetId() != p.GetId() || read.GetSpec().GetName() != "py" || read.GetRunnerId() != "" {
			t.Fatalf("get = %v, want POOL_NOT_ON_RUNNER, this pool and no runner", read)
		}
		list := call(t, http.MethodGet, ts.URL+"/v1/pools", key, nil)
		if list.code != http.StatusOK || !strings.Contains(list.body, p.GetId()) || !strings.Contains(list.body, "POOL_NOT_ON_RUNNER") {
			t.Fatalf("list = %d %s, want the pool as POOL_NOT_ON_RUNNER", list.code, list.body)
		}
		if route, ok := routeNamed(t, s, "tenant-a", "py"); !ok || route.PoolID != p.GetId() || committed(s).count != 2 {
			t.Fatalf("route %+v, %v, committed %+v: want both kept", route, ok, committed(s))
		}
	})
	t.Run("a stranger still sees nothing", func(t *testing.T) {
		_, s, ts, _, p := setup(t)
		stranger := tenantKey(t, s, "tenant-b")
		if got := call(t, http.MethodGet, ts.URL+"/v1/pools/py", stranger, nil); got.code != http.StatusNotFound || strings.Contains(got.body, p.GetId()) {
			t.Fatalf("get by a stranger = %d %s, want 404", got.code, got.body)
		}
	})
	t.Run("a run says what to do", func(t *testing.T) {
		_, s, ts, key, p := setup(t)
		got := call(t, http.MethodPost, ts.URL+"/v1/pools/py/run", key, map[string]any{"command": "true", "timeoutSeconds": "5"})
		if got.code != http.StatusConflict || !strings.Contains(got.body, "declare it again") {
			t.Fatalf("run = %d %s, want 409 saying to declare it again", got.code, got.body)
		}
		if route, ok := routeNamed(t, s, "tenant-a", "py"); !ok || route.PoolID != p.GetId() {
			t.Fatalf("route = %+v, %v after a run", route, ok)
		}
	})
	t.Run("a declare brings it back", func(t *testing.T) {
		fr, s, ts, key, p := setup(t)
		got := call(t, http.MethodPut, ts.URL+"/v1/pools/py", key, poolBody(1, 2))
		if got.code != http.StatusOK {
			t.Fatalf("declare = %d %s, want 200", got.code, got.body)
		}
		id := decodePool(t, got.body).GetId()
		if route, ok := routeNamed(t, s, "tenant-a", "py"); id == p.GetId() || !ok || route.PoolID != id || fr.pool(id) == nil {
			t.Fatalf("new pool %s, old %s, route %+v, %v", id, p.GetId(), route, ok)
		}
		if read := call(t, http.MethodGet, ts.URL+"/v1/pools/py", key, nil); decodePool(t, read.body).GetState() != v1.PoolState_POOL_ACTIVE {
			t.Fatalf("get after declare = %d %s", read.code, read.body)
		}
	})
	t.Run("a delete releases it", func(t *testing.T) {
		_, s, ts, key, _ := setup(t)
		if got := call(t, http.MethodDelete, ts.URL+"/v1/pools/py", key, nil); got.code != http.StatusNoContent {
			t.Fatalf("delete = %d %s, want 204", got.code, got.body)
		}
		if _, ok := routeNamed(t, s, "tenant-a", "py"); ok || committed(s).count != 0 {
			t.Fatalf("committed %+v: want the route and its reservation gone", committed(s))
		}
		if got := call(t, http.MethodGet, ts.URL+"/v1/pools/py", key, nil); got.code != http.StatusNotFound {
			t.Fatalf("get after delete = %d %s, want 404", got.code, got.body)
		}
	})
}

type failingDeleteStore struct{ Store }

func (failingDeleteStore) DeletePoolRoute(context.Context, string, string, string) error {
	return errors.New("db down")
}

// A pool the runner refused to load at boot reads as a state, not a failure. It
// must keep its route for the boot that can honour it, and be deletable.
func TestNotLoadedPool(t *testing.T) {
	fr := &fakeRunner{}
	s, ts := newTestServer(t, fr)
	key := tenantKey(t, s, "tenant-a")
	p := declare(t, ts, key, "py", poolBody(1, 2))
	fr.mu.Lock()
	fr.pools[p.GetId()] = &v1.Pool{
		Id: p.GetId(), State: v1.PoolState_POOL_NOT_LOADED, NotLoadedReason: "backend_missing",
		Spec: &v1.PoolSpec{Name: "py", TenantId: "tenant-a"}, Stats: &v1.PoolStats{},
	}
	fr.mu.Unlock()

	got := call(t, http.MethodGet, ts.URL+"/v1/pools/py", key, nil)
	if got.code != http.StatusOK || decodePool(t, got.body).GetState() != v1.PoolState_POOL_NOT_LOADED || decodePool(t, got.body).GetNotLoadedReason() != "backend_missing" {
		t.Fatalf("get = %d %s, want 200 POOL_NOT_LOADED", got.code, got.body)
	}
	if route, ok := routeNamed(t, s, "tenant-a", "py"); !ok || route.MaxSize != 2 {
		t.Fatalf("route = %+v, %v: reading a not-loaded spec must not rewrite the reservation", route, ok)
	}
	for _, c := range []struct {
		method, path string
		body         any
	}{
		{http.MethodPost, "/v1/pools/py/run", map[string]any{"command": "true", "timeoutSeconds": "5"}},
		{http.MethodPut, "/v1/pools/py", poolBody(1, 2)},
	} {
		got := call(t, c.method, ts.URL+c.path, key, c.body)
		if got.code != http.StatusConflict || !strings.Contains(got.body, "not loaded") {
			t.Fatalf("%s %s = %d %s, want 409 saying not loaded", c.method, c.path, got.code, got.body)
		}
		if _, ok := routeNamed(t, s, "tenant-a", "py"); !ok {
			t.Fatalf("%s %s dropped the route", c.method, c.path)
		}
	}
	if got := call(t, http.MethodDelete, ts.URL+"/v1/pools/py", key, nil); got.code != http.StatusNoContent {
		t.Fatalf("delete = %d %s, want 204", got.code, got.body)
	}
	if _, ok := routeNamed(t, s, "tenant-a", "py"); ok {
		t.Fatal("delete kept the route")
	}
}

func TestRunStatusMapping(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		want       int
		retryAfter bool
		mustSay    string
		mustNotSay string
	}{
		{"pool full", status.Error(codes.ResourceExhausted, "pool: no sandbox free within the borrow timeout"), http.StatusTooManyRequests, true, "no sandbox free", ""},
		{"draining", status.Error(codes.FailedPrecondition, "pool: draining"), http.StatusConflict, false, "draining", ""},
		{"runner loading", status.Error(codes.Unavailable, "pools are not loaded yet"), http.StatusServiceUnavailable, true, "still starting", ""},
		{"runner gone", status.Error(codes.Unavailable, `connection error: desc = "transport: dial tcp 10.0.0.5:9200: connect: connection refused"`), http.StatusServiceUnavailable, true, "retry", "10.0.0.5"},
		{"bad request", status.Error(codes.InvalidArgument, "timeout_seconds is required"), http.StatusBadRequest, false, "timeout_seconds", ""},
		{"no such pool", status.Error(codes.NotFound, "pool pool_x not found"), http.StatusNotFound, false, "pool not found", "pool_x"},
		{"runner too slow", status.Error(codes.DeadlineExceeded, "context deadline exceeded"), http.StatusGatewayTimeout, true, "retry", ""},
		{"internal", status.Error(codes.Internal, "open /var/lib/vora/x: permission denied"), http.StatusInternalServerError, false, "internal server error", "/var/lib"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fr := &fakeRunner{}
			s, ts := newTestServer(t, fr)
			key := tenantKey(t, s, "tenant-a")
			declare(t, ts, key, "py", poolBody(1, 1))
			fr.runErr = c.err
			resp := do(t, http.MethodPost, ts.URL+"/v1/pools/py/run", key, map[string]any{"command": "true", "timeoutSeconds": "5"})
			defer resp.Body.Close()
			data, _ := io.ReadAll(resp.Body)
			body := string(data)
			if resp.StatusCode != c.want {
				t.Fatalf("status = %d %s, want %d", resp.StatusCode, body, c.want)
			}
			if got := resp.Header.Get("Retry-After") != ""; got != c.retryAfter {
				t.Fatalf("Retry-After present = %v, want %v", got, c.retryAfter)
			}
			if !strings.Contains(body, c.mustSay) {
				t.Fatalf("body %q does not say %q", body, c.mustSay)
			}
			if c.mustNotSay != "" && strings.Contains(body, c.mustNotSay) {
				t.Fatalf("body %q leaks %q", body, c.mustNotSay)
			}
		})
	}
}

// A NotFound out of a run may come from inside it. Only the pool's own absence is
// answered as a lost pool, and neither drops the route.
func TestRunNotFoundIsALostPoolOnlyIfThePoolIsGone(t *testing.T) {
	fr := &fakeRunner{}
	s, ts := newTestServer(t, fr)
	key := tenantKey(t, s, "tenant-a")
	p := declare(t, ts, key, "py", poolBody(1, 1))
	run := map[string]any{"command": "true", "timeoutSeconds": "5"}

	fr.runErr = status.Error(codes.NotFound, "sandbox not found")
	if got := call(t, http.MethodPost, ts.URL+"/v1/pools/py/run", key, run); got.code != http.StatusNotFound {
		t.Fatalf("run = %d %s", got.code, got.body)
	}
	if _, ok := routeNamed(t, s, "tenant-a", "py"); !ok {
		t.Fatal("a NotFound from inside a run dropped a live pool's route")
	}

	fr.runErr = nil
	delete(fr.pools, p.GetId())
	if got := call(t, http.MethodPost, ts.URL+"/v1/pools/py/run", key, run); got.code != http.StatusConflict {
		t.Fatalf("run = %d %s, want 409", got.code, got.body)
	}
	if _, ok := routeNamed(t, s, "tenant-a", "py"); !ok {
		t.Fatal("a run on a pool the runner lost dropped the route")
	}
}

// During a runner's startup, before LoadPools, plain creates and forks answer
// Unavailable. The client must get something it can act on, not a bare 500 or the
// runner's address.
func TestRunnerStartingWindowIsExplained(t *testing.T) {
	loading := status.Error(codes.Unavailable, "the runner is still starting")
	for _, c := range []struct {
		name string
		err  error
		code int
		say  string
	}{
		{"still starting", loading, http.StatusServiceUnavailable, "still starting"},
		{"capacity", status.Error(codes.ResourceExhausted, "sandbox capacity reached"), http.StatusTooManyRequests, "capacity"},
		{"state", status.Error(codes.FailedPrecondition, "sandbox sbx_x is in state STOPPED"), http.StatusConflict, "STOPPED"},
		{"unreachable", status.Error(codes.Unavailable, `connection error: dial tcp 10.0.0.5:9200: connection refused`), http.StatusServiceUnavailable, "retry"},
	} {
		t.Run(c.name, func(t *testing.T) {
			fr := &fakeRunner{createErr: c.err}
			_, ts := newTestServer(t, fr)
			resp := do(t, http.MethodPost, ts.URL+"/v1/sandboxes", bootstrap, map[string]any{"image": "vora/base", "backend": "bwrap"})
			defer resp.Body.Close()
			data, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != c.code || !strings.Contains(string(data), c.say) {
				t.Fatalf("create = %d %s, want %d saying %q", resp.StatusCode, data, c.code, c.say)
			}
			if c.code != http.StatusConflict && resp.Header.Get("Retry-After") == "" {
				t.Fatal("a retryable answer has no Retry-After")
			}
			if strings.Contains(string(data), "10.0.0.5") {
				t.Fatalf("the runner's address reached a client: %s", data)
			}
		})
	}
}

func TestPlainRoutesRefuseAMember(t *testing.T) {
	fr := &fakeRunner{}
	s, ts := newTestServer(t, fr)
	owner := tenantKey(t, s, "tenant-a")
	p := declare(t, ts, owner, "py", poolBody(1, 1))
	fr.members = map[string]*v1.Sandbox{memberID: {Id: memberID, PoolId: p.GetId(), Spec: &v1.SandboxSpec{TenantId: "tenant-a"}}}
	fr.sandboxes = []*v1.Sandbox{fr.members[memberID]}

	base := "/v1/sandboxes/"
	routes := func(id string) []struct {
		method, path string
		body         any
	} {
		return []struct {
			method, path string
			body         any
		}{
			{http.MethodGet, base + id, nil},
			{http.MethodDelete, base + id, nil},
			{http.MethodPost, base + id + "/exec", map[string]any{"command": "true"}},
			{http.MethodGet, base + id + "/stats", nil},
			{http.MethodGet, base + id + "/events", nil},
			{http.MethodGet, base + id + "/execs", nil},
			{http.MethodGet, base + id + "/egress", nil},
			{http.MethodGet, base + id + "/files?path=/tmp/x", nil},
			{http.MethodPut, base + id + "/files?path=/tmp/x", map[string]any{}},
			{http.MethodPost, base + id + "/snapshot", nil},
		}
	}
	for _, route := range routes(memberID) {
		got := call(t, route.method, ts.URL+route.path, owner, route.body)
		if got.code != http.StatusConflict || !strings.Contains(got.body, "use the pool API") {
			t.Errorf("%s %s = %d %s, want 409 pointing at the pool API", route.method, route.path, got.code, got.body)
		}
	}
	if len(fr.destroyed) != 0 || len(fr.snapshotted) != 0 {
		t.Fatalf("a member was acted on: destroyed %v, snapshotted %v", fr.destroyed, fr.snapshotted)
	}

	// Listing shows the member with its pool, so the client can see why.
	list := call(t, http.MethodGet, ts.URL+"/v1/sandboxes", owner, nil)
	if list.code != http.StatusOK || !strings.Contains(list.body, p.GetId()) {
		t.Fatalf("listing = %d %s, want the member's pool", list.code, list.body)
	}

	// Another tenant holding a pool on the same runner can make the API look, and
	// must not learn the member exists. Nor may another agent of the tenant.
	stranger := tenantKey(t, s, "tenant-b")
	declare(t, ts, stranger, "theirs", poolBody(1, 1))
	otherAgent := mintKey(t, s, "tenant-a", "agent-2", ScopeSandbox)
	declare(t, ts, otherAgent, "agents", poolBody(1, 1))
	const nothing = "sbx_01JYYYYYYYYYYYYYYYYYYYYYYY"
	for i, route := range routes(memberID) {
		none := routes(nothing)[i]
		want := call(t, none.method, ts.URL+none.path, stranger, none.body)
		if want.code != http.StatusNotFound {
			t.Fatalf("%s %s on nothing = %d %s", none.method, none.path, want.code, want.body)
		}
		for name, key := range map[string]string{"another tenant": stranger, "another agent": otherAgent} {
			if got := call(t, route.method, ts.URL+route.path, key, route.body); got != want {
				t.Errorf("%s %s by %s = %v, but on no sandbox %v", route.method, route.path, name, got, want)
			}
		}
	}
}

// The refusal must cost a tenant with no pools nothing, and must not forward what
// is not a sandbox id to the runner's record store.
func TestMemberLookupAsksOnlyWhenItCanMatter(t *testing.T) {
	fr := &fakeRunner{}
	s, ts := newTestServer(t, fr)
	plain := tenantKey(t, s, "tenant-a")
	if got := call(t, http.MethodGet, ts.URL+"/v1/sandboxes/"+memberID, plain, nil); got.code != http.StatusNotFound {
		t.Fatalf("get = %d %s", got.code, got.body)
	}
	if fr.getBoxes != 0 {
		t.Fatalf("a tenant with no pools caused %d runner lookups", fr.getBoxes)
	}
	declare(t, ts, plain, "py", poolBody(1, 1))
	for _, bad := range []string{"not-an-id", "..%2F..%2Fetc", "pool_01JZZZZZZZZZZZZZZZZZZZZZZZ"} {
		if got := call(t, http.MethodGet, ts.URL+"/v1/sandboxes/"+bad, plain, nil); got.code != http.StatusNotFound {
			t.Fatalf("get %s = %d %s", bad, got.code, got.body)
		}
	}
	if fr.getBoxes != 0 {
		t.Fatalf("%d lookups for ids that are not sandbox ids", fr.getBoxes)
	}
}

func TestMemberTrail(t *testing.T) {
	fr := &fakeRunner{}
	s, ts := newTestServer(t, fr)
	key := tenantKey(t, s, "tenant-a")
	p := declare(t, ts, key, "py", poolBody(1, 1))
	other := declare(t, ts, key, "other", poolBody(1, 1))
	secretPath := "/var/lib/vora/sandboxes/sbx_x/control.sock"
	fr.members = map[string]*v1.Sandbox{memberID: {Id: memberID, PoolId: p.GetId(), Spec: &v1.SandboxSpec{TenantId: "tenant-a"}}}
	fr.events = []*v1.SandboxEvent{
		{Reason: "", To: "RUNNING", RunnerId: "run_test"},
		{Reason: "reset: ok: killed 2, removed 3 entries, in 2ms", RunnerId: "run_test"},
		{Reason: "reset: failed: dial unix " + secretPath + ": connection refused", RunnerId: "run_test"},
		{Reason: "destroy: failed: remove " + secretPath + ": busy", RunnerId: "run_test"},
		{Reason: "alive: ping " + secretPath + ": timeout", RunnerId: "run_test"},
		{Reason: "retire: reset_failed", RunnerId: "run_test"},
		{Reason: "RUNTIME_FAILURE", RunnerId: "run_test"},
		{Reason: "something_new: " + secretPath, RunnerId: "run_test"},
	}

	got := call(t, http.MethodGet, ts.URL+"/v1/pools/py/sandboxes/"+memberID+"/execs", key, nil)
	if got.code != http.StatusOK || !strings.Contains(got.body, "exe_member") || fr.execs[0].GetPoolId() != p.GetId() {
		t.Fatalf("execs = %d %s, GetSandboxExecs %v", got.code, got.body, fr.execs)
	}

	got = call(t, http.MethodGet, ts.URL+"/v1/pools/py/sandboxes/"+memberID+"/events", key, nil)
	if got.code != http.StatusOK {
		t.Fatalf("events = %d %s", got.code, got.body)
	}
	if strings.Contains(got.body, "/var/lib") || strings.Contains(got.body, "run_test") {
		t.Fatalf("a tenant sees backend text or the runner: %s", got.body)
	}
	events := &v1.GetSandboxEventsResponse{}
	if err := protojson.Unmarshal([]byte(got.body), events); err != nil {
		t.Fatal(err)
	}
	want := []string{"", "reset: ok: killed 2, removed 3 entries, in 2ms", "reset: failed", "destroy: failed", "alive: failed", "retire: reset_failed", "RUNTIME_FAILURE", "something_new"}
	for i, e := range events.GetEvents() {
		if e.GetReason() != want[i] {
			t.Errorf("event %d reason = %q, want %q", i, e.GetReason(), want[i])
		}
	}
	admin := call(t, http.MethodGet, ts.URL+"/v1/pools/py/sandboxes/"+memberID+"/events?tenant_id=tenant-a", bootstrap, nil)
	if !strings.Contains(admin.body, secretPath) {
		t.Fatalf("the operator lost the backend's words: %s", admin.body)
	}

	// A member read through a pool that is not its own is no member at all.
	for _, tail := range []string{"/execs", "/events"} {
		if got := call(t, http.MethodGet, ts.URL+"/v1/pools/other/sandboxes/"+memberID+tail, key, nil); got.code != http.StatusNotFound {
			t.Fatalf("%s through pool %s = %d %s", tail, other.GetId(), got.code, got.body)
		}
	}
}

func TestPoolRoutesCapRequestBodies(t *testing.T) {
	fr := &fakeRunner{}
	s, ts := newTestServer(t, fr)
	key := tenantKey(t, s, "tenant-a")
	declare(t, ts, key, "py", poolBody(1, 1))
	before := fr.callCount()
	huge := strings.Repeat("x", 1<<20+1)
	for _, c := range []struct {
		method, path string
		body         any
	}{
		{http.MethodPost, "/v1/pools/py/run", map[string]any{"command": "true", "timeoutSeconds": "5", "stdin": huge}},
		{http.MethodPut, "/v1/pools/big", map[string]any{"name": huge}},
	} {
		if got := call(t, c.method, ts.URL+c.path, key, c.body); got.code != http.StatusBadRequest {
			t.Fatalf("%s %s with an oversized body = %d, want 400", c.method, c.path, got.code)
		}
	}
	if fr.callCount() != before {
		t.Fatal("an oversized body reached the runner")
	}
}

func TestPoolRoutesRequireAKey(t *testing.T) {
	_, ts := newTestServer(t, &fakeRunner{})
	for _, route := range append(poolRoutes("py"), struct {
		method, path string
		body         any
	}{http.MethodPut, "/v1/pools/py", poolBody(1, 1)}) {
		if got := call(t, route.method, ts.URL+route.path, "", route.body); got.code != http.StatusUnauthorized {
			t.Errorf("%s %s with no key = %d, want 401", route.method, route.path, got.code)
		}
	}
}

// The reused connection is the point of the pool routes: thousands of runs per
// minute must not each dial, and must not close what the next run needs.
func TestPoolRoutesReuseOneConnectionPerRunner(t *testing.T) {
	fr := &fakeRunner{}
	dials := map[string]int{}
	var mu sync.Mutex
	s := New(Options{
		Store: NewMemStore(), JoinToken: joinToken, BootstrapKey: bootstrap, Now: time.Now,
		DialRunner: func(addr string) (RunnerClient, error) {
			mu.Lock()
			defer mu.Unlock()
			dials[addr]++
			return fr, nil
		},
	})
	register := func(addr string) {
		t.Helper()
		if _, err := s.Register(metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Bearer "+joinToken)), &v1.RegisterRunnerRequest{
			RunnerId: "run_test", Address: addr, Backends: []string{"bwrap"}, NetworkModes: []string{"none"},
		}); err != nil {
			t.Fatal(err)
		}
	}
	register("10.0.0.1:9200")
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	key := tenantKey(t, s, "tenant-a")
	declare(t, ts, key, "py", poolBody(1, 1))
	fr.runChunks = []*v1.RunChunk{{Payload: &v1.RunChunk_RunDone{RunDone: &v1.PoolRunDone{}}}}
	run := map[string]any{"command": "true", "timeoutSeconds": "5"}
	for range 20 {
		if got := call(t, http.MethodPost, ts.URL+"/v1/pools/py/run", key, run); got.code != http.StatusOK {
			t.Fatalf("run = %d %s", got.code, got.body)
		}
	}
	if dials["10.0.0.1:9200"] != 1 || fr.closed != 0 {
		t.Fatalf("20 runs dialled %d times and closed %d, want 1 and 0", dials["10.0.0.1:9200"], fr.closed)
	}

	register("10.0.0.2:9200")
	if got := call(t, http.MethodPost, ts.URL+"/v1/pools/py/run", key, run); got.code != http.StatusOK {
		t.Fatalf("run after the runner moved = %d %s", got.code, got.body)
	}
	if dials["10.0.0.2:9200"] != 1 || fr.closed != 1 {
		t.Fatalf("after the runner moved: dials %v, closed %d, want a dial to the new address and the old one closed", dials, fr.closed)
	}

	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if fr.closed != 2 {
		t.Fatalf("closed %d after server close, want 2", fr.closed)
	}
}

func TestTimesCountCannotWrapToFree(t *testing.T) {
	r := Runner{Backends: []string{"bwrap"}, MemoryBytes: 1 << 30, CPUMillis: 1000}
	need := needFor(1<<31-1, &v1.ResourceLimits{MemoryBytes: 1 << 62})
	if got := rejectReason(r, "bwrap", "none", need, sandboxResources{}); got != "memory" {
		t.Fatalf("rejectReason for a need that overflows = %q, want memory", got)
	}
}
