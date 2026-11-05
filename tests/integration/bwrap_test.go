//go:build integration

package integration

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	v1 "github.com/vorash/vora/api/proto/v1"
	"github.com/vorash/vora/internal/cgroup"
	"github.com/vorash/vora/internal/config"
	"github.com/vorash/vora/internal/runner"
	"github.com/vorash/vora/internal/runtime"
	"github.com/vorash/vora/internal/runtime/bwrap"
	"github.com/vorash/vora/internal/sandbox"
	"github.com/vorash/vora/internal/ulid"
	"github.com/vorash/vora/internal/workspace"
)

// imageRootfs returns the cached rootfs for an image, skipping the test if the
// image has not been built.
func imageRootfs(t *testing.T, name string) string {
	t.Helper()
	stateHome := os.Getenv("XDG_STATE_HOME")
	if stateHome == "" {
		home, _ := os.UserHomeDir()
		stateHome = filepath.Join(home, ".local", "state")
	}
	rootfs := filepath.Join(stateHome, "vora", "images", name, "latest", "rootfs")
	if info, err := os.Stat(rootfs); err != nil || !info.IsDir() {
		t.Skipf("image %q not built (run 'make images'): %v", name, err)
	}
	return rootfs
}

func TestBwrapSandboxLifecycle(t *testing.T) {
	backend, h := newBwrapSandbox(t)
	ctx := context.Background()

	// First exec writes a file.
	run(t, backend, h, "/bin/sh", []string{"-c", "echo data > /workspace/marker.txt"})

	// Second exec sees the same filesystem state (sandbox is long-lived).
	out := run(t, backend, h, "/bin/sh", []string{"-c", "cat /workspace/marker.txt"})
	if strings.TrimSpace(out) != "data" {
		t.Fatalf("marker content = %q, want data", out)
	}

	// Python works inside the sandbox.
	out = run(t, backend, h, "/usr/bin/python3", []string{"-c", "print(1 + 1)"})
	if strings.TrimSpace(out) != "2" {
		t.Fatalf("python output = %q, want 2", out)
	}

	// Stats reflect the running sandbox.
	st, err := backend.Stats(ctx, h)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if st.MemoryBytes <= 0 {
		t.Fatalf("expected positive memory usage, got %+v", st)
	}

	// Destroy removes the cgroup and process tree.
	if err := backend.Destroy(ctx, h); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if _, err := backend.Stats(ctx, h); err == nil {
		t.Fatal("Stats after Destroy should fail")
	}
}

// idlePastGrace outlasts vora-init's watchdog grace. The runner does not pass
// --grace, so the agent runs with its 20 s default.
const idlePastGrace = 30 * time.Second

// A client may leave a sandbox idle between calls for longer than vora-init's
// watchdog grace. The runner must keep it alive, so the next exec still finds
// the agent (vorash/vora#113).
func TestBwrapIdleOutlivesGrace(t *testing.T) {
	backend, h := newBwrapSandbox(t)
	time.Sleep(idlePastGrace)
	if out := strings.TrimSpace(run(t, backend, h, "/bin/echo", []string{"alive"})); out != "alive" {
		t.Fatalf("exec after %s idle = %q, want alive", idlePastGrace, out)
	}
}

// The keepalive must not defeat the watchdog's purpose: once the runner stops
// (Close), nothing pings the agent, and the sandbox ends itself.
func TestBwrapExitsWithoutRunner(t *testing.T) {
	backend, h := newBwrapSandbox(t)
	if err := backend.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	time.Sleep(idlePastGrace)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	stream, err := backend.Exec(ctx, h, runtime.ExecRequest{ExecutionID: "exe_test", Command: "/bin/echo", Args: []string{"alive"}, Cwd: "/workspace"})
	if err == nil {
		defer stream.Close()
		_, err = stream.Recv()
	}
	if err == nil {
		t.Fatalf("exec reached the agent %s after the runner stopped pinging; the watchdog did not end the sandbox", idlePastGrace)
	}
}

// stateTempDir returns a short-lived state directory for a sandbox. Not
// t.TempDir: it embeds the test's name, and the control socket under the state
// directory must fit in a Unix socket path's 108 bytes.
func stateTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "vora-it-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// newBwrapSandbox starts a bwrap sandbox on the cached base image and returns
// the backend and its handle, tearing the sandbox down at the end of the test.
func newBwrapSandbox(t *testing.T) (*bwrap.Backend, runtime.Handle) {
	t.Helper()
	return newBwrapSandboxWithEnv(t, nil)
}

func newBwrapSandboxWithEnv(t *testing.T, env map[string]string) (*bwrap.Backend, runtime.Handle) {
	t.Helper()
	return newBwrapSandboxWith(t, env, runtime.ResourceLimits{CPUMillis: 1000, MemoryBytes: 256 << 20, Pids: 64})
}

func newBwrapSandboxWith(t *testing.T, env map[string]string, limits runtime.ResourceLimits) (*bwrap.Backend, runtime.Handle) {
	t.Helper()
	backend, h, _ := newBwrapSandboxPaths(t, env, limits)
	return backend, h
}

// newBwrapSandboxPaths also returns the sandbox's paths on the host.
func newBwrapSandboxPaths(t *testing.T, env map[string]string, limits runtime.ResourceLimits) (*bwrap.Backend, runtime.Handle, runtime.WorkspaceSpec) {
	t.Helper()
	return startBwrapSandbox(t, runtime.SandboxSpec{Resources: limits, Env: env})
}

// startBwrapSandbox starts a sandbox from what spec sets, plus an id, image, network
// mode, TTL and workspace of its own.
func startBwrapSandbox(t *testing.T, spec runtime.SandboxSpec) (*bwrap.Backend, runtime.Handle, runtime.WorkspaceSpec) {
	t.Helper()

	initBin := buildBinary(t, "vora-init")
	t.Setenv("VORA_INIT_BINARY", initBin)

	cgroups, err := cgroup.New(config.Config{SystemdMode: config.SystemdUser})
	if err != nil {
		t.Skipf("systemd user manager unavailable: %v", err)
	}
	rootfs := imageRootfs(t, "base")

	cfg := config.Config{StateDir: stateTempDir(t), SystemdMode: config.SystemdUser}
	wsManager := workspace.NewManager(cfg.SandboxesDir())
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	backend, err := bwrap.New(cfg, cgroups, logger)
	if err != nil {
		t.Fatalf("new backend: %v", err)
	}

	id := ulid.NewSandbox()
	ws := wsManager.Paths(id)
	ws.RootfsDir = rootfs
	if _, err := wsManager.Create(ws); err != nil {
		t.Fatalf("workspace: %v", err)
	}
	t.Cleanup(func() { _ = wsManager.Destroy(id) })

	spec.ID = id
	spec.Image = "base"
	spec.Network = runtime.NetworkPolicy{Mode: runtime.NetworkNone}
	spec.TTL = time.Minute
	spec.Workspace = ws

	ctx := context.Background()
	h, err := backend.Create(ctx, spec)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := backend.Start(ctx, h); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = backend.Destroy(ctx, h) })
	return backend, h, ws
}

// Exec output must survive a command that writes and exits immediately. The
// in-sandbox agent drains stdout/stderr in goroutines, and it used to call
// cmd.Wait() before waiting for them: Go's Wait closes the read ends of the
// pipes StdoutPipe/StderrPipe handed out, so anything still unread — the tail of
// a burst, or all of a fast echo — was discarded while the exit chunk still
// arrived reporting success.
func TestBwrapExecDoesNotLoseOutput(t *testing.T) {
	backend, h := newBwrapSandbox(t)

	// Still draining when the writer exits: the pipe holds more than the reader
	// has taken, which is exactly what Wait-on-top-of-it throws away. Repeated,
	// because the window is narrow — a single burst only trips it about a third
	// of the time — so the run as a whole is what has to be reliable.
	const want = 1 << 20
	for i := range 8 {
		out := execStdout(t, backend, h, "/bin/sh", []string{"-c", "yes vora | head -c " + strconv.Itoa(want)})
		if len(out) != want {
			t.Fatalf("burst %d: stdout = %d bytes, want %d: output was truncated by the exit path", i, len(out), want)
		}
		if !strings.HasPrefix(out, "vora\nvora\n") {
			t.Fatalf("burst %d: stdout starts with %q, want the repeated 'vora' pattern", i, out[:min(32, len(out))])
		}
	}

	// The shape seen in the wild: a tiny command that exits at once.
	for i := range 25 {
		if got := execStdout(t, backend, h, "/bin/sh", []string{"-c", "echo api-e2e"}); got != "api-e2e\n" {
			t.Fatalf("iteration %d: stdout = %q, want %q", i, got, "api-e2e\n")
		}
	}
}

// A workload must not outlive its timeout by moving a process out of the group
// the kill reaches. The escaped process holds the exec's pipes, so the agent used
// to read on until it exited, holding its exec lock against the whole sandbox.
func TestBwrapPipeHolder(t *testing.T) {
	backend, h := newBwrapSandbox(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	start := time.Now()
	stream, err := backend.Exec(ctx, h, runtime.ExecRequest{
		ExecutionID: "exe_holder",
		Command:     "/bin/sh",
		Args:        []string{"-c", "setsid sleep 50 & echo before; sleep 50"},
		Cwd:         "/workspace",
		Timeout:     time.Second,
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	defer stream.Close()

	var out strings.Builder
	var exit *runtime.ExecResult
	for exit == nil {
		chunk, err := stream.Recv()
		if err != nil {
			t.Fatalf("stream ended without an exit chunk: %v", err)
		}
		out.Write(chunk.Stdout)
		exit = chunk.Exit
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("exec with a 1s timeout took %v: it waited for the escaped process", took)
	}
	if exit.Reason != runtime.ReasonTimeout {
		t.Fatalf("reason = %s, want %s", exit.Reason, runtime.ReasonTimeout)
	}
	if out.String() != "before\n" {
		t.Fatalf("stdout = %q: output written before the kill was lost", out.String())
	}

	start = time.Now()
	if got := execStdout(t, backend, h, "/bin/sh", []string{"-c", "echo next"}); got != "next\n" {
		t.Fatalf("next exec stdout = %q", got)
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("next exec took %v: it queued behind the escaped process", took)
	}
}

// A small pids limit must not break ordinary execs. Everything in the sandbox
// counts against the limit, including the agent's threads, so anything the agent
// adds per exec eats the workload's budget. A shim written in Go needed OS threads
// of its own, so execs died with `fatal error: newosproc` and the agent soon
// crashed the same way, which ended the sandbox.
//
// The agent's idle thread count depends on the host's core count, so the limit is
// set relative to what this host measures: room for the shell and little else.
func TestBwrapLowPids(t *testing.T) {
	probe, ph := newBwrapSandbox(t)
	st, err := probe.Stats(context.Background(), ph)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	limit := st.Pids + lowPidsHeadroom
	t.Logf("idle tasks %d, pids limit %d", st.Pids, limit)

	backend, h := newBwrapSandboxWith(t, nil, runtime.ResourceLimits{CPUMillis: 1000, MemoryBytes: 256 << 20, Pids: limit})
	for i := range 150 {
		if got := execStdout(t, backend, h, "/bin/sh", []string{"-c", "echo ok"}); got != "ok\n" {
			t.Fatalf("exec %d at pids=%d: stdout = %q, want %q", i, limit, got, "ok\n")
		}
	}
}

// lowPidsHeadroom covers the exec's shell plus a thread or two the agent may add.
const lowPidsHeadroom = 3

// execStdout runs a command and returns its stdout only.
func execStdout(t *testing.T, backend *bwrap.Backend, h runtime.Handle, command string, args []string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	stream, err := backend.Exec(ctx, h, runtime.ExecRequest{
		ExecutionID: "exe_output",
		Command:     command,
		Args:        args,
		Cwd:         "/workspace",
	})
	if err != nil {
		t.Fatalf("Exec %s: %v", command, err)
	}
	defer stream.Close()

	var out strings.Builder
	for {
		chunk, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Recv %s: %v", command, err)
		}
		out.Write(chunk.Stdout)
	}
	return out.String()
}

// run executes a command and returns its combined stdout.
func run(t *testing.T, backend *bwrap.Backend, h runtime.Handle, command string, args []string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	stream, err := backend.Exec(ctx, h, runtime.ExecRequest{
		ExecutionID: "exe_test",
		Command:     command,
		Args:        args,
		Cwd:         "/workspace",
	})
	if err != nil {
		t.Fatalf("Exec %s: %v", command, err)
	}
	defer stream.Close()

	var out strings.Builder
	for {
		chunk, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Recv %s: %v", command, err)
		}
		out.Write(chunk.Stderr)
		out.Write(chunk.Stdout)
	}
	return out.String()
}

// An exec's usage covers that exec, not the sandbox's history: a small exec
// after a large one must report the smaller peak. Before kernel 6.12
// memory.peak cannot be reset per fd, so the meter reports the lifetime peak
// and must say so rather than pass it off as the exec's.
func TestBwrapExecPeakWindow(t *testing.T) {
	backend, h := newBwrapSandbox(t)

	big := meteredRun(t, backend, h, "/usr/bin/python3", []string{"-c", "x = bytearray(64 * 1024 * 1024)"})
	small := meteredRun(t, backend, h, "/bin/true", nil)
	kernel, _ := os.ReadFile("/proc/sys/kernel/osrelease")
	t.Logf("kernel %s: big exec %+v, small exec %+v", strings.TrimSpace(string(kernel)), big, small)

	if big.MemoryPeak < 64<<20 {
		t.Fatalf("big exec peak = %d, want at least the 64 MiB it allocated", big.MemoryPeak)
	}
	switch small.PeakScope {
	case runtime.PeakWindow:
		if big.PeakScope != runtime.PeakWindow {
			t.Fatalf("scopes big=%d small=%d, want both WINDOW on one kernel", big.PeakScope, small.PeakScope)
		}
		if small.MemoryPeak >= big.MemoryPeak {
			t.Fatalf("small exec peak %d >= big exec peak %d: the window did not reset", small.MemoryPeak, big.MemoryPeak)
		}
	case runtime.PeakLifetime:
		// A lifetime peak never goes down; if it did, it was not the lifetime one.
		if small.MemoryPeak < big.MemoryPeak {
			t.Fatalf("LIFETIME peak fell from %d to %d", big.MemoryPeak, small.MemoryPeak)
		}
		t.Logf("memory.peak is not resettable on this kernel; scope LIFETIME reported")
	default:
		t.Fatalf("peak scope = %d, want WINDOW or LIFETIME on a cgroup v2 host", small.PeakScope)
	}
}

// meteredRun runs one command inside a usage window and returns the usage.
func meteredRun(t *testing.T, backend *bwrap.Backend, h runtime.Handle, command string, args []string) runtime.Usage {
	t.Helper()
	ctx := context.Background()
	m, err := backend.Meter(ctx, h)
	if err != nil {
		t.Fatalf("Meter: %v", err)
	}
	run(t, backend, h, command, args)
	u, err := m.Stop(ctx)
	if err != nil {
		t.Fatalf("meter Stop: %v", err)
	}
	return u
}

// Tenant env values must reach the sandbox byte for byte. systemd expands
// ${VAR} and $$ in the arguments of a transient service, so an unguarded
// FOO=${HOME} handed the tenant the host service manager's environment.
func TestBwrapEnvNotExpanded(t *testing.T) {
	backend, h := newBwrapSandboxWithEnv(t, map[string]string{
		"VORA_LEAK": "${HOME}",
		"VORA_COST": "$$5",
	})
	for name, want := range map[string]string{"VORA_LEAK": "${HOME}", "VORA_COST": "$$5"} {
		got := execStdout(t, backend, h, "/bin/sh", []string{"-c", `printf %s "$` + name + `"`})
		if got != want {
			t.Errorf("%s = %q, want %q: the service manager rewrote a tenant env value", name, got, want)
		}
	}
}

// An OOM in an exec must end that exec, not the sandbox (vorash/vora#108).
// systemd's default OOMPolicy=stop stopped the unit on the first kill, so the
// exec had no exit chunk, the next exec found no agent and stats lost
// everything but oom_kills. Driven through the runner, which derives the
// reason from the exec's usage window.
func TestBwrapOOMExecSurvives(t *testing.T) {
	const limit = 128 << 20
	client := newBwrapRunner(t)
	id := newRunnerSandbox(t, client, &v1.ResourceLimits{CpuMillis: 1000, MemoryBytes: limit, Pids: 64})

	exit, out, err := execExit(client, id, "/usr/bin/python3", "-c", "x = b'x' * (512 << 20)")
	if exit == nil {
		t.Fatalf("OOM exec ended without an exit chunk (stream error %v, output %q): the sandbox died with it", err, out)
	}
	u := exit.GetUsage()
	t.Logf("OOM exec: exit code %d, reason %v, usage %+v, output %q", exit.GetExitCode(), exit.GetReason(), u, out)
	if exit.GetReason() != v1.ExitReason_OOM_KILLED {
		t.Fatalf("reason = %v, want OOM_KILLED", exit.GetReason())
	}
	if u.GetOomKills() < 1 {
		t.Fatalf("usage oom_kills = %d, want >= 1", u.GetOomKills())
	}
	if peak := u.GetMemoryPeakBytes(); peak < limit*8/10 || peak > limit*12/10 {
		t.Fatalf("usage memory_peak_bytes = %d, want within 20%% of the %d limit", peak, limit)
	}

	if got := strings.TrimSpace(execViaRunner(t, client, id, "/bin/echo", "ok")); got != "ok" {
		t.Fatalf("exec after the OOM = %q, want ok", got)
	}

	st, err := client.StatsSandbox(context.Background(), &v1.StatsSandboxRequest{SandboxId: id})
	if err != nil {
		t.Fatalf("StatsSandbox: %v", err)
	}
	t.Logf("stats after the OOM: %+v", st)
	if st.GetMemoryBytes() <= 0 || st.GetCpuMillis() <= 0 || st.GetOomKills() < 1 {
		t.Fatalf("stats after the OOM = %+v, want memory, cpu and oom_kills all set", st)
	}
	if st.GetMemoryPeakBytes() < limit*8/10 {
		t.Fatalf("stats memory_peak_bytes = %d, want the OOM exec's peak near the %d limit", st.GetMemoryPeakBytes(), limit)
	}
}

// manySmallHogs starts processes that each hold an eighth of vora-init's RSS
// until together they ask for the limit plus one more agent's worth, holds them
// there for five seconds, then kills its own process group. Every hog is
// smaller than the agent, so without the workload's raised oom_score_adj the
// kernel would pick vora-init and end the sandbox. $1 is the limit in KiB.
//
// Every hog past the limit spins in reclaim, on its own core, until the kernel
// has killed one, which takes about two seconds each when the sandbox's CPU
// quota is exhausted. Asking for twice the limit, holding for 30 seconds and
// forking once per /proc entry put this past a minute on a 16-core host, so the
// overshoot is small, the hold short, and only builtins run once the hogs are up.
const manySmallHogs = `
init=
for d in /proc/[0-9]*; do
	read c < "$d/comm" && [ "$c" = vora-init ] && init=${d#/proc/} && break
done 2>/dev/null
rss() { while read k v _; do [ "$k" = VmRSS: ] && echo "$v"; done < "/proc/$init/status"; }
rss=$(rss)
kib=$((rss / 8))
n=$(( ($1 - rss) / kib + 8 ))
echo "vora-init pid $init rss $rss KiB; starting $n hogs of $kib KiB"
i=0
while [ "$i" -lt "$n" ]; do
	dd if=/dev/zero bs="${kib}k" count=1 2>/dev/null &
	i=$((i + 1))
done | sleep 5 >/dev/null
echo "hogs released; vora-init rss now $(rss) KiB"
kill -KILL 0
`

// A workload of many processes, each smaller than vora-init, must not get the
// agent OOM-killed: with OOMPolicy=continue the kernel kills only the process
// with the highest badness, and the sandbox has to survive that choice.
func TestBwrapOOMManySmall(t *testing.T) {
	const limit = 64 << 20
	backend, h := newBwrapSandboxWith(t, nil, runtime.ResourceLimits{CPUMillis: 1000, MemoryBytes: limit, Pids: 512})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	m, err := backend.Meter(ctx, h)
	if err != nil {
		t.Fatalf("Meter: %v", err)
	}
	stream, err := backend.Exec(ctx, h, runtime.ExecRequest{
		ExecutionID: "exe_hogs",
		Command:     "/bin/sh",
		Args:        []string{"-c", manySmallHogs, "sh", strconv.Itoa(limit >> 10)},
		Cwd:         "/workspace",
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	var (
		out     strings.Builder
		exit    *runtime.ExecResult
		recvErr error
	)
	for exit == nil {
		chunk, err := stream.Recv()
		if err != nil {
			recvErr = err
			break
		}
		out.Write(chunk.Stdout)
		out.Write(chunk.Stderr)
		exit = chunk.Exit
	}
	stream.Close()
	u, err := m.Stop(ctx)
	t.Logf("hogs: exit %+v, usage %+v (meter error %v), output %q", exit, u, err, out.String())
	if exit == nil {
		// A stream can end on this test's own deadline as well as on the agent's
		// death, and only the second breaks the promise under test.
		if err := execEcho(backend, h, 2*time.Minute); err != nil {
			t.Fatalf("hog exec stream ended without an exit chunk (%v) and the sandbox no longer answers (%v): the OOM took the agent down", recvErr, err)
		}
		t.Fatalf("hog exec stream ended without an exit chunk (%v) but the sandbox still answers execs: the hogs outran this test's deadline", recvErr)
	}
	if u.OOMKills < 1 {
		t.Fatalf("usage oom_kills = %d, want >= 1: the hogs never reached the limit, so this proved nothing", u.OOMKills)
	}
	if got := strings.TrimSpace(run(t, backend, h, "/bin/echo", []string{"ok"})); got != "ok" {
		t.Fatalf("exec after the OOM = %q, want ok", got)
	}
}

// leakedHogs leaves processes behind that hold the sandbox at its memory limit
// for a minute after the exec that started them has returned, the way a
// workload that leaked would: 36 of a thirty-second of the limit each, so a few
// more than fit. $1 is the limit in KiB.
const leakedHogs = `
setsid sh -c '
i=0
while [ "$i" -lt 36 ]; do
	dd if=/dev/zero bs=$(( $1 / 32 ))k count=1 2>/dev/null &
	i=$((i + 1))
done | sleep 60 >/dev/null
' sh "$1" >/dev/null 2>&1 </dev/null &
echo started
`

// An exec started while leaked workload processes hold the sandbox at its
// memory limit must not get the agent OOM-killed. The fork stalls in reclaim
// until the kernel has killed a hog, and the kernel picks that victim by
// oom_score_adj and size: the agent, at its baseline score, must not be it.
func TestBwrapOOMExecUnderPressure(t *testing.T) {
	const limit = 64 << 20
	backend, h := newBwrapSandboxWith(t, nil, runtime.ResourceLimits{CPUMillis: 1000, MemoryBytes: limit, Pids: 512})

	if got := strings.TrimSpace(run(t, backend, h, "/bin/sh", []string{"-c", leakedHogs, "sh", strconv.Itoa(limit >> 10)})); got != "started" {
		t.Fatalf("leak exec = %q, want started", got)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for {
		st, err := backend.Stats(ctx, h)
		if err != nil {
			t.Fatalf("Stats: %v", err)
		}
		if st.OOMKills >= 1 {
			t.Logf("pressure: %+v", st)
			break
		}
		if ctx.Err() != nil {
			t.Fatalf("the hogs never reached the limit: stats %+v", st)
		}
		time.Sleep(200 * time.Millisecond)
	}

	// The kernel can keep a sandbox at its limit unscheduled for a minute or
	// more, so a deadline here is a starved agent, not a dead one. Only a
	// stream the agent dropped proves it died.
	if err := execEcho(backend, h, 4*time.Minute); err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("exec under memory pressure: %v: the agent is starved or dead", err)
		}
		t.Fatalf("exec under memory pressure: %v: the agent died with the hogs", err)
	}
}

// execEcho runs echo in the sandbox on a deadline of its own and reports whether
// the exec ran to its exit chunk.
func execEcho(backend *bwrap.Backend, h runtime.Handle, deadline time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), deadline)
	defer cancel()
	stream, err := backend.Exec(ctx, h, runtime.ExecRequest{
		ExecutionID: "exe_echo",
		Command:     "/bin/echo",
		Args:        []string{"ok"},
		Cwd:         "/workspace",
	})
	if err != nil {
		return err
	}
	defer stream.Close()
	for {
		chunk, err := stream.Recv()
		if err != nil {
			if ctx.Err() != nil {
				return fmt.Errorf("no exit chunk within %s: %w", deadline, ctx.Err())
			}
			return fmt.Errorf("stream ended without an exit chunk: %w", err)
		}
		if chunk.Exit != nil {
			return nil
		}
	}
}

// newBwrapRunner serves a runner with the bwrap backend on a unix socket and
// returns a client for it.
func newBwrapRunner(t *testing.T) v1.RunnerServiceClient {
	t.Helper()
	initBin := buildBinary(t, "vora-init")
	t.Setenv("VORA_INIT_BINARY", initBin)

	cgroups, err := cgroup.New(config.Config{SystemdMode: config.SystemdUser})
	if err != nil {
		t.Skipf("systemd user manager unavailable: %v", err)
	}
	rootfs := imageRootfs(t, "base")
	realImages := filepath.Dir(filepath.Dir(filepath.Dir(rootfs))) // .../images

	stateDir := stateTempDir(t)
	if err := os.Symlink(realImages, filepath.Join(stateDir, "images")); err != nil {
		t.Fatalf("symlink images: %v", err)
	}
	cfg := config.Config{StateDir: stateDir, SystemdMode: config.SystemdUser, DefaultTTL: time.Minute}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	backend, err := bwrap.New(cfg, cgroups, logger)
	if err != nil {
		t.Fatalf("bwrap backend: %v", err)
	}
	registry := runtime.NewRegistry()
	if err := registry.Register(backend); err != nil {
		t.Fatalf("register: %v", err)
	}
	srv := runner.NewServer(cfg, sandbox.NewStore(cfg.SandboxesDir()), registry, ulid.NewRunner(), logger)

	lis, err := net.Listen("unix", filepath.Join(t.TempDir(), "runner.sock"))
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	grpcServer := grpc.NewServer()
	v1.RegisterRunnerServiceServer(grpcServer, srv)
	go func() { _ = grpcServer.Serve(lis) }()
	t.Cleanup(grpcServer.Stop)

	conn, err := grpc.NewClient("unix://"+lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return v1.NewRunnerServiceClient(conn)
}

// newRunnerSandbox creates and starts a network-none sandbox on the base
// image and destroys it at the end of the test.
func newRunnerSandbox(t *testing.T, client v1.RunnerServiceClient, limits *v1.ResourceLimits) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	created, err := client.CreateSandbox(ctx, &v1.CreateSandboxRequest{Spec: &v1.SandboxSpec{
		Image:      "vora/base",
		Resources:  limits,
		Network:    &v1.NetworkPolicy{Mode: v1.NetworkMode_NETWORK_NONE},
		TtlSeconds: 120,
	}})
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	id := created.GetSandbox().GetId()
	t.Cleanup(func() {
		_, _ = client.DestroySandbox(context.Background(), &v1.DestroySandboxRequest{SandboxId: id})
	})
	if _, err := client.StartSandbox(ctx, &v1.StartSandboxRequest{SandboxId: id}); err != nil {
		t.Fatalf("StartSandbox: %v", err)
	}
	return id
}

// execExit runs a command through the runner and returns its exit chunk, nil
// when the stream ended without one, along with its output and the error that
// ended the stream.
func execExit(client v1.RunnerServiceClient, id, command string, args ...string) (*v1.ExecExit, string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	stream, err := client.ExecSandbox(ctx, &v1.ExecSandboxRequest{
		SandboxId: id,
		Exec:      &v1.ExecRequest{Command: command, Args: args, Cwd: "/workspace"},
	})
	if err != nil {
		return nil, "", err
	}
	var (
		out  strings.Builder
		exit *v1.ExecExit
	)
	for {
		chunk, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return exit, out.String(), nil
		}
		if err != nil {
			return exit, out.String(), err
		}
		out.Write(chunk.GetStderr())
		out.Write(chunk.GetStdout())
		if e := chunk.GetExit(); e != nil {
			exit = e
		}
	}
}
