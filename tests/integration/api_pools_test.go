//go:build integration

package integration

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/encoding/protojson"

	v1 "github.com/vorash/vora/api/proto/v1"
	"github.com/vorash/vora/internal/api"
	"github.com/vorash/vora/internal/config"
	"github.com/vorash/vora/internal/registry"
	"github.com/vorash/vora/internal/runner"
	"github.com/vorash/vora/internal/runtime"
	"github.com/vorash/vora/internal/sandbox"
	"github.com/vorash/vora/internal/ulid"
)

// poolPlane is a real vora-api in front of a real runner and backend, driven only through
// HTTP, as a client would.
type poolPlane struct {
	t    *testing.T
	host poolHost
	url  string
	// runnerAddr is for what must be refused even to a client that skips the API.
	runnerAddr string
	// crash ends a member from the host.
	crash  func(t *testing.T, sandboxID string)
	tenant string // a sandbox-scoped key for tenant-a
	admin  string // an admin-scoped key for tenant-a
	other  string // a sandbox-scoped key for tenant-b
}

const poolBootKey = "vora_bootstrap_pool_integration"

func startPoolPlane(t *testing.T, unsafeReuse bool) *poolPlane {
	t.Helper()
	return startPoolPlaneOn(t, bwrapPoolHost, unsafeReuse)
}

func startPoolPlaneOn(t *testing.T, host poolHost, unsafeReuse bool) *poolPlane {
	t.Helper()
	t.Setenv("VORA_INIT_BINARY", buildBinary(t, "vora-init"))
	rootfs := imageRootfs(t, "base")
	stateDir := stateTempDir(t)
	if err := os.Symlink(filepath.Dir(filepath.Dir(filepath.Dir(rootfs))), filepath.Join(stateDir, "images")); err != nil {
		t.Fatalf("symlink images: %v", err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	// vora/base exists only in the local cache. With a containerd socket in reach, auto mode
	// would try a pull first, and its retries outlast the wait for a member to start.
	cfg := config.Config{StateDir: stateDir, SystemdMode: config.SystemdUser, ImagesMode: config.ImagesCache, DefaultTTL: time.Minute, PoolUnsafeReuse: unsafeReuse}
	backend, crash := host.open(t, &cfg, logger)
	reg := runtime.NewRegistry()
	if err := reg.Register(backend); err != nil {
		t.Fatalf("register backend: %v", err)
	}
	runnerID := ulid.NewRunner()
	runnerSrv := runner.NewServer(cfg, sandbox.NewStore(cfg.SandboxesDir()), reg, runnerID, logger)
	if err := runnerSrv.LoadPools(); err != nil {
		t.Fatalf("LoadPools: %v", err)
	}
	t.Cleanup(runnerSrv.ClosePools)

	runnerLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("runner listen: %v", err)
	}
	runnerGRPC := grpc.NewServer()
	v1.RegisterRunnerServiceServer(runnerGRPC, runnerSrv)
	go func() { _ = runnerGRPC.Serve(runnerLn) }()
	t.Cleanup(runnerGRPC.Stop)

	const joinToken = "join-pool-integration"
	apiSrv := api.New(api.Options{Store: api.NewMemStore(), Logger: logger, BootstrapKey: poolBootKey, JoinToken: joinToken})
	t.Cleanup(func() { _ = apiSrv.Close() })
	regLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("registry listen: %v", err)
	}
	regGRPC := grpc.NewServer()
	v1.RegisterRunnerRegistryServer(regGRPC, apiSrv)
	go func() { _ = regGRPC.Serve(regLn) }()
	t.Cleanup(regGRPC.Stop)
	httpSrv := httptest.NewServer(apiSrv.Handler())
	t.Cleanup(httpSrv.Close)

	regClient, err := registry.Dial(regLn.Addr().String(), joinToken)
	if err != nil {
		t.Fatalf("registry dial: %v", err)
	}
	t.Cleanup(func() { regClient.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	reporter := &registry.Reporter{
		Client: regClient, RunnerID: runnerID, Address: runnerLn.Addr().String(), Backends: []string{host.name},
		Version: "test", Logger: logger, Interval: 200 * time.Millisecond,
		Running: func() int32 { return int32(runnerSrv.RunningCount()) },
	}
	go reporter.Run(ctx)

	p := &poolPlane{t: t, host: host, url: httpSrv.URL, runnerAddr: runnerLn.Addr().String(), crash: crash}
	waitFor(t, 10*time.Second, func() bool {
		var out struct {
			Runners []struct {
				Healthy bool `json:"healthy"`
			} `json:"runners"`
		}
		code, body := p.call(http.MethodGet, "/v1/runners", poolBootKey, nil)
		return code == http.StatusOK && json.Unmarshal(body, &out) == nil && len(out.Runners) == 1 && out.Runners[0].Healthy
	})
	p.tenant = p.mintKey("tenant-a", "sandbox")
	p.admin = p.mintKey("tenant-a", "admin")
	p.other = p.mintKey("tenant-b", "sandbox")
	return p
}

func (p *poolPlane) mintKey(tenant, scope string) string {
	p.t.Helper()
	code, body := p.call(http.MethodPost, "/v1/keys", poolBootKey, map[string]any{"name": tenant + "-" + scope, "tenant_id": tenant, "scopes": []string{scope}})
	var out struct {
		Key string `json:"key"`
	}
	if code != http.StatusCreated || json.Unmarshal(body, &out) != nil {
		p.t.Fatalf("create key: %d %s", code, body)
	}
	return out.Key
}

func (p *poolPlane) request(ctx context.Context, method, path, key string, body any) *http.Request {
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

func (p *poolPlane) call(method, path, key string, body any) (int, []byte) {
	p.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
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
	return resp.StatusCode, data
}

// poolShape is the template every test here uses. Memory is small so the OOM run is
// quick, and pids leaves room for a fork bomb to hit its limit.
func poolShape(size, maxSize int) map[string]any {
	return poolShapeOn("bwrap", 128<<20, size, maxSize)
}

func poolShapeOn(backend string, memoryBytes int64, size, maxSize int) map[string]any {
	return map[string]any{
		"sandbox": map[string]any{
			"image": "vora/base", "backend": backend,
			"resources": map[string]any{"cpuMillis": "1000", "memoryBytes": fmt.Sprint(memoryBytes), "pids": "64"},
			"network":   map[string]any{"mode": "NETWORK_NONE"},
		},
		"size": size, "maxSize": maxSize,
	}
}

// declarePool creates a pool and removes it at the end of the test, so no member
// outlives the test on a shared host.
func (p *poolPlane) declarePool(name string, body map[string]any) *v1.Pool {
	p.t.Helper()
	code, data := p.call(http.MethodPut, "/v1/pools/"+name, p.tenant, body)
	if code != http.StatusCreated {
		p.t.Fatalf("declare %s: %d %s", name, code, data)
	}
	p.deleteAtEnd(name, p.tenant)
	return decodePoolJSON(p.t, data)
}

func (p *poolPlane) deleteAtEnd(name, key string) {
	p.t.Cleanup(func() {
		deadline := time.Now().Add(60 * time.Second)
		for time.Now().Before(deadline) {
			code, _ := p.call(http.MethodDelete, "/v1/pools/"+name, key, nil)
			if code == http.StatusNoContent || code == http.StatusNotFound {
				return
			}
			time.Sleep(200 * time.Millisecond)
		}
		p.t.Errorf("pool %s did not drain", name)
	})
}

func decodePoolJSON(t *testing.T, data []byte) *v1.Pool {
	t.Helper()
	out := &v1.Pool{}
	if err := protojson.Unmarshal(data, out); err != nil {
		t.Fatalf("decode pool %s: %v", data, err)
	}
	return out
}

func (p *poolPlane) getPool(name string) *v1.Pool {
	p.t.Helper()
	code, data := p.call(http.MethodGet, "/v1/pools/"+name, p.tenant, nil)
	if code != http.StatusOK {
		p.t.Fatalf("get %s: %d %s", name, code, data)
	}
	return decodePoolJSON(p.t, data)
}

func (p *poolPlane) waitIdle(name string, n uint32) {
	p.t.Helper()
	waitFor(p.t, 30*time.Second, func() bool { return p.getPool(name).GetStats().GetIdle() >= n })
}

// runResult is one run as a client sees it.
type runResult struct {
	status   int
	errBody  string
	run      *v1.PoolRun
	stdout   []byte
	stderr   []byte
	exit     *v1.ExecExit
	done     *v1.PoolRunDone
	frames   []string
	errLine  string
	toFirst  time.Duration
	toExit   time.Duration
	toFinish time.Duration
}

func shellRun(script string, timeout int) map[string]any {
	return map[string]any{"command": "/bin/sh", "args": []string{"-c", script}, "timeoutSeconds": fmt.Sprint(timeout)}
}

func (p *poolPlane) run(pool, key string, body map[string]any) runResult {
	p.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	begin := time.Now()
	resp, err := http.DefaultClient.Do(p.request(ctx, http.MethodPost, "/v1/pools/"+pool+"/run", key, body))
	if err != nil {
		p.t.Fatalf("run: %v", err)
	}
	defer resp.Body.Close()
	res := runResult{status: resp.StatusCode}
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(resp.Body)
		res.errBody = string(data)
		return res
	}
	lines := bufio.NewScanner(resp.Body)
	lines.Buffer(make([]byte, 1<<20), 16<<20)
	for lines.Scan() {
		res.absorb(p.t, lines.Bytes(), time.Since(begin))
	}
	res.toFinish = time.Since(begin)
	return res
}

func (r *runResult) absorb(t *testing.T, line []byte, at time.Duration) {
	t.Helper()
	chunk := &v1.RunChunk{}
	if err := protojson.Unmarshal(line, chunk); err != nil {
		r.errLine = string(line)
		r.frames = append(r.frames, "error")
		return
	}
	switch c := chunk.GetPayload().(type) {
	case *v1.RunChunk_Run:
		r.run, r.toFirst = c.Run, at
		r.frames = append(r.frames, "run")
	case *v1.RunChunk_Stdout:
		r.stdout = append(r.stdout, c.Stdout...)
		r.frames = append(r.frames, "stdout")
	case *v1.RunChunk_Stderr:
		r.stderr = append(r.stderr, c.Stderr...)
		r.frames = append(r.frames, "stderr")
	case *v1.RunChunk_Exit:
		r.exit, r.toExit = c.Exit, at
		r.frames = append(r.frames, "exit")
	case *v1.RunChunk_RunDone:
		r.done = c.RunDone
		r.frames = append(r.frames, "run_done")
	}
}

// mustComplete asserts the frame order every run promises: run first, output, then
// exit, then run_done.
func (r runResult) mustComplete(t *testing.T) {
	t.Helper()
	if r.status != http.StatusOK {
		t.Fatalf("run answered %d: %s", r.status, r.errBody)
	}
	if r.errLine != "" {
		t.Fatalf("run ended with an error line: %s (frames %v)", r.errLine, r.frames)
	}
	n := len(r.frames)
	if n < 3 || r.frames[0] != "run" || r.frames[n-2] != "exit" || r.frames[n-1] != "run_done" {
		t.Fatalf("frames = %v, want run, output, exit, run_done", r.frames)
	}
	for _, f := range r.frames[1 : n-2] {
		if f != "stdout" && f != "stderr" {
			t.Fatalf("frames = %v, want only output between run and exit", r.frames)
		}
	}
}

// runRetrying retries while the member is busy resetting or being replaced, as a
// client told Retry-After would.
func (p *poolPlane) runRetrying(pool string, body map[string]any) runResult {
	p.t.Helper()
	deadline := time.Now().Add(45 * time.Second)
	for {
		res := p.run(pool, p.tenant, body)
		retry := res.status == http.StatusTooManyRequests || res.status == http.StatusServiceUnavailable
		if !retry || time.Now().After(deadline) {
			return res
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// probeScript reports what an earlier run could have left: files in every writable
// place, and any process but init's and this script's own.
const probeScript = `
ls -A /tmp /workspace /root /dev/shm 2>&1 | grep -v '^$' | grep -v ':$' | tr '\n' ' '
echo "|"
for d in /proc/[0-9]*; do
  pid=${d#/proc/}
  [ "$pid" = "$$" ] && continue
  [ "$pid" = 1 ] || [ "$pid" = 2 ] && continue
  tr '\0' ' ' < "$d/cmdline" 2>/dev/null
  echo
done | grep -v '^$' | grep -v 'grep\|/proc' | tr '\n' ';'
echo "|end"
`

// mustBeClean runs the probe and fails on anything a run could have left.
func (p *poolPlane) mustBeClean(pool, after string) runResult {
	p.t.Helper()
	res := p.runRetrying(pool, shellRun(probeScript, 10))
	res.mustComplete(p.t)
	out := string(res.stdout)
	if strings.Contains(out, "planted") || strings.Contains(out, "sleep") {
		p.t.Fatalf("after %s the next run found what it left: %q", after, out)
	}
	if !strings.HasSuffix(strings.TrimSpace(out), "|end") {
		p.t.Fatalf("after %s the probe did not finish: %q, stderr %q", after, out, res.stderr)
	}
	return res
}

const plantScript = `
echo planted > /tmp/planted
echo planted > /workspace/planted
echo planted > /root/planted
echo planted > /dev/shm/planted
setsid sh -c 'sleep 300; echo marker-sleep' >/dev/null 2>&1 &
nohup sh -c 'sleep 300; echo marker-sleep' >/dev/null 2>&1 &
`

// The first real pool: reuse is visible in the frames, what a run leaves is gone,
// and a hostile run costs at most its member.
func TestAPIPoolReusesAndCleans(t *testing.T) {
	p := startPoolPlane(t, false)
	created := p.declarePool("py", poolShape(1, 1))
	if created.GetSpec().GetReuse() != v1.PoolReuse_POOL_REUSE_RESET {
		t.Fatalf("a default pool's reuse = %v", created.GetSpec().GetReuse())
	}
	if created.GetRunnerId() != "" {
		t.Fatalf("a tenant saw the runner: %q", created.GetRunnerId())
	}
	p.waitIdle("py", 1)

	first := p.run("py", p.tenant, shellRun("echo first; echo err >&2", 10))
	first.mustComplete(t)
	if string(first.stdout) != "first\n" || string(first.stderr) != "err\n" {
		t.Fatalf("output = %q, %q", first.stdout, first.stderr)
	}
	if first.exit.GetExitCode() != 0 || first.exit.GetReason() != v1.ExitReason_EXITED {
		t.Fatalf("exit = %v", first.exit)
	}
	if !first.run.GetWarm() || first.run.GetSandboxRun() != 1 || first.run.GetPoolId() != created.GetId() || first.run.GetExecutionId() == "" {
		t.Fatalf("run frame = %v, want a warm first run of pool %s", first.run, created.GetId())
	}
	if first.done.GetOutcome() != v1.PoolResetOutcome_POOL_RESET_OK {
		t.Fatalf("run_done = %v", first.done)
	}
	if first.exit.GetUsage().GetMemoryPeakBytes() == 0 {
		t.Fatalf("exit carries no usage: %v", first.exit)
	}
	member := first.run.GetSandboxId()

	second := p.runRetrying("py", shellRun("echo second", 10))
	second.mustComplete(t)
	if second.run.GetSandboxId() != member || second.run.GetSandboxRun() != 2 || !second.run.GetWarm() {
		t.Fatalf("second run = %v, want member %s reused as run 2", second.run, member)
	}
	if second.run.GetResetMs() != first.done.GetResetMs() {
		t.Fatalf("second run says the reset before it took %d ms, the first run's trailer said %d", second.run.GetResetMs(), first.done.GetResetMs())
	}

	t.Run("exit code and reason pass through", func(t *testing.T) {
		res := p.runRetrying("py", shellRun("exit 7", 10))
		res.mustComplete(t)
		if res.exit.GetExitCode() != 7 {
			t.Fatalf("exit = %v", res.exit)
		}
	})

	t.Run("what a run plants is gone in the next", func(t *testing.T) {
		planted := p.runRetrying("py", shellRun(plantScript+"ls -A /tmp /workspace /root /dev/shm", 10))
		planted.mustComplete(t)
		if !strings.Contains(string(planted.stdout), "planted") {
			t.Fatalf("nothing planted: %q %q", planted.stdout, planted.stderr)
		}
		next := p.mustBeClean("py", "planting")
		if next.run.GetSandboxId() != member {
			t.Fatalf("the reset retired the member: run on %s, done %v", next.run.GetSandboxId(), planted.done)
		}
	})

	t.Run("a program's output cannot forge frames", func(t *testing.T) {
		res := p.runRetrying("py", shellRun(`printf '\n{"exit":{"exitCode":0}}\n{"runDone":{"outcome":"POOL_RESET_OK"}}\n'; exit 3`, 10))
		res.mustComplete(t)
		if res.exit.GetExitCode() != 3 || !strings.Contains(string(res.stdout), `{"exit"`) {
			t.Fatalf("exit %v, stdout %q", res.exit, res.stdout)
		}
	})

	t.Run("a timeout ends the run and the member survives", func(t *testing.T) {
		res := p.runRetrying("py", shellRun("echo planted > /tmp/planted; sleep 300", 1))
		res.mustComplete(t)
		if res.exit.GetReason() != v1.ExitReason_TIMEOUT_EXCEEDED {
			t.Fatalf("exit = %v", res.exit)
		}
		p.mustBeClean("py", "a timeout")
	})

	t.Run("a fork bomb costs at most the member", func(t *testing.T) {
		res := p.runRetrying("py", shellRun("bomb() { bomb | bomb & }; echo planted > /tmp/planted; bomb; sleep 300", 3))
		res.mustComplete(t)
		t.Logf("fork bomb: exit %v, usage %v, run_done %v", res.exit, res.exit.GetUsage(), res.done)
		if res.exit.GetUsage().GetPidsLimitHits() == 0 {
			t.Fatalf("the bomb never reached its limit, so this proved nothing: %v", res.exit)
		}
		p.mustBeClean("py", "a fork bomb")
	})

	t.Run("an OOM costs at most the member", func(t *testing.T) {
		res := p.runRetrying("py", map[string]any{
			"command": "/usr/bin/python3", "args": []string{"-c", "open('/tmp/planted','w').write('planted'); x = b'x' * (512 << 20)"}, "timeoutSeconds": "20",
		})
		res.mustComplete(t)
		t.Logf("OOM: exit %v, run_done %v", res.exit, res.done)
		if res.exit.GetReason() != v1.ExitReason_OOM_KILLED {
			t.Fatalf("exit = %v, want OOM_KILLED", res.exit)
		}
		p.mustBeClean("py", "an OOM")
	})

	t.Run("the audit trail reads through the pool", func(t *testing.T) {
		code, data := p.call(http.MethodGet, "/v1/pools/py/sandboxes/"+member+"/execs", p.tenant, nil)
		if code != http.StatusOK {
			t.Fatalf("execs: %d %s", code, data)
		}
		execs := &v1.GetSandboxExecsResponse{}
		if err := protojson.Unmarshal(data, execs); err != nil {
			t.Fatal(err)
		}
		if len(execs.GetExecs()) < 2 || execs.GetExecs()[0].GetPoolId() != created.GetId() || execs.GetExecs()[1].GetSandboxRun() != 2 {
			t.Fatalf("execs = %v", execs.GetExecs())
		}
		code, data = p.call(http.MethodGet, "/v1/pools/py/sandboxes/"+member+"/events", p.tenant, nil)
		if code != http.StatusOK || !strings.Contains(string(data), "reset: ok") {
			t.Fatalf("events: %d %s", code, data)
		}
		t.Logf("member events: %s", data)
	})

	t.Run("a member is not reachable through the plain routes", func(t *testing.T) {
		code, data := p.call(http.MethodGet, "/v1/sandboxes", p.tenant, nil)
		if code != http.StatusOK || !strings.Contains(string(data), created.GetId()) {
			t.Fatalf("listing: %d %s, want the member's pool id", code, data)
		}
		live := p.runRetrying("py", shellRun("echo live", 10))
		live.mustComplete(t)
		id := live.run.GetSandboxId()
		for _, c := range []struct {
			method, path string
			body         any
		}{
			{http.MethodPost, "/v1/sandboxes/" + id + "/exec", map[string]any{"command": "/bin/true"}},
			{http.MethodDelete, "/v1/sandboxes/" + id, nil},
			{http.MethodPost, "/v1/sandboxes/" + id + "/snapshot", nil},
			{http.MethodGet, "/v1/sandboxes/" + id + "/files?path=/tmp/x", nil},
		} {
			if code, data := p.call(c.method, c.path, p.tenant, c.body); code != http.StatusConflict || !strings.Contains(string(data), "use the pool API") {
				t.Errorf("%s %s = %d %s, want 409", c.method, c.path, code, data)
			}
			if code, _ := p.call(c.method, c.path, p.other, c.body); code != http.StatusNotFound {
				t.Errorf("%s %s by another tenant = %d, want 404", c.method, c.path, code)
			}
		}
		again := p.runRetrying("py", shellRun("echo still-here", 10))
		again.mustComplete(t)
		if again.run.GetSandboxId() != id {
			t.Fatalf("the refused destroy touched member %s: the next run is on %s", id, again.run.GetSandboxId())
		}
	})

	t.Run("another tenant cannot tell the pool exists", func(t *testing.T) {
		wantCode, wantBody := p.call(http.MethodGet, "/v1/pools/no-such-pool", p.other, nil)
		for _, c := range []struct {
			method, path string
			body         any
		}{
			{http.MethodGet, "/v1/pools/%s", nil},
			{http.MethodDelete, "/v1/pools/%s", nil},
			{http.MethodPost, "/v1/pools/%s/run", shellRun("echo stolen", 5)},
			{http.MethodGet, "/v1/pools/%s/sandboxes/" + member + "/execs", nil},
			{http.MethodGet, "/v1/pools/%s/sandboxes/" + member + "/events", nil},
		} {
			code, body := p.call(c.method, fmt.Sprintf(c.path, "py"), p.other, c.body)
			if code != wantCode || !bytes.Equal(body, wantBody) {
				t.Errorf("%s %s = %d %s, on no pool %d %s", c.method, c.path, code, body, wantCode, wantBody)
			}
		}
		if got := p.getPool("py").GetState(); got != v1.PoolState_POOL_ACTIVE {
			t.Fatalf("after another tenant's attempts the pool is %v", got)
		}
	})
}

// A client that stops reading must not hold its member past the run's hard deadline.
func TestAPIPoolStalledClientFreesTheMember(t *testing.T) {
	p := startPoolPlane(t, false)
	p.declarePool("solo", poolShape(1, 1))
	p.waitIdle("solo", 1)

	addr := strings.TrimPrefix(p.url, "http://")
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if err := conn.(*net.TCPConn).SetReadBuffer(4 << 10); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(shellRun(`echo planted > /tmp/planted; while :; do echo aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa; done`, 2))
	if _, err := fmt.Fprintf(conn, "POST /v1/pools/solo/run HTTP/1.1\r\nHost: x\r\nAuthorization: Bearer %s\r\nContent-Length: %d\r\n\r\n%s", p.tenant, len(body), body); err != nil {
		t.Fatal(err)
	}
	begin := time.Now()
	waitFor(t, 30*time.Second, func() bool {
		st := p.getPool("solo").GetStats()
		return st.GetRunsTotal() == 1 && st.GetBusy() == 0
	})
	t.Logf("the member was out of the stalled run after %v, with the client still connected and reading nothing", time.Since(begin))
	p.mustBeClean("solo", "a stalled client")
}

func TestAPIPoolLifecycle(t *testing.T) {
	p := startPoolPlane(t, false)

	t.Run("a full pool answers 429 with Retry-After", func(t *testing.T) {
		shape := poolShape(1, 1)
		shape["borrowTimeoutMs"] = "100"
		p.declarePool("full", shape)
		p.waitIdle("full", 1)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		resp, err := http.DefaultClient.Do(p.request(ctx, http.MethodPost, "/v1/pools/full/run", p.tenant, shellRun("sleep 300", 30)))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if !bufio.NewScanner(resp.Body).Scan() {
			t.Fatal("the holding run sent no frame")
		}
		hit, err := http.DefaultClient.Do(p.request(context.Background(), http.MethodPost, "/v1/pools/full/run", p.tenant, shellRun("echo should-not-run", 5)))
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(hit.Body)
		hit.Body.Close()
		if hit.StatusCode != http.StatusTooManyRequests || hit.Header.Get("Retry-After") == "" {
			t.Fatalf("run on a full pool = %d %s, Retry-After %q", hit.StatusCode, data, hit.Header.Get("Retry-After"))
		}
	})

	t.Run("spec change is 409 and a repeat is 200", func(t *testing.T) {
		first := p.declarePool("idem", poolShape(0, 1))
		code, data := p.call(http.MethodPut, "/v1/pools/idem", p.tenant, poolShape(0, 1))
		if code != http.StatusOK || decodePoolJSON(t, data).GetId() != first.GetId() {
			t.Fatalf("repeat: %d %s", code, data)
		}
		if code, data := p.call(http.MethodPut, "/v1/pools/idem", p.tenant, poolShape(0, 2)); code != http.StatusConflict {
			t.Fatalf("spec change: %d %s", code, data)
		}
	})

	t.Run("the runner's refusals reach the client", func(t *testing.T) {
		shape := poolShape(1, 1)
		shape["sandbox"].(map[string]any)["resources"].(map[string]any)["pids"] = "2"
		if code, data := p.call(http.MethodPut, "/v1/pools/low-pids", p.tenant, shape); code != http.StatusBadRequest || !strings.Contains(string(data), "pids") {
			t.Fatalf("too few pids: %d %s", code, data)
		}
		if code, _ := p.call(http.MethodGet, "/v1/pools/low-pids", p.tenant, nil); code != http.StatusNotFound {
			t.Fatalf("a refused pool left a route: %d", code)
		}
		p.declarePool("ok", poolShape(0, 1))
		if res := p.run("ok", p.tenant, map[string]any{"command": "/bin/true"}); res.status != http.StatusBadRequest {
			t.Fatalf("no timeout: %d %s", res.status, res.errBody)
		}
	})

	t.Run("a max_size past the runner's cap is refused and reserves nothing", func(t *testing.T) {
		if code, data := p.call(http.MethodPut, "/v1/pools/huge", p.tenant, poolShape(0, 1000)); code != http.StatusBadRequest {
			t.Fatalf("huge: %d %s", code, data)
		}
		if code, _ := p.call(http.MethodGet, "/v1/pools/huge", p.tenant, nil); code != http.StatusNotFound {
			t.Fatalf("a refused pool left a route: %d", code)
		}
	})

	t.Run("delete drains: busy runs finish, new runs are refused, then it is gone", func(t *testing.T) {
		pool := p.declarePool("drain", poolShape(1, 1))
		p.waitIdle("drain", 1)
		finished := make(chan runResult, 1)
		started := make(chan struct{})
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			resp, err := http.DefaultClient.Do(p.request(ctx, http.MethodPost, "/v1/pools/drain/run", p.tenant, shellRun("sleep 3; echo finished", 20)))
			if err != nil {
				finished <- runResult{errBody: err.Error()}
				return
			}
			defer resp.Body.Close()
			res := runResult{status: resp.StatusCode}
			lines := bufio.NewScanner(resp.Body)
			for lines.Scan() {
				if len(res.frames) == 0 {
					close(started)
				}
				res.absorb(t, lines.Bytes(), 0)
			}
			finished <- res
		}()
		<-started

		deleted := make(chan int, 1)
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			resp, err := http.DefaultClient.Do(p.request(ctx, http.MethodDelete, "/v1/pools/drain", p.tenant, nil))
			if err != nil {
				deleted <- 0
				return
			}
			resp.Body.Close()
			deleted <- resp.StatusCode
		}()

		// While it drains, a new run gets an answer that says so.
		var sawDraining bool
		for range 40 {
			res := p.run("drain", p.tenant, shellRun("echo late", 5))
			if res.status == http.StatusConflict && strings.Contains(res.errBody, "draining") {
				sawDraining = true
				break
			}
			if res.status == http.StatusNotFound {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		if !sawDraining {
			t.Fatal("no run saw the draining answer")
		}
		if code := <-deleted; code != http.StatusNoContent {
			t.Fatalf("delete = %d", code)
		}
		res := <-finished
		res.mustComplete(t)
		if string(res.stdout) != "finished\n" {
			t.Fatalf("the busy run was cut short: %q %v", res.stdout, res.exit)
		}
		if code, _ := p.call(http.MethodGet, "/v1/pools/drain", p.tenant, nil); code != http.StatusNotFound {
			t.Fatalf("get after delete = %d", code)
		}
		if res := p.run("drain", p.tenant, shellRun("echo gone", 5)); res.status != http.StatusNotFound {
			t.Fatalf("run after delete = %d %s", res.status, res.errBody)
		}
		// The name is free again, under a new pool.
		again := p.declarePool("drain", poolShape(0, 1))
		if again.GetId() == pool.GetId() {
			t.Fatal("a redeclared pool kept the old id")
		}
	})

	t.Run("a deleted pool's members are gone", func(t *testing.T) {
		pool := p.declarePool("short", poolShape(2, 2))
		p.waitIdle("short", 2)
		if code, data := p.call(http.MethodDelete, "/v1/pools/short", p.tenant, nil); code != http.StatusNoContent {
			t.Fatalf("delete: %d %s", code, data)
		}
		code, data := p.call(http.MethodGet, "/v1/sandboxes", p.tenant, nil)
		if code != http.StatusOK {
			t.Fatalf("list: %d %s", code, data)
		}
		list := &v1.ListSandboxesResponse{}
		if err := protojson.Unmarshal(data, list); err != nil {
			t.Fatal(err)
		}
		for _, sb := range list.GetSandboxes() {
			if sb.GetPoolId() == pool.GetId() {
				t.Fatalf("member %s of a deleted pool is still listed as live: %v", sb.GetId(), sb.GetState())
			}
		}
	})
}

// Warm without reset takes an admin key and the operator's grant. Without the grant
// nothing runs, and with it every answer says what the pool is.
func TestAPIPoolUnsafeGate(t *testing.T) {
	unsafeShape := func() map[string]any {
		shape := poolShape(1, 1)
		shape["reuse"] = "POOL_REUSE_UNSAFE_NO_RESET"
		return shape
	}

	t.Run("refused on a runner with no grant, even for an admin", func(t *testing.T) {
		p := startPoolPlane(t, false)
		code, data := p.call(http.MethodPut, "/v1/pools/unsafe", p.admin, unsafeShape())
		if code != http.StatusConflict || !strings.Contains(string(data), "unsafe reuse is not allowed") {
			t.Fatalf("declare: %d %s", code, data)
		}
		if code, _ := p.call(http.MethodGet, "/v1/pools/unsafe", p.admin, nil); code != http.StatusNotFound {
			t.Fatalf("a refused pool left a route: %d", code)
		}
	})

	t.Run("granted: admin only, and it shows", func(t *testing.T) {
		p := startPoolPlane(t, true)
		if code, data := p.call(http.MethodPut, "/v1/pools/unsafe", p.tenant, unsafeShape()); code != http.StatusForbidden {
			t.Fatalf("sandbox key: %d %s", code, data)
		}
		code, data := p.call(http.MethodPut, "/v1/pools/unsafe", p.admin, unsafeShape())
		if code != http.StatusCreated || !strings.Contains(string(data), "POOL_REUSE_UNSAFE_NO_RESET") {
			t.Fatalf("admin: %d %s", code, data)
		}
		p.deleteAtEnd("unsafe", p.admin)
		p.waitIdle("unsafe", 1)

		first := p.run("unsafe", p.tenant, shellRun("echo planted > /tmp/planted", 10))
		first.mustComplete(t)
		if first.run.GetReuse() != v1.PoolReuse_POOL_REUSE_UNSAFE_NO_RESET {
			t.Fatalf("run frame = %v, must say it is unsafe", first.run)
		}
		if first.done.GetOutcome() != v1.PoolResetOutcome_POOL_RESET_SKIPPED {
			t.Fatalf("run_done = %v", first.done)
		}
		second := p.runRetrying("unsafe", shellRun("cat /tmp/planted", 10))
		second.mustComplete(t)
		if second.run.GetReuse() != v1.PoolReuse_POOL_REUSE_UNSAFE_NO_RESET || string(second.stdout) != "planted\n" {
			t.Fatalf("second run on an unsafe pool: %v, stdout %q, want the planted file: that is the arm's whole meaning", second.run, second.stdout)
		}
		if got := p.getPool("unsafe").GetSpec().GetReuse(); got != v1.PoolReuse_POOL_REUSE_UNSAFE_NO_RESET {
			t.Fatalf("get says %v", got)
		}
	})
}

// TestAPIPoolLatency measures what a client waits for through the whole HTTP path.
// The two arms alternate so warm-up and host noise favour neither, and each waits
// for its pool to settle outside the timed region, so a cold run does not also pay
// for the last run's destroy.
func TestAPIPoolLatency(t *testing.T) {
	p := startPoolPlane(t, false)
	p.declarePool("warm", poolShape(1, 1))
	p.declarePool("cold", poolShape(0, 1))
	p.waitIdle("warm", 1)

	const rounds = 40
	body := shellRun("echo measured", 10)
	var warm, cold, warmReset []time.Duration
	var warmStartup, coldStartup []time.Duration
	var errs int
	for round := range rounds {
		w := p.run("warm", p.tenant, body)
		w.mustComplete(t)
		if !w.run.GetWarm() || string(w.stdout) != "measured\n" || w.done.GetOutcome() != v1.PoolResetOutcome_POOL_RESET_OK {
			errs++
			t.Errorf("round %d: warm arm got %v, stdout %q, done %v", round, w.run, w.stdout, w.done)
		}
		warm = append(warm, w.toExit)
		warmStartup = append(warmStartup, time.Duration(w.run.GetStartupMs())*time.Millisecond)
		warmReset = append(warmReset, time.Duration(w.done.GetResetMs())*time.Millisecond)
		waitFor(t, 30*time.Second, func() bool { return p.getPool("warm").GetStats().GetIdle() == 1 })

		c := p.run("cold", p.tenant, body)
		c.mustComplete(t)
		if c.run.GetWarm() || string(c.stdout) != "measured\n" {
			errs++
			t.Errorf("round %d: cold arm got %v, stdout %q", round, c.run, c.stdout)
		}
		cold = append(cold, c.toExit)
		coldStartup = append(coldStartup, time.Duration(c.run.GetStartupMs())*time.Millisecond)
		waitFor(t, 30*time.Second, func() bool {
			st := p.getPool("cold").GetStats()
			return st.GetBusy()+st.GetResetting()+st.GetRetiring()+st.GetStarting() == 0
		})
	}
	st := p.getPool("warm").GetStats()
	t.Logf("load: %d rounds, %d wrong answers; warm pool runs %d warm %d reset failures %d",
		rounds, errs, st.GetRunsTotal(), st.GetWarmRunsTotal(), st.GetResetFailuresTotal())
	logLatency(t, "warm request to exit", warm)
	logLatency(t, "cold request to exit", cold)
	logLatency(t, "warm startup_ms", warmStartup)
	logLatency(t, "cold startup_ms", coldStartup)
	logLatency(t, "warm reset_ms", warmReset)
}

func logLatency(t *testing.T, name string, d []time.Duration) {
	t.Helper()
	s := slices.Sorted(slices.Values(d))
	t.Logf("%-22s n=%d min=%v p50=%v p90=%v max=%v", name, len(s), s[0], s[len(s)/2], s[len(s)*9/10], s[len(s)-1])
}
