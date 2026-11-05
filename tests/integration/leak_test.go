//go:build integration

package integration

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vorash/vora/internal/pool"
	"github.com/vorash/vora/internal/runtime"
)

// This file is the leak matrix of the warm-pool design, written against runtime.Backend
// and runtime.Resettable only, so that any backend with runtime.Reusable can run it: give
// testLeakMatrix a leakBackend. Each row is run A plants, reset, run B checks, and each
// also checks that its plant took, or a reset that did nothing would pass.

// keepFile is seeded into /workspace before the sandbox starts, the way a template is.
const keepFile = "keep"

// leakMember is a prepared sandbox.
type leakMember struct {
	backend runtime.Backend
	h       runtime.Handle
	reset   runtime.Resettable
}

// mustReset resets with the deadline the pool uses and fails the test if it is not proven.
func (m leakMember) mustReset(t *testing.T) runtime.ResetReport {
	t.Helper()
	report, err := m.tryReset()
	if err != nil {
		t.Fatalf("Reset: %v", err)
	}
	return report
}

func (m leakMember) tryReset() (runtime.ResetReport, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return m.reset.Reset(ctx)
}

// leakBackend is what a backend hands the matrix.
type leakBackend struct {
	// newMember starts a sandbox with keepFile in /workspace and calls Prepare on it. It
	// returns an error rather than failing the test, because a pool calls it from its own
	// goroutine.
	newMember func(t *testing.T) (leakMember, error)
	// scratch lists the sandbox's own writable places, binds its bind mounts.
	scratch []string
	binds   []string
}

func (b leakBackend) member(t *testing.T) leakMember {
	t.Helper()
	m, err := b.newMember(t)
	if err != nil {
		t.Fatalf("new member: %v", err)
	}
	return m
}

// leakRows is the matrix. A row plants, resets and checks, leaving a clean member, so the
// rows also run back to back on one member.
var leakRows = []struct {
	name string
	run  func(t *testing.T, b leakBackend, m leakMember)
}{
	{"L1_scratch_files", leakL1ScratchFiles},
	{"L2_bind_mount_files", leakL2BindMountFiles},
	{"L3_vora", leakL3Vora},
	{"L4_env", leakL4Env},
	{"L5_background_processes", leakL5BackgroundProcesses},
	{"L6_holder_of_stdout", leakL6HolderOfStdout},
	{"L6_holder_of_stdout_past_a_timeout", leakL6HolderPastATimeout},
}

func testLeakMatrix(t *testing.T, b leakBackend) {
	for _, row := range leakRows {
		t.Run(row.name, func(t *testing.T) { row.run(t, b, b.member(t)) })
	}
	t.Run("back_to_back_on_one_member", func(t *testing.T) {
		m := b.member(t)
		for range 2 {
			for _, row := range leakRows {
				row.run(t, b, m)
			}
		}
	})
	t.Run("L9_failed_reset_never_returns", func(t *testing.T) { leakL9FailedResetNeverReturns(t, b) })
}

func leakFilesRow(t *testing.T, m leakMember, dirs []string, extra func(t *testing.T)) {
	t.Helper()
	before := snapshot(t, m.backend, m.h, dirs)
	plant(t, m.backend, m.h, dirs)
	if extra != nil {
		extra(t)
	}
	if got := snapshot(t, m.backend, m.h, dirs); got == before {
		t.Fatal("the plant changed nothing, so this row proves nothing")
	}
	// After the control, because the snapshot itself cannot read these.
	mustExec(t, m.backend, m.h, "/usr/bin/python3", append([]string{"-c", oddPlants}, dirs...)...)
	report := m.mustReset(t)
	if report.EntriesRemoved == 0 {
		t.Errorf("report %+v, want the planted entries counted", report)
	}
	if got := snapshot(t, m.backend, m.h, dirs); got != before {
		t.Fatalf("a next run reads something else after the reset:\n%s", lineDiff(before, got))
	}
}

// lineDiff shows the lines only one side has, which beats two whole listings.
func lineDiff(before, after string) string {
	a, b := strings.Split(before, "\n"), strings.Split(after, "\n")
	var out []string
	for _, l := range a {
		if !slices.Contains(b, l) {
			out = append(out, "- "+l)
		}
	}
	for _, l := range b {
		if !slices.Contains(a, l) {
			out = append(out, "+ "+l)
		}
	}
	return strings.Join(out, "\n")
}

// oddPlants leaves what would hang or confuse a helper that opened or read what it removes:
// a FIFO, a socket nobody listens on, a name that is not UTF-8, and a tree deeper than a
// path can reach in one go.
const oddPlants = `
import os, socket, sys
for d in sys.argv[1:]:
    os.mkfifo(d + "/fifo")
    s = socket.socket(socket.AF_UNIX)
    s.bind(d + "/socket")
    s.close()
    with open(os.fsencode(d) + b"/name-\xff\xfe", "w") as f:
        f.write("x")
    fd = os.open(d, os.O_RDONLY)
    for _ in range(400):
        os.mkdir("deep-directory-name-to-pass-path-max", dir_fd=fd)
        next_fd = os.open("deep-directory-name-to-pass-path-max", os.O_RDONLY, dir_fd=fd)
        os.close(fd)
        fd = next_fd
    os.close(fd)
`

func leakL1ScratchFiles(t *testing.T, b leakBackend, m leakMember) {
	leakFilesRow(t, m, b.scratch, nil)
}

// userHooks makes Python run a run's code in a later run: the user site's usercustomize.py
// and .pth files both execute at interpreter start.
const userHooks = `
import os, site
user = site.getusersitepackages()
os.makedirs(user, exist_ok=True)
with open(os.path.join(user, "usercustomize.py"), "w") as f:
    f.write("import sys; sys.leak_usercustomize = True\n")
with open(os.path.join(user, "leak.pth"), "w") as f:
    f.write("import sys; sys.leak_pth = True\n")
for rc in (".bashrc", ".profile"):
    with open(os.path.expanduser("~/" + rc), "a") as f:
        f.write("export LEAK_RC=1\n")
`

const hooksLoaded = `import sys; print(hasattr(sys, "leak_usercustomize"), hasattr(sys, "leak_pth"))`

func leakL2BindMountFiles(t *testing.T, b leakBackend, m leakMember) {
	leakFilesRow(t, m, b.binds, func(t *testing.T) {
		mustExec(t, m.backend, m.h, "/usr/bin/python3", "-c", userHooks)
		if got := strings.TrimSpace(mustExec(t, m.backend, m.h, "/usr/bin/python3", "-c", hooksLoaded)); got != "True True" {
			t.Fatalf("planted hooks did not load before the reset (%q), so this row proves nothing", got)
		}
	})
	if got := strings.TrimSpace(mustExec(t, m.backend, m.h, "/usr/bin/python3", "-c", hooksLoaded)); got != "False False" {
		t.Fatalf("run A's code ran in run B: usercustomize and .pth loaded = %q", got)
	}
}

// voraAttempts tries everything a run could do to /vora and reports nothing: each backend
// decides what is possible, and the check afterwards is that none of it stayed.
const voraAttempts = `
for action in "touch /vora/leak" "mkdir /vora/leakdir" "cp /bin/sh /vora/vora-init" "rm /vora/vora-init" "mv /vora/vora-init /vora/moved"; do
	$action 2>/dev/null || true
done
exit 0
`

func leakL3Vora(t *testing.T, _ leakBackend, m leakMember) {
	const hashInit = `import hashlib; print(hashlib.sha256(open("/vora/vora-init", "rb").read()).hexdigest())`
	dirs := []string{"/vora"}
	before := snapshot(t, m.backend, m.h, dirs)
	hash := mustExec(t, m.backend, m.h, "/usr/bin/python3", "-c", hashInit)
	mustExec(t, m.backend, m.h, "/bin/sh", "-c", voraAttempts)
	m.mustReset(t)
	if got := snapshot(t, m.backend, m.h, dirs); got != before {
		t.Fatalf("/vora differs after the reset:\n%s", lineDiff(before, got))
	}
	if got := mustExec(t, m.backend, m.h, "/usr/bin/python3", "-c", hashInit); got != hash {
		t.Fatalf("/vora/vora-init changed: %s, was %s", got, hash)
	}
}

func leakL4Env(t *testing.T, _ leakBackend, m leakMember) {
	const secret = "leak-secret-value"
	out, _, code := execSandbox(t, m.backend, m.h, runtime.ExecRequest{
		Command: "/bin/sh",
		Args:    []string{"-c", "export LEAK_EXPORTED=" + secret + "; echo \"$LEAK_REQUEST $LEAK_EXPORTED\""},
		Env:     map[string]string{"LEAK_REQUEST": secret},
	})
	if code != 0 || out != secret+" "+secret+"\n" {
		t.Fatalf("run A did not hold the values (exit %d): %q", code, out)
	}
	m.mustReset(t)
	out = mustExec(t, m.backend, m.h, "/bin/sh", "-c", "env; cat /proc/self/environ | tr '\\0' '\\n'; cat /proc/1/environ 2>/dev/null | tr '\\0' '\\n'; exit 0")
	if strings.Contains(out, secret) || strings.Contains(out, "LEAK_") {
		t.Fatalf("run B sees run A's environment:\n%s", out)
	}
}

// survivors starts one process per way of getting away from the exec that started it.
// Each writes its beat file again and again, so one left alive puts a file back after the
// reset removed it. stdout is closed everywhere, or the exec would not end.
const survivors = `
beat='while :; do : > /tmp/beat-$0; /bin/sleep 0.05; done'
/bin/sh -c "$beat" background </dev/null >/dev/null 2>&1 &
setsid /bin/sh -c "$beat" setsid </dev/null >/dev/null 2>&1 &
nohup /bin/sh -c "$beat" nohup </dev/null >/dev/null 2>&1 &
( /bin/sh -c "$beat" subshell </dev/null >/dev/null 2>&1 & )
exec /usr/bin/python3 -c '
import ctypes, os, signal, sys, threading, time
for fd in (0, 1, 2):
    os.close(fd)
os.open("/dev/null", os.O_RDWR); os.dup2(0, 1); os.dup2(0, 2)

def beat(name):
    while True:
        open("/tmp/beat-" + name, "w").close()
        time.sleep(0.05)

def spawn(name, setup=lambda: None):
    if os.fork() == 0:
        os.setsid()
        if os.fork() != 0:
            os._exit(0)
        setup()
        beat(name)

spawn("double-fork")
spawn("ignores-signals", lambda: [signal.signal(s, signal.SIG_IGN) for s in (signal.SIGTERM, signal.SIGINT, signal.SIGHUP)])

def threads():
    for _ in range(16):
        threading.Thread(target=time.sleep, args=(300,), daemon=True).start()
spawn("threads", threads)

def nested():
    libc = ctypes.CDLL(None, use_errno=True)
    # A filter may refuse these. The row then still covers the rest.
    if libc.unshare(0x10000000) == 0 and libc.unshare(0x20000000) == 0 and os.fork() != 0:
        time.sleep(300)
spawn("nested-pid-namespace", nested)

if os.fork() == 0:
    os.setsid()
    signal.signal(signal.SIGCONT, signal.SIG_IGN)
    os.kill(os.getpid(), signal.SIGSTOP)
    time.sleep(300)
os._exit(0)
'
`

var survivorNames = []string{"background", "setsid", "nohup", "subshell", "double-fork", "ignores-signals", "threads", "nested-pid-namespace"}

func leakL5BackgroundProcesses(t *testing.T, _ leakBackend, m leakMember) {
	baseline := processes(t, m.backend, m.h)
	mustExec(t, m.backend, m.h, "/bin/sh", "-c", survivors)
	for _, name := range survivorNames {
		waitForOutput(t, func() string {
			return mustExec(t, m.backend, m.h, "/bin/sh", "-c", "ls /tmp")
		}, "beat-"+name)
	}
	if got := processes(t, m.backend, m.h); len(got) <= len(baseline) {
		t.Fatalf("no process stayed: %v, baseline %v", got, baseline)
	}

	report := m.mustReset(t)
	if report.ProcessesKilled == 0 {
		t.Errorf("report %+v, want the survivors counted", report)
	}
	time.Sleep(300 * time.Millisecond)
	if got := processes(t, m.backend, m.h); !slices.Equal(got, baseline) {
		t.Errorf("run B sees processes %v, want %v", got, baseline)
	}
	if out := mustExec(t, m.backend, m.h, "/bin/sh", "-c", "ls -A /tmp"); strings.Contains(out, "beat-") {
		t.Errorf("a survivor is still writing: /tmp holds\n%s", out)
	}
}

// holdStdout ends at once and leaves a process that keeps its stdout open.
const holdStdout = `/bin/sleep 300 & echo started`

// A process holding the stdout of an exec keeps that exec open. The reset's kill is what
// frees it.
func leakL6HolderOfStdout(t *testing.T, _ leakBackend, m leakMember) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	stream, err := m.backend.Exec(ctx, m.h, runtime.ExecRequest{ExecutionID: "exe_holder", Command: "/bin/sh", Args: []string{"-c", holdStdout}, Cwd: "/workspace"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stream.Close() }()
	first := make(chan string, 1)
	ended := make(chan error, 1)
	go func() {
		sent := false
		for {
			chunk, err := stream.Recv()
			if err != nil {
				ended <- err
				return
			}
			if len(chunk.Stdout) > 0 && !sent {
				sent = true
				first <- string(chunk.Stdout)
			}
		}
	}()
	select {
	case got := <-first:
		if !strings.Contains(got, "started") {
			t.Fatalf("first output %q", got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the exec printed nothing")
	}
	select {
	case err := <-ended:
		t.Fatalf("the stream ended (%v) with a process still holding stdout, so this row proves nothing", err)
	case <-time.After(300 * time.Millisecond):
	}

	m.mustReset(t)

	select {
	case err := <-ended:
		if !errors.Is(err, io.EOF) {
			t.Fatalf("stream ended with %v, want EOF", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the reset killed the holder but the exec's stream stayed open")
	}
}

// A holder must not stretch an exec past its timeout, and the sandbox must still reset.
func leakL6HolderPastATimeout(t *testing.T, _ leakBackend, m leakMember) {
	const timeout, grace = time.Second, 5 * time.Second
	start := time.Now()
	stdout, _, _ := execSandbox(t, m.backend, m.h, runtime.ExecRequest{Command: "/bin/sh", Args: []string{"-c", holdStdout}, Timeout: timeout})
	if took := time.Since(start); took > timeout+grace {
		t.Errorf("exec took %v with a %v timeout", took, timeout)
	}
	if !strings.Contains(stdout, "started") {
		t.Errorf("stdout %q, want the output from before the timeout", stdout)
	}
	baseline := processes(t, m.backend, m.h)
	if len(baseline) == 0 {
		t.Fatal("no processes listed")
	}
	report := m.mustReset(t)
	if report.ProcessesKilled == 0 {
		t.Errorf("report %+v, want the holder counted", report)
	}
	for _, pid := range processes(t, m.backend, m.h) {
		if !slices.Contains(baseline, pid) {
			t.Errorf("new process %d after the reset", pid)
		}
	}
	if len(processes(t, m.backend, m.h)) >= len(baseline) {
		t.Errorf("the holder is still there: %v", processes(t, m.backend, m.h))
	}
}

// leakPoolSandboxes is the part of the runner a pool needs.
type leakPoolSandboxes struct {
	t *testing.T
	b leakBackend

	mu        sync.Mutex
	members   map[string]leakMember
	destroyed map[string]bool
}

func (s *leakPoolSandboxes) Start(_ context.Context, _ runtime.SandboxSpec, prepare bool) (pool.Started, error) {
	if !prepare {
		return pool.Started{}, errors.New("a resetting pool must prepare its members")
	}
	m, err := s.b.newMember(s.t)
	if err != nil {
		return pool.Started{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.members[m.h.ID()] = m
	return pool.Started{SandboxID: m.h.ID(), Reset: m.reset}, nil
}

func (s *leakPoolSandboxes) Destroy(ctx context.Context, id string) error {
	s.mu.Lock()
	m, ok := s.members[id]
	s.destroyed[id] = true
	s.mu.Unlock()
	if !ok {
		return fmt.Errorf("unknown member %s", id)
	}
	return m.backend.Destroy(ctx, m.h)
}

func (s *leakPoolSandboxes) Event(string, string, string) {}

func (s *leakPoolSandboxes) member(id string) leakMember {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.members[id]
}

func (s *leakPoolSandboxes) isDestroyed(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.destroyed[id]
}

// A reset that fails must end the member for good, through the real pool: the run after a
// failure lands on a different sandbox, and that one has never seen what the failed one held.
func leakL9FailedResetNeverReturns(t *testing.T, b leakBackend) {
	always := func(string) bool { return true }
	size, maxSize := 1, 1
	spec, err := pool.Parse(pool.RawSpec{
		ID:       "pool_leak",
		Name:     "leak",
		TenantID: "t1",
		AgentID:  "a1",
		Size:     &size,
		MaxSize:  &maxSize,
		Template: runtime.SandboxSpec{
			Backend:   "leak-matrix",
			Image:     "base",
			Resources: runtime.ResourceLimits{CPUMillis: 1000, MemoryBytes: 256 << 20, Pids: 64},
			Network:   runtime.NetworkPolicy{Mode: runtime.NetworkNone},
		},
	}, pool.Limits{MaxSize: 4, MaxTTL: time.Hour, MaxMemoryBytes: 1 << 30, MaxCPUMillis: 4000, MaxPids: 1024, MaxDiskBytes: 1 << 40, Reusable: always, HasBackend: always})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	sb := &leakPoolSandboxes{t: t, b: b, members: map[string]leakMember{}, destroyed: map[string]bool{}}
	p := pool.New(spec, sb, pool.Options{})
	defer p.Drain(context.Background())

	run := func(script string) (string, pool.Done) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		var id string
		done, err := p.Run(ctx, func(_ context.Context, pl pool.Placement) error {
			id = pl.SandboxID
			m := sb.member(id)
			mustExec(t, m.backend, m.h, "/bin/sh", "-c", script)
			return nil
		})
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		return id, done
	}

	first, done := run(": > /tmp/ok")
	if done.Reset != pool.ResetOK {
		t.Fatalf("a clean run's reset: %+v", done)
	}
	if again, done := run(": > /tmp/ok"); again != first || done.Reset != pool.ResetOK {
		t.Fatalf("a clean member was not reused: %s then %s, %+v, so this row proves nothing", first, again, done)
	}

	// A listed file changing is what a reset refuses to repair.
	failed, done := run("echo run-a >> /workspace/" + keepFile)
	if failed != first {
		t.Fatalf("the failing run landed on %s, want %s", failed, first)
	}
	if done.Reset != pool.ResetFailed || done.Cause != pool.RetireResetFailed {
		t.Fatalf("after a failed reset: %+v, want failed and retired", done)
	}

	for range 3 {
		id, done := run("cat /workspace/" + keepFile + " > /tmp/seen; test ! -s /tmp/seen")
		if id == failed {
			t.Fatalf("member %s came back after its reset failed", failed)
		}
		if done.Reset != pool.ResetOK {
			t.Fatalf("the replacement's reset: %+v", done)
		}
	}
	if !sb.isDestroyed(failed) {
		t.Errorf("member %s was not destroyed", failed)
	}
	if got := p.Stats(); got.ResetFailuresTotal != 1 || got.Idle+got.Busy+got.Resetting != 1 {
		t.Errorf("stats %+v, want one failed reset and one live member", got)
	}
}

func seedKeepFile(ws runtime.WorkspaceSpec) error {
	return os.WriteFile(filepath.Join(ws.WorkspaceDir, keepFile), nil, 0o644)
}
