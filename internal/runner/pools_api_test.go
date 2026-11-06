package runner

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/encoding/protojson"

	v1 "github.com/vorash/vora/api/proto/v1"
	"github.com/vorash/vora/internal/api"
	"github.com/vorash/vora/internal/client"
	"github.com/vorash/vora/internal/config"
	"github.com/vorash/vora/internal/pool"
	"github.com/vorash/vora/internal/runtime"
	"github.com/vorash/vora/internal/sandbox"
	"github.com/vorash/vora/internal/ulid"
)

// These tests run vora-api's real handlers against the real runner over real gRPC,
// with only the backend faked. They hold what each side's own tests cannot: that
// the codes and texts the runner really sends turn into the answers the API
// promises, and that the backend's ledger stays true through the whole path.

const apiPlaneBootKey = "vora_bootstrap_runner_test"

type apiPlane struct {
	t     *testing.T
	api   *api.Server
	store *api.MemStore
	url   string
	key   string // sandbox scope, testTenant
	other string // sandbox scope, another tenant
	admin string // admin scope, testTenant
}

func newAPIPlane(t *testing.T, s *Server) *apiPlane {
	t.Helper()
	store := api.NewMemStore()
	srv := api.New(api.Options{
		Store: store, BootstrapKey: apiPlaneBootKey,
		DialRunner: func(addr string) (api.RunnerClient, error) { return client.DialAddress(addr) },
	})
	t.Cleanup(func() { _ = srv.Close() })
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	p := &apiPlane{t: t, api: srv, store: store, url: ts.URL}
	p.key = p.mintKey(testTenant, api.ScopeSandbox)
	p.other = p.mintKey("tenant-b", api.ScopeSandbox)
	p.admin = p.mintKey(testTenant, api.ScopeAdmin)
	p.serve(s)
	return p
}

// serve points the API at a runner, as a runner registering at boot does.
func (p *apiPlane) serve(s *Server) {
	p.t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		p.t.Fatal(err)
	}
	srv := grpc.NewServer()
	v1.RegisterRunnerServiceServer(srv, s)
	go func() { _ = srv.Serve(lis) }()
	p.t.Cleanup(srv.Stop)
	if _, err := p.api.Register(metadata.NewIncomingContext(context.Background(), metadata.MD{}), &v1.RegisterRunnerRequest{
		RunnerId: "run_plane", Address: lis.Addr().String(), Backends: []string{"fake"}, NetworkModes: []string{"none"},
	}); err != nil {
		p.t.Fatal(err)
	}
}

func (p *apiPlane) mintKey(tenant, scope string) string {
	p.t.Helper()
	plaintext, hash, err := api.NewAPIKey()
	if err != nil {
		p.t.Fatal(err)
	}
	if err := p.store.CreateKey(context.Background(), api.APIKey{
		ID: ulid.NewAPIKeyID(), TenantID: tenant, KeyHash: hash, Scopes: []string{scope}, CreatedAt: time.Now(),
	}); err != nil {
		p.t.Fatal(err)
	}
	return plaintext
}

func (p *apiPlane) request(ctx context.Context, method, path, key string, body any) *http.Request {
	p.t.Helper()
	var r io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			p.t.Fatal(err)
		}
		r = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, p.url+path, r)
	if err != nil {
		p.t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	return req
}

type planeAnswer struct {
	code       int
	body       string
	retryAfter string
}

func (p *apiPlane) call(method, path, key string, body any) planeAnswer {
	p.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	resp, err := http.DefaultClient.Do(p.request(ctx, method, path, key, body))
	if err != nil {
		p.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		p.t.Fatalf("%s %s: read: %v", method, path, err)
	}
	return planeAnswer{resp.StatusCode, string(data), resp.Header.Get("Retry-After")}
}

func planeShape(size, maxSize int) map[string]any {
	return map[string]any{
		"sandbox": map[string]any{
			"image": "img", "backend": "fake",
			"resources": map[string]any{"cpuMillis": "100", "memoryBytes": "1048576", "pids": "16"},
			"network":   map[string]any{"mode": "NETWORK_NONE"},
		},
		"size": size, "maxSize": maxSize,
	}
}

func (p *apiPlane) declare(name string, body map[string]any) *v1.Pool {
	p.t.Helper()
	got := p.call(http.MethodPut, "/v1/pools/"+name, p.key, body)
	if got.code != http.StatusCreated {
		p.t.Fatalf("declare %s = %d %s", name, got.code, got.body)
	}
	return decodePlanePool(p.t, got.body)
}

func decodePlanePool(t *testing.T, body string) *v1.Pool {
	t.Helper()
	out := &v1.Pool{}
	if err := protojson.Unmarshal([]byte(body), out); err != nil {
		t.Fatalf("decode %q: %v", body, err)
	}
	return out
}

func (p *apiPlane) waitIdle(name string, n uint32) {
	p.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		got := p.call(http.MethodGet, "/v1/pools/"+name, p.key, nil)
		if got.code == http.StatusOK && decodePlanePool(p.t, got.body).GetStats().GetIdle() >= n {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	p.t.Fatalf("pool %s never reached %d idle", name, n)
}

type planeRun struct {
	code   int
	body   string
	retry  string
	run    *v1.PoolRun
	stdout string
	exit   *v1.ExecExit
	done   *v1.PoolRunDone
	frames []string
}

func (p *apiPlane) run(name string, timeout int) planeRun {
	p.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	resp, err := http.DefaultClient.Do(p.request(ctx, http.MethodPost, "/v1/pools/"+name+"/run", p.key, map[string]any{"command": "/bin/true", "timeoutSeconds": fmt.Sprint(timeout)}))
	if err != nil {
		p.t.Fatalf("run: %v", err)
	}
	defer resp.Body.Close()
	out := planeRun{code: resp.StatusCode, retry: resp.Header.Get("Retry-After")}
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(resp.Body)
		out.body = string(data)
		return out
	}
	lines := bufio.NewScanner(resp.Body)
	for lines.Scan() {
		chunk := &v1.RunChunk{}
		if err := protojson.Unmarshal(lines.Bytes(), chunk); err != nil {
			out.frames = append(out.frames, "error:"+lines.Text())
			continue
		}
		switch c := chunk.GetPayload().(type) {
		case *v1.RunChunk_Run:
			out.run = c.Run
			out.frames = append(out.frames, "run")
		case *v1.RunChunk_Stdout:
			out.stdout += string(c.Stdout)
			out.frames = append(out.frames, "stdout")
		case *v1.RunChunk_Exit:
			out.exit = c.Exit
			out.frames = append(out.frames, "exit")
		case *v1.RunChunk_RunDone:
			out.done = c.RunDone
			out.frames = append(out.frames, "run_done")
		}
	}
	return out
}

func (r planeRun) mustComplete(t *testing.T) {
	t.Helper()
	if r.code != http.StatusOK {
		t.Fatalf("run = %d %s", r.code, r.body)
	}
	if strings.Join(r.frames, ",") != "run,stdout,exit,run_done" {
		t.Fatalf("frames = %v", r.frames)
	}
}

func TestAPIRunnerReuseAndAudit(t *testing.T) {
	h := newWarmHarness(t)
	p := newAPIPlane(t, h.s)
	created := p.declare("py", planeShape(1, 1))
	if created.GetState() != v1.PoolState_POOL_ACTIVE || created.GetSpec().GetMaxRuns() != 100 {
		t.Fatalf("declare answer = %v, want the runner's defaults filled in", created)
	}
	p.waitIdle("py", 1)

	first := p.run("py", 5)
	first.mustComplete(t)
	if !first.run.GetWarm() || first.run.GetSandboxRun() != 1 || first.run.GetReuse() != v1.PoolReuse_POOL_REUSE_RESET || first.done.GetOutcome() != v1.PoolResetOutcome_POOL_RESET_OK {
		t.Fatalf("first run = %v, %v", first.run, first.done)
	}
	second := p.run("py", 5)
	second.mustComplete(t)
	if second.run.GetSandboxId() != first.run.GetSandboxId() || second.run.GetSandboxRun() != 2 {
		t.Fatalf("second run = %v, want the same member as run 2", second.run)
	}
	member := first.run.GetSandboxId()

	got := p.call(http.MethodGet, "/v1/pools/py/sandboxes/"+member+"/execs", p.key, nil)
	execs := &v1.GetSandboxExecsResponse{}
	if err := protojson.Unmarshal([]byte(got.body), execs); err != nil || got.code != http.StatusOK || len(execs.GetExecs()) != 2 || execs.GetExecs()[1].GetSandboxRun() != 2 {
		t.Fatalf("execs = %d %s, %v", got.code, got.body, err)
	}

	// A sandbox of another pool is no member of this one.
	p.declare("other", planeShape(0, 1))
	for _, tail := range []string{"/execs", "/events"} {
		if got := p.call(http.MethodGet, "/v1/pools/other/sandboxes/"+member+tail, p.key, nil); got.code != http.StatusNotFound {
			t.Fatalf("%s through another pool = %d %s", tail, got.code, got.body)
		}
	}

	// The real runner's records say what a plain route refuses, and listing says why.
	if list := p.call(http.MethodGet, "/v1/sandboxes", p.key, nil); !strings.Contains(list.body, created.GetId()) {
		t.Fatalf("listing has no pool id: %s", list.body)
	}
	for _, c := range []struct {
		method, path string
		body         any
	}{
		{http.MethodGet, "/v1/sandboxes/" + member, nil},
		{http.MethodDelete, "/v1/sandboxes/" + member, nil},
		{http.MethodPost, "/v1/sandboxes/" + member + "/exec", map[string]any{"command": "/bin/true"}},
		{http.MethodPost, "/v1/sandboxes/" + member + "/snapshot", nil},
	} {
		if got := p.call(c.method, c.path, p.key, c.body); got.code != http.StatusConflict || !strings.Contains(got.body, created.GetId()) {
			t.Errorf("%s %s = %d %s, want 409 naming the pool", c.method, c.path, got.code, got.body)
		}
		if got := p.call(c.method, c.path, p.other, c.body); got.code != http.StatusNotFound {
			t.Errorf("%s %s by another tenant = %d %s, want 404", c.method, c.path, got.code, got.body)
		}
	}

	if got := p.call(http.MethodDelete, "/v1/pools/py", p.key, nil); got.code != http.StatusNoContent {
		t.Fatalf("delete = %d %s", got.code, got.body)
	}
	if got := p.call(http.MethodGet, "/v1/pools/py", p.key, nil); got.code != http.StatusNotFound {
		t.Fatalf("get after delete = %d %s", got.code, got.body)
	}
}

// What the runner really records on a failed reset names host paths. A tenant reads
// what happened, and the operator reads why.
func TestAPIRunnerFiltersRawResetText(t *testing.T) {
	const hostPath = "/var/lib/vora/sandboxes/secret/control.sock"
	h := newWarmHarness(t)
	p := newAPIPlane(t, h.s)
	p.declare("py", planeShape(1, 1))
	p.waitIdle("py", 1)
	h.backend.set(func(b *poolBackend) {
		b.resetHook = func(context.Context, string) error { return errors.New("dial unix " + hostPath + ": refused") }
	})
	res := p.run("py", 5)
	res.mustComplete(t)
	if res.done.GetOutcome() != v1.PoolResetOutcome_POOL_RESET_FAILED {
		t.Fatalf("run_done = %v, want the failed reset reported", res.done)
	}
	if strings.Contains(res.done.GetRetireCause(), "/var/lib") {
		t.Fatalf("run_done carries backend text: %v", res.done)
	}
	path := "/v1/pools/py/sandboxes/" + res.run.GetSandboxId() + "/events"
	tenant := p.call(http.MethodGet, path, p.key, nil)
	if tenant.code != http.StatusOK || !strings.Contains(tenant.body, "reset: failed") || strings.Contains(tenant.body, "/var/lib") {
		t.Fatalf("tenant events = %d %s", tenant.code, tenant.body)
	}
	operator := p.call(http.MethodGet, path+"?tenant_id="+testTenant, apiPlaneBootKey, nil)
	if !strings.Contains(operator.body, hostPath) {
		t.Fatalf("operator events = %d %s", operator.code, operator.body)
	}
}

// Between a runner registering and its pools loading, it refuses creates. A client
// has to get an answer it can act on, and a repeat must converge.
func TestAPIRunnerStartupWindow(t *testing.T) {
	backend := reusableBackend{newPoolBackend(t)}
	reg := runtime.NewRegistry()
	if err := reg.Register(backend); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{StateDir: t.TempDir()}
	s := NewServer(cfg, sandbox.NewStore(cfg.SandboxesDir()), reg, "run_test", slog.New(slog.NewTextHandler(io.Discard, nil)))
	tunePoolServer(t, s)
	p := newAPIPlane(t, s)

	plain := p.call(http.MethodPost, "/v1/sandboxes", p.key, planeShape(0, 0)["sandbox"])
	if plain.code != http.StatusServiceUnavailable || plain.retryAfter == "" || !strings.Contains(plain.body, "still starting") {
		t.Fatalf("plain create while loading = %+v", plain)
	}
	got := p.call(http.MethodPut, "/v1/pools/py", p.key, planeShape(0, 1))
	if got.code != http.StatusServiceUnavailable || got.retryAfter == "" {
		t.Fatalf("declare while loading = %+v", got)
	}

	if err := s.LoadPools(); err != nil {
		t.Fatal(err)
	}
	again := p.call(http.MethodPut, "/v1/pools/py", p.key, planeShape(0, 1))
	if again.code != http.StatusOK && again.code != http.StatusCreated {
		t.Fatalf("declare after loading = %+v", again)
	}
	if res := p.run("py", 5); res.code != http.StatusOK {
		t.Fatalf("run after loading = %d %s", res.code, res.body)
	}
	if got := p.call(http.MethodPost, "/v1/sandboxes", p.key, planeShape(0, 0)["sandbox"]); got.code != http.StatusCreated {
		t.Fatalf("plain create after loading = %+v", got)
	}
}

// A declare the runner refuses while shutting down keeps its route, and answers 503.
func TestAPIRunnerShutdownAnswersUnavailable(t *testing.T) {
	h := newWarmHarness(t)
	p := newAPIPlane(t, h.s)
	h.s.ClosePools()
	got := p.call(http.MethodPut, "/v1/pools/py", p.key, planeShape(0, 1))
	if got.code != http.StatusServiceUnavailable || got.retryAfter == "" {
		t.Fatalf("declare during shutdown = %+v", got)
	}
	// Nothing was created, so a delete learns that and frees the reservation.
	if got := p.call(http.MethodDelete, "/v1/pools/py", p.key, nil); got.code != http.StatusNoContent {
		t.Fatalf("delete of a pool the runner never made = %+v", got)
	}
}

func TestAPIRunnerCapacityAndSpecRefusals(t *testing.T) {
	h := newWarmHarness(t)
	h.s.cfg.MaxSandboxesPerTenant = 2
	p := newAPIPlane(t, h.s)

	if got := p.call(http.MethodPut, "/v1/pools/huge", p.key, planeShape(0, 3)); got.code != http.StatusBadRequest || !strings.Contains(got.body, "max_size") {
		t.Fatalf("max_size past the cap = %+v", got)
	}
	p.declare("one", planeShape(0, 2))
	// The API's books have room, as the runner reported no capacity; the runner's do not.
	got := p.call(http.MethodPut, "/v1/pools/two", p.key, planeShape(0, 1))
	if got.code != http.StatusTooManyRequests || got.retryAfter == "" {
		t.Fatalf("declare past the runner's cap = %+v", got)
	}
	if route, err := p.store.PoolRoute(context.Background(), testTenant, "two"); !errors.Is(err, api.ErrNotFound) {
		t.Fatalf("a refused pool kept its reservation: %+v, %v", route, err)
	}
	patient := planeShape(0, 1)
	patient["borrowTimeoutMs"] = "300001"
	if got := p.call(http.MethodPut, "/v1/pools/patient", p.key, patient); got.code != http.StatusBadRequest || !strings.Contains(got.body, "borrow_timeout") {
		t.Fatalf("borrow_timeout_ms past its cap = %+v", got)
	}
	if got := p.call(http.MethodPut, "/v1/pools/BAD_NAME", p.key, planeShape(0, 1)); got.code != http.StatusBadRequest {
		t.Fatalf("a name the runner refuses = %+v", got)
	}
	if res := p.run("one", 0); res.code != http.StatusBadRequest || !strings.Contains(res.body, "timeout_seconds") {
		t.Fatalf("a run with no timeout = %d %s", res.code, res.body)
	}
}

func TestAPIRunnerUnsafeGate(t *testing.T) {
	unsafe := planeShape(1, 1)
	unsafe["reuse"] = "POOL_REUSE_UNSAFE_NO_RESET"

	t.Run("no grant", func(t *testing.T) {
		h := newWarmHarness(t)
		p := newAPIPlane(t, h.s)
		got := p.call(http.MethodPut, "/v1/pools/py", p.admin, unsafe)
		if got.code != http.StatusConflict || !strings.Contains(got.body, "unsafe reuse is not allowed") {
			t.Fatalf("admin on a runner with no grant = %+v", got)
		}
		if _, err := p.store.PoolRoute(context.Background(), testTenant, "py"); !errors.Is(err, api.ErrNotFound) {
			t.Fatalf("the refused pool kept a route: %v", err)
		}
	})
	t.Run("granted", func(t *testing.T) {
		h := newWarmHarness(t)
		h.s.cfg.PoolUnsafeReuse = true
		p := newAPIPlane(t, h.s)
		if got := p.call(http.MethodPut, "/v1/pools/py", p.key, unsafe); got.code != http.StatusForbidden {
			t.Fatalf("sandbox key = %+v", got)
		}
		got := p.call(http.MethodPut, "/v1/pools/py", p.admin, unsafe)
		if got.code != http.StatusCreated || decodePlanePool(t, got.body).GetSpec().GetReuse() != v1.PoolReuse_POOL_REUSE_UNSAFE_NO_RESET {
			t.Fatalf("admin = %+v", got)
		}
		p.waitIdle("py", 1)
		res := p.run("py", 5)
		res.mustComplete(t)
		if res.run.GetReuse() != v1.PoolReuse_POOL_REUSE_UNSAFE_NO_RESET || res.done.GetOutcome() != v1.PoolResetOutcome_POOL_RESET_SKIPPED {
			t.Fatalf("run = %v, %v, want every frame to say it is unsafe", res.run, res.done)
		}
		if same := p.call(http.MethodPut, "/v1/pools/py", p.key, planeShape(1, 1)); same.code != http.StatusConflict {
			t.Fatalf("a safe spec over an unsafe pool = %+v", same)
		}
	})
}

// A pool a later boot refused reads as a state. It keeps its route for the boot that
// can honour it, refuses runs and same-name declares, and can be deleted.
func TestAPIRunnerNotLoadedPool(t *testing.T) {
	h := newWarmHarness(t)
	h.s.cfg.PoolUnsafeReuse = true
	p := newAPIPlane(t, h.s)
	unsafe := planeShape(1, 2)
	unsafe["reuse"] = "POOL_REUSE_UNSAFE_NO_RESET"
	if got := p.call(http.MethodPut, "/v1/pools/py", p.admin, unsafe); got.code != http.StatusCreated {
		t.Fatalf("declare = %+v", got)
	}

	next := h.restart(func(c *config.Config) { c.PoolUnsafeReuse = false }, reusableBackend{newPoolBackend(t)})
	p.serve(next.s)

	got := p.call(http.MethodGet, "/v1/pools/py", p.key, nil)
	pool := decodePlanePool(t, got.body)
	if got.code != http.StatusOK || pool.GetState() != v1.PoolState_POOL_NOT_LOADED || pool.GetNotLoadedReason() != "unsafe_reuse_not_granted" {
		t.Fatalf("get = %+v", got)
	}
	route, err := p.store.PoolRoute(context.Background(), testTenant, "py")
	if err != nil || route.MaxSize != 2 {
		t.Fatalf("route = %+v, %v: reading a not-loaded pool, whose spec has no defaults, must not touch it", route, err)
	}
	if list := p.call(http.MethodGet, "/v1/pools", p.key, nil); !strings.Contains(list.body, "POOL_NOT_LOADED") {
		t.Fatalf("list = %+v", list)
	}
	if res := p.run("py", 5); res.code != http.StatusConflict || !strings.Contains(res.body, "not loaded") {
		t.Fatalf("run = %d %s", res.code, res.body)
	}
	if got := p.call(http.MethodPut, "/v1/pools/py", p.key, planeShape(1, 2)); got.code != http.StatusConflict || !strings.Contains(got.body, "not loaded") {
		t.Fatalf("same-name declare = %+v", got)
	}
	if _, err := p.store.PoolRoute(context.Background(), testTenant, "py"); err != nil {
		t.Fatalf("the refused calls dropped the route: %v", err)
	}
	if got := p.call(http.MethodDelete, "/v1/pools/py", p.key, nil); got.code != http.StatusNoContent {
		t.Fatalf("delete = %+v", got)
	}
	// The name is free, and a pool of it now loads.
	if got := p.call(http.MethodPut, "/v1/pools/py", p.key, planeShape(1, 2)); got.code != http.StatusCreated {
		t.Fatalf("redeclare = %+v", got)
	}
}

// A runner that restarted and lost nothing keeps answering on the API's reused
// connection, with a pool's route still good.
func TestAPIRunnerRestartKeepsTheRoute(t *testing.T) {
	h := newWarmHarness(t)
	p := newAPIPlane(t, h.s)
	created := p.declare("py", planeShape(1, 1))
	p.waitIdle("py", 1)
	next := h.restart(nil, reusableBackend{newPoolBackend(t)})
	p.serve(next.s)
	got := p.call(http.MethodGet, "/v1/pools/py", p.key, nil)
	if got.code != http.StatusOK || decodePlanePool(t, got.body).GetId() != created.GetId() {
		t.Fatalf("get after restart = %+v", got)
	}
	p.waitIdle("py", 1)
	p.run("py", 5).mustComplete(t)
}

// The runner answers every declare with the reuse it filled in, so a route never
// stays unspecified once the runner has answered.
func TestAPIRunnerFillsTheRoutesReuse(t *testing.T) {
	h := newWarmHarness(t)
	p := newAPIPlane(t, h.s)
	p.declare("py", planeShape(0, 1))
	route, err := p.store.PoolRoute(context.Background(), testTenant, "py")
	if err != nil || route.Reuse != v1.PoolReuse_POOL_REUSE_RESET {
		t.Fatalf("route = %+v, %v, want POOL_REUSE_RESET", route, err)
	}
}

// A runner that comes back without a pool's record leaves a route the runner cannot
// back. The API must say so, keep the reservation until the tenant acts, and let a
// declare bring the pool back on that runner.
func TestAPIRunnerRestartThatLostThePool(t *testing.T) {
	h := newWarmHarness(t)
	p := newAPIPlane(t, h.s)
	created := p.declare("py", planeShape(1, 1))
	p.waitIdle("py", 1)
	if err := h.s.deletePoolRecord(pool.ID(created.GetId())); err != nil {
		t.Fatal(err)
	}
	next := h.restart(nil, reusableBackend{newPoolBackend(t)})
	p.serve(next.s)

	got := p.call(http.MethodGet, "/v1/pools/py", p.key, nil)
	if got.code != http.StatusOK || decodePlanePool(t, got.body).GetState() != v1.PoolState_POOL_NOT_ON_RUNNER {
		t.Fatalf("get after losing the record = %+v, want 200 POOL_NOT_ON_RUNNER", got)
	}
	if res := p.run("py", 5); res.code != http.StatusConflict || !strings.Contains(res.body, "declare it again") {
		t.Fatalf("run after losing the record = %d %s, want 409", res.code, res.body)
	}
	if route, err := p.store.PoolRoute(context.Background(), testTenant, "py"); err != nil || route.PoolID != created.GetId() {
		t.Fatalf("reads changed the route: %+v, %v", route, err)
	}
	again := p.call(http.MethodPut, "/v1/pools/py", p.key, planeShape(1, 1))
	if again.code != http.StatusOK || decodePlanePool(t, again.body).GetId() == created.GetId() {
		t.Fatalf("redeclare = %+v, want 200 and a new id", again)
	}
	p.waitIdle("py", 1)
	p.run("py", 5).mustComplete(t)
}

// Resources have no default to fill in: Parse refuses a template without them, so
// no pool can exist that reserved nothing.
func TestAPIRunnerRefusesATemplateWithNoResources(t *testing.T) {
	h := newWarmHarness(t)
	p := newAPIPlane(t, h.s)
	for name, resources := range map[string]map[string]any{
		"no-cpu":    {"memoryBytes": "1048576", "pids": "16"},
		"no-memory": {"cpuMillis": "100", "pids": "16"},
		"none":      {"pids": "16"},
	} {
		shape := planeShape(0, 1)
		shape["sandbox"].(map[string]any)["resources"] = resources
		got := p.call(http.MethodPut, "/v1/pools/"+name, p.key, shape)
		if got.code != http.StatusBadRequest {
			t.Fatalf("declare %s = %+v, want 400", name, got)
		}
		if _, err := p.store.PoolRoute(context.Background(), testTenant, name); !errors.Is(err, api.ErrNotFound) {
			t.Fatalf("declare %s kept a route: %v", name, err)
		}
	}
}

func TestAPIRunnerRunStatuses(t *testing.T) {
	t.Run("a full pool is 429", func(t *testing.T) {
		h := newWarmHarness(t)
		p := newAPIPlane(t, h.s)
		shape := planeShape(1, 1)
		shape["borrowTimeoutMs"] = "5"
		p.declare("py", shape)
		p.waitIdle("py", 1)
		release := make(chan struct{})
		defer close(release)
		h.backend.set(func(b *poolBackend) {
			b.execHook = func(ctx context.Context, _ string, _ runtime.ExecRequest) (runtime.ExecStream, error) {
				s := newCtxStream(ctx, false, runtime.ExecChunk{Stdout: []byte("out")}, runtime.ExecChunk{Exit: &runtime.ExecResult{Reason: runtime.ReasonExited}})
				s.gate = release
				return s, nil
			}
		})
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		resp, err := http.DefaultClient.Do(p.request(ctx, http.MethodPost, "/v1/pools/py/run", p.key, map[string]any{"command": "/bin/true", "timeoutSeconds": "20"}))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		res := p.run("py", 5)
		if res.code != http.StatusTooManyRequests || res.retry == "" {
			t.Fatalf("run on a full pool = %d %s, Retry-After %q", res.code, res.body, res.retry)
		}
	})
	t.Run("a cold start that fails is 503 with no backend text", func(t *testing.T) {
		h := newWarmHarness(t)
		h.backend.set(func(b *poolBackend) {
			b.startHook = func(context.Context, string) error { return errors.New("mount /var/lib/vora/secret: denied") }
		})
		p := newAPIPlane(t, h.s)
		p.declare("py", planeShape(0, 1))
		res := p.run("py", 5)
		if res.code != http.StatusServiceUnavailable || res.retry == "" || strings.Contains(res.body, "/var/lib") {
			t.Fatalf("run = %d %s, Retry-After %q", res.code, res.body, res.retry)
		}
		got := p.call(http.MethodGet, "/v1/pools/py", p.key, nil)
		if strings.Contains(got.body, "/var/lib") {
			t.Fatalf("stats carry backend text: %s", got.body)
		}
	})
	t.Run("a run on a draining pool says so", func(t *testing.T) {
		h := newWarmHarness(t)
		p := newAPIPlane(t, h.s)
		p.declare("py", planeShape(1, 1))
		p.waitIdle("py", 1)
		release := make(chan struct{})
		entered := make(chan struct{}, 1)
		h.backend.set(func(b *poolBackend) {
			b.execHook = func(ctx context.Context, _ string, _ runtime.ExecRequest) (runtime.ExecStream, error) {
				entered <- struct{}{}
				s := newCtxStream(ctx, false, runtime.ExecChunk{Exit: &runtime.ExecResult{Reason: runtime.ReasonExited}})
				s.gate = release
				return s, nil
			}
		})
		running := make(chan planeRun, 1)
		go func() { running <- p.run("py", 20) }()
		<-entered
		deleted := make(chan planeAnswer, 1)
		go func() { deleted <- p.call(http.MethodDelete, "/v1/pools/py", p.key, nil) }()
		deadline := time.Now().Add(5 * time.Second)
		for {
			if res := p.run("py", 5); res.code == http.StatusConflict && strings.Contains(res.body, "draining") {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("no run saw the draining answer")
			}
			time.Sleep(5 * time.Millisecond)
		}
		close(release)
		if res := <-running; res.exit == nil {
			t.Fatalf("the busy run was cut short: %v", res.frames)
		}
		if got := <-deleted; got.code != http.StatusNoContent {
			t.Fatalf("delete = %+v", got)
		}
		if _, err := p.store.PoolRoute(context.Background(), testTenant, "py"); !errors.Is(err, api.ErrNotFound) {
			t.Fatalf("route after the drain: %v", err)
		}
	})
}

// The whole path of the build log's hazard: a client that leaves mid-run. The
// backend's ledger fails the test if a reset ever starts while an exec is live, or a
// member runs again with no passing reset, which is what a leak past the proof
// would need. A pool of one puts the next run on the abandoned member.
func TestAPIRunnerDisconnectNeverReachesAResetWithALiveExec(t *testing.T) {
	h := newWarmHarness(t)
	p := newAPIPlane(t, h.s)
	p.declare("py", planeShape(1, 1))
	p.waitIdle("py", 1)
	h.backend.set(func(b *poolBackend) {
		b.execHook = func(ctx context.Context, _ string, req runtime.ExecRequest) (runtime.ExecStream, error) {
			if req.Command == "/bin/hold" {
				return newCtxStream(ctx, false, runtime.ExecChunk{Stdout: []byte("out")}), nil
			}
			return newCtxStream(ctx, false, runtime.ExecChunk{Stdout: []byte("out")}, runtime.ExecChunk{Exit: &runtime.ExecResult{Reason: runtime.ReasonExited}}), nil
		}
	})
	var member string
	for round := range 40 {
		ctx, cancel := context.WithCancel(context.Background())
		resp, err := http.DefaultClient.Do(p.request(ctx, http.MethodPost, "/v1/pools/py/run", p.key, map[string]any{"command": "/bin/hold", "timeoutSeconds": "30"}))
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		if resp.StatusCode == http.StatusOK && round%2 == 0 {
			bufio.NewScanner(resp.Body).Scan()
		}
		cancel()
		resp.Body.Close()

		// The next run waits out the reset, and finds the same member reset.
		res := p.run("py", 5)
		res.mustComplete(t)
		if member == "" {
			member = res.run.GetSandboxId()
		}
		if res.run.GetSandboxId() != member {
			t.Fatalf("round %d: the member was replaced (%s), so the next run proved nothing about it", round, res.run.GetSandboxId())
		}
	}
	box := h.backend.box(member)
	if box.resets < 40 {
		t.Fatalf("%d resets across 40 disconnected runs and their followers", box.resets)
	}
}

// A client that stops reading is cut off at the run's hard deadline, however long it
// stays connected, and the member goes back through its reset.
func TestAPIRunnerStalledClientFreesTheMember(t *testing.T) {
	h := newWarmHarness(t)
	p := newAPIPlane(t, h.s)
	p.declare("py", planeShape(1, 1))
	p.waitIdle("py", 1)
	out := bytes.Repeat([]byte("x"), 32<<10)
	h.backend.set(func(b *poolBackend) {
		b.execHook = func(ctx context.Context, _ string, _ runtime.ExecRequest) (runtime.ExecStream, error) {
			return &endlessStream{ctx: ctx, chunk: out}, nil
		}
	})
	conn, err := net.Dial("tcp", strings.TrimPrefix(p.url, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	body := `{"command":"/bin/true","timeoutSeconds":"1"}`
	if _, err := fmt.Fprintf(conn, "POST /v1/pools/py/run HTTP/1.1\r\nHost: x\r\nAuthorization: Bearer %s\r\nContent-Length: %d\r\n\r\n%s", p.key, len(body), body); err != nil {
		t.Fatal(err)
	}
	begin := time.Now()
	deadline := begin.Add(20 * time.Second)
	for {
		got := decodePlanePool(t, p.call(http.MethodGet, "/v1/pools/py", p.key, nil).body)
		if st := got.GetStats(); st.GetRunsTotal() == 1 && st.GetBusy() == 0 && st.GetIdle() == 1 {
			t.Logf("member idle again after %v, with the client connected and reading nothing", time.Since(begin))
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the member is still held: %v", got.GetStats())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// endlessStream is a program that writes for ever until it is killed.
type endlessStream struct {
	ctx   context.Context
	chunk []byte
}

func (s *endlessStream) Recv() (runtime.ExecChunk, error) {
	if err := s.ctx.Err(); err != nil {
		return runtime.ExecChunk{}, err
	}
	return runtime.ExecChunk{Stdout: s.chunk}, nil
}

func (s *endlessStream) Close() error { return nil }
