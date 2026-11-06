//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	goruntime "runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/vorash/vora/internal/cgroup"
	"github.com/vorash/vora/internal/config"
	"github.com/vorash/vora/internal/network"
	"github.com/vorash/vora/internal/runtime"
	"github.com/vorash/vora/internal/runtime/gvisor"
	"github.com/vorash/vora/internal/sandboxreset"
	"github.com/vorash/vora/internal/workspace"
)

// gvisorMember is a started gVisor sandbox with the means to reach around the backend,
// which only a test has reason to do.
type gvisorMember struct {
	leakMember
	root  string
	id    string
	group string
	ws    runtime.WorkspaceSpec
}

type gvisorMemberOpts struct {
	pids, memory, cpuMillis int64
	noDenyIPC               bool
	restricted              bool
	seed                    func(runtime.WorkspaceSpec) error
	skipPrepare             bool
}

func requireGvisor(t *testing.T) {
	t.Helper()
	if os.Getuid() != 0 {
		t.Skip("gVisor Method 2 requires a rootlesskit userns or root (run under rootlesskit)")
	}
	if _, err := os.Stat("/dev/kvm"); err != nil {
		t.Skipf("/dev/kvm unavailable: %v", err)
	}
}

// newGvisorMember returns an error rather than failing the test, because the pool's
// goroutines call it through leakBackend.
func newGvisorMember(t *testing.T, o gvisorMemberOpts) (gvisorMember, error) {
	t.Helper()
	if o.pids == 0 {
		o.pids = 64
	}
	if o.memory == 0 {
		o.memory = 512 << 20
	}
	if o.cpuMillis == 0 {
		o.cpuMillis = 1000
	}
	cfg := config.Config{StateDir: t.TempDir(), GvisorPlatform: "kvm", GvisorRunRoot: filepath.Join(os.Getenv("XDG_RUNTIME_DIR"), "vora-gvisor-reuse")}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	subtree, err := cgroup.NewSubtree()
	if err != nil {
		return gvisorMember{}, err
	}
	backend, err := gvisor.New(cfg, logger, subtree)
	if err != nil {
		return gvisorMember{}, err
	}
	netPolicy := runtime.NetworkPolicy{Mode: runtime.NetworkNone}
	if o.restricted {
		backend.EnableNetns(network.NewManager(cfg.StateDir, network.ManagerConfig{ProxyPort: network.DefaultProxyPort, BrokerPort: cfg.EgressBrokerPort}, logger), nil)
		netPolicy = runtime.NetworkPolicy{Mode: runtime.NetworkRestricted, AllowedDomains: []string{"example.com"}}
	}

	id := "sbx_" + ulid.Make().String()
	ws := workspace.NewManager(cfg.SandboxesDir())
	wsp := ws.Paths(id)
	wsp.RootfsDir = imageRootfs(t, "base")
	if _, err := ws.Create(wsp); err != nil {
		return gvisorMember{}, fmt.Errorf("workspace: %w", err)
	}
	t.Cleanup(func() { _ = ws.Destroy(id) })
	if o.seed != nil {
		if err := o.seed(wsp); err != nil {
			return gvisorMember{}, fmt.Errorf("seed: %w", err)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	h, err := backend.Create(ctx, runtime.SandboxSpec{
		ID:        id,
		Image:     "base",
		Resources: runtime.ResourceLimits{CPUMillis: o.cpuMillis, MemoryBytes: o.memory, Pids: o.pids},
		Network:   netPolicy,
		TTL:       time.Hour,
		Workspace: wsp,
		DenyIPC:   !o.noDenyIPC,
	})
	if err != nil {
		return gvisorMember{}, fmt.Errorf("Create: %w", err)
	}
	t.Cleanup(func() { _ = backend.Destroy(context.Background(), h) })
	if err := backend.Start(ctx, h); err != nil {
		return gvisorMember{}, fmt.Errorf("Start: %w", err)
	}
	group, err := subtree.Attach(ctx, id)
	if err != nil {
		return gvisorMember{}, fmt.Errorf("cgroup: %w", err)
	}
	m := gvisorMember{
		leakMember: leakMember{backend: backend, h: h},
		root:       cfg.GvisorRunRoot,
		id:         id,
		group:      group.Path(),
		ws:         wsp,
	}
	if o.skipPrepare {
		return m, nil
	}
	if m.reset, err = backend.Prepare(ctx, h); err != nil {
		return gvisorMember{}, fmt.Errorf("Prepare: %w", err)
	}
	return m, nil
}

func mustGvisorMember(t *testing.T, o gvisorMemberOpts) gvisorMember {
	t.Helper()
	m, err := newGvisorMember(t, o)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func (m gvisorMember) cgroupFile(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(m.group, name))
	if err != nil {
		return "unreadable: " + err.Error()
	}
	return strings.ReplaceAll(strings.TrimSpace(string(data)), "\n", " ")
}

// runsc calls the real binary with the flags the backend uses, for what a test does around
// the backend: timing one phase, and finding the Sentry.
func (m gvisorMember) runsc(stdin []byte, args ...string) (string, time.Duration, error) {
	all := append([]string{"--root", m.root, "--platform", "kvm", "--network", "none", "--ignore-cgroups"}, args...)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "runsc", all...)
	if stdin != nil {
		cmd.Stdin = strings.NewReader(string(stdin))
	}
	start := time.Now()
	out, err := cmd.CombinedOutput()
	return string(out), time.Since(start), err
}

func (m gvisorMember) sentryPID(t *testing.T) int {
	t.Helper()
	out, _, err := m.runsc(nil, "state", "--", m.id)
	if err != nil {
		t.Fatalf("runsc state: %v: %s", err, out)
	}
	var state struct {
		PID int `json:"pid"`
	}
	if err := json.Unmarshal([]byte(out), &state); err != nil || state.PID <= 0 {
		t.Fatalf("runsc state printed %q: %v", out, err)
	}
	return state.PID
}

// leakSandbox adapts a member to the leak matrix.
func (m gvisorMember) leakSandbox() leakSandbox {
	return m.sandbox([]string{"/tmp", "/dev", "/dev/shm", "/workspace", "/root"}, []int{1})
}

func TestLeakMatrixGvisor(t *testing.T) {
	requireGvisor(t)
	for name, o := range map[string]gvisorMemberOpts{"none": {seed: seedKeepFile}, "restricted": {seed: seedKeepFile, restricted: true}} {
		t.Run(name, func(t *testing.T) {
			testLeakMatrix(t, mustGvisorMember(t, o).leakSandbox())
			testLeakFailedResetNeverReturns(t, func(t *testing.T) (leakMember, error) {
				m, err := newGvisorMember(t, o)
				return m.leakMember, err
			})
		})
	}
}

// A sandbox that does not deny IPC cannot be proven clean of it, but Reset's own path must
// not depend on the filter either way.
func TestGvisorResetWithoutDenyIPC(t *testing.T) {
	requireGvisor(t)
	leakL1ScratchFiles(t, mustGvisorMember(t, gvisorMemberOpts{noDenyIPC: true}).leakSandbox())
}

// The denial has to hold through the helper path, and the helper has to work under it.
func TestGvisorDenyIPCAcrossReset(t *testing.T) {
	requireGvisor(t)
	m := mustGvisorMember(t, gvisorMemberOpts{})
	testIPCDenied(t, ipcSandbox{
		backend: m.backend,
		h:       m.h,
		capture: func(*testing.T) []byte { return nil },
		tryReset: func([]byte) error {
			_, err := m.tryReset()
			return err
		},
	})
}

func TestGvisorPrepareRefusesASandboxThatAlreadyRanSomething(t *testing.T) {
	requireGvisor(t)
	m := mustGvisorMember(t, gvisorMemberOpts{skipPrepare: true})
	mustExec(t, m.backend, m.h, "/bin/sh", "-c", "/bin/sleep 60 </dev/null >/dev/null 2>&1 &")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := m.backend.(runtime.Reusable).Prepare(ctx, m.h)
	if err == nil || !strings.Contains(err.Error(), "before capture") {
		t.Fatalf("Prepare = %v, want a refusal naming the running process", err)
	}
}

func seedChangeTargets(ws runtime.WorkspaceSpec) error {
	for _, dir := range []string{ws.WorkspaceDir, ws.HomeDir} {
		if err := os.MkdirAll(filepath.Join(dir, "keepdir"), 0o755); err != nil {
			return err
		}
		for _, name := range []string{"keep", "keepdir/inner"} {
			if err := os.WriteFile(filepath.Join(dir, name), []byte("seeded\n"), 0o644); err != nil {
				return err
			}
		}
	}
	return nil
}

// Each of these leaves a listed entry different in a way a next run could read, or could
// only happen to one that was touched, on gVisor's 9p mounts, where a value that did not
// come through would pass unnoticed. Restoring a mode or a time by hand moves ctime.
func TestGvisorResetDetectsChanges(t *testing.T) {
	requireGvisor(t)
	const putBack = `
import os, sys
path = sys.argv[1]
st = os.lstat(path)
`
	for _, tc := range []struct {
		name, script string
		wantInError  string
	}{
		{"mode put back", `os.chmod(path, 0o600); os.chmod(path, st.st_mode)`, "keep"},
		{"times put back", `os.utime(path, ns=(1000, 1000)); os.utime(path, ns=(st.st_atime_ns, st.st_mtime_ns))`, "keep"},
		{"xattr set", `os.setxattr(path, "user.vora", b"run")`, "keep"},
		{"xattr set and removed", `os.setxattr(path, "user.vora", b"run"); os.removexattr(path, "user.vora")`, "keep"},
		{"content, same size", `open(path, "w").write("SEEDED\n")`, "keep"},
		{"content put back", `open(path, "w").write("seeded\n")`, "keep"},
		{"replaced by a copy", `open(path + ".new", "w").write("seeded\n"); os.rename(path + ".new", path)`, "keep"},
		{"hard link planted", `os.link(path, path + ".link")`, "keep"},
		{"removed", `os.remove(path)`, "keep"},
		{"directory xattr", `os.setxattr(os.path.dirname(path) + "/keepdir", "user.vora", b"run")`, "keepdir"},
		{"directory mode", `os.chmod(os.path.dirname(path) + "/keepdir", 0o700)`, "keepdir"},
	} {
		for _, top := range []string{"/workspace", "/root"} {
			t.Run(tc.name+" in "+top, func(t *testing.T) {
				m := mustGvisorMember(t, gvisorMemberOpts{seed: seedChangeTargets})
				mustExec(t, m.backend, m.h, "/usr/bin/python3", "-c", putBack+tc.script, top+"/keep")
				_, err := m.tryReset()
				if !errors.Is(err, runtime.ErrUnproven) {
					t.Fatalf("Reset = %v, want ErrUnproven", err)
				}
				if !strings.Contains(err.Error(), tc.wantInError) {
					t.Fatalf("Reset = %v, want it to name %s", err, tc.wantInError)
				}
			})
		}
	}
}

// Reads, a planted file and a removed one must not cost a member, or every run would.
func TestGvisorResetAcceptsWhatARunMayDo(t *testing.T) {
	requireGvisor(t)
	m := mustGvisorMember(t, gvisorMemberOpts{seed: seedChangeTargets})
	dirs := []string{"/workspace", "/root"}
	before := snapshot(t, m.backend, m.h, dirs)
	for range 5 {
		mustExec(t, m.backend, m.h, "/bin/sh", "-c", `
cat /workspace/keep /root/keepdir/inner >/dev/null
ls -laR /workspace /root >/dev/null
echo run > /workspace/keepdir/planted
mkdir -p /root/keepdir/new/deep && echo run > /root/keepdir/new/deep/file
`)
		m.mustReset(t)
	}
	if got := snapshot(t, m.backend, m.h, dirs); got != before {
		t.Fatalf("a next run reads something else:\n%s", lineDiff(before, got))
	}
}

// entryFields compares what a reset compares, and names each so a drift says what moved.
var entryFields = []struct {
	name string
	get  func(sandboxreset.Entry) any
}{
	{"type", func(e sandboxreset.Entry) any { return e.Type }},
	{"mode", func(e sandboxreset.Entry) any { return e.Mode }},
	{"uid", func(e sandboxreset.Entry) any { return e.UID }},
	{"gid", func(e sandboxreset.Entry) any { return e.GID }},
	{"ino", func(e sandboxreset.Entry) any { return e.Ino }},
	{"rdev", func(e sandboxreset.Entry) any { return e.Rdev }},
	{"size", func(e sandboxreset.Entry) any { return e.Size }},
	{"sha256", func(e sandboxreset.Entry) any { return e.SHA256 }},
	{"target", func(e sandboxreset.Entry) any { return e.Target }},
	{"xattrs", func(e sandboxreset.Entry) any { return e.Xattrs }},
	{"ctime", func(e sandboxreset.Entry) any { return e.CtimeNs }},
	{"mtime", func(e sandboxreset.Entry) any { return e.MtimeNs }},
	{"atime", func(e sandboxreset.Entry) any { return e.AtimeNs }},
}

// countDrift counts, per field, the entries of each mount that differ between two captures.
func countDrift(a, b sandboxreset.Manifest, drift map[string]int) (entries int) {
	for _, la := range a.Mounts {
		i := slices.IndexFunc(b.Mounts, func(l sandboxreset.MountListing) bool { return l.Path == la.Path })
		if i < 0 {
			drift[la.Path+" missing"]++
			continue
		}
		for _, ea := range la.Entries {
			j := slices.IndexFunc(b.Mounts[i].Entries, func(e sandboxreset.Entry) bool { return e.Rel == ea.Rel })
			if j < 0 {
				drift[la.Path+" entry missing"]++
				continue
			}
			entries++
			for _, f := range entryFields {
				if f.get(ea) != f.get(b.Mounts[i].Entries[j]) {
					drift[la.Path+" "+f.name]++
				}
			}
		}
	}
	return entries
}

// The 9p mounts carry dcache=1000, so a walk over more entries than that evicts the
// Sentry's dentries, and they come back from the host. If a value in a listed entry moved
// by itself, a member would fail a reset nothing was wrong with. Every cycle here walks
// past that size, and every few cycles a run plants past it too.
func TestGvisor9pStability(t *testing.T) {
	requireGvisor(t)
	const seeded, cycles, captureEvery, plantEvery = 2500, 200, 20, 5
	seed := func(ws runtime.WorkspaceSpec) error {
		for _, dir := range []string{ws.WorkspaceDir, ws.HomeDir} {
			for i := range seeded {
				sub := filepath.Join(dir, "seed", strconv.Itoa(i%50))
				if err := os.MkdirAll(sub, 0o755); err != nil {
					return err
				}
				if err := os.WriteFile(filepath.Join(sub, strconv.Itoa(i)), []byte(strconv.Itoa(i)), 0o644); err != nil {
					return err
				}
			}
		}
		return nil
	}
	m := mustGvisorMember(t, gvisorMemberOpts{seed: seed, memory: 1 << 30})
	first := decodeManifest(t, []byte(mustExec(t, m.backend, m.h, "/vora/vora-init", "capture")))
	mounts := regexp.MustCompile(`dcache=\d+`).FindAllString(mustExec(t, m.backend, m.h, "/bin/cat", "/proc/self/mountinfo"), -1)
	t.Logf("mount options: %v", mounts)

	drift := map[string]int{}
	var compared, entries int
	var resetTimes []time.Duration
	for i := range cycles {
		if i%plantEvery == 0 {
			mustExec(t, m.backend, m.h, "/usr/bin/python3", "-c", `
import os
os.makedirs("/workspace/evict")
for i in range(1500):
    with open("/workspace/evict/%d" % i, "w") as f:
        f.write("x")
    os.stat("/workspace/evict/%d" % i)
`)
		} else {
			mustExec(t, m.backend, m.h, "/bin/sh", "-c", "cat /workspace/seed/0/0 /root/seed/1/1 >/dev/null; touch /tmp/run")
		}
		start := time.Now()
		if _, err := m.tryReset(); err != nil {
			t.Fatalf("cycle %d of %d: a clean member failed: %v", i+1, cycles, err)
		}
		resetTimes = append(resetTimes, time.Since(start))
		if i%captureEvery == captureEvery-1 {
			again := decodeManifest(t, []byte(mustExec(t, m.backend, m.h, "/vora/vora-init", "capture")))
			entries = countDrift(first, again, drift)
			compared++
		}
	}
	t.Logf("%d resets over %d seeded files in each of 2 bind mounts: reset %s", cycles, seeded, describeLatency(resetTimes))
	t.Logf("%d captures of %d entries compared with the first. Differences: %v", compared, entries, drift)
	for name, n := range drift {
		// A directory's atime moves when something reads it, and the reset writes it back
		// from the capture: a capture that ran right after a walk may see the walk.
		if !strings.HasSuffix(name, " atime") {
			t.Errorf("%s drifted in %d entries over %d captures", name, n, compared)
		}
	}
}

// The numbers the pool experiment starts from, on an otherwise idle member.
func TestGvisorResetCost(t *testing.T) {
	requireGvisor(t)
	var creates, prepares, destroys []time.Duration
	for range 15 {
		start := time.Now()
		m := mustGvisorMember(t, gvisorMemberOpts{skipPrepare: true, seed: seedKeepFile})
		creates = append(creates, time.Since(start))
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		start = time.Now()
		if _, err := m.backend.(runtime.Reusable).Prepare(ctx, m.h); err != nil {
			cancel()
			t.Fatal(err)
		}
		prepares = append(prepares, time.Since(start))
		start = time.Now()
		if err := m.backend.Destroy(ctx, m.h); err != nil {
			t.Error(err)
		}
		destroys = append(destroys, time.Since(start))
		cancel()
	}
	t.Logf("start (workspace, Create, Start) %s", describeLatency(creates))
	t.Logf("Prepare %s", describeLatency(prepares))
	t.Logf("Destroy %s", describeLatency(destroys))

	for _, cpu := range []int64{1000, 4000} {
		for _, load := range []struct {
			name, command string
			args          []string
			runs          int
			wantKilled    int
			wantRemoved   bool
		}{
			{name: "nothing left behind", command: "/bin/true", runs: 300},
			{name: "files left behind", command: "/bin/sh", args: []string{"-c", `for i in 1 2 3 4 5 6 7 8 9 10; do echo x > /tmp/f$i; echo x > /workspace/f$i; done`}, runs: 250, wantRemoved: true},
			{name: "five processes left behind", command: "/bin/sh", args: []string{"-c", `for i in 1 2 3 4 5; do /bin/sleep 300 </dev/null >/dev/null 2>&1 & done`}, runs: 100, wantKilled: 5},
		} {
			m := mustGvisorMember(t, gvisorMemberOpts{seed: seedKeepFile, cpuMillis: cpu})
			var took []time.Duration
			throttledBefore := m.cgroupFile(t, "cpu.stat")
			for range load.runs {
				mustExec(t, m.backend, m.h, load.command, load.args...)
				start := time.Now()
				report, err := m.tryReset()
				if err != nil {
					t.Fatal(err)
				}
				took = append(took, time.Since(start))
				if report.ProcessesKilled != load.wantKilled {
					t.Fatalf("report %+v, want %d killed: the timing would be of something else", report, load.wantKilled)
				}
				if load.wantRemoved && report.EntriesRemoved == 0 {
					t.Fatalf("report %+v, want entries removed", report)
				}
			}
			t.Logf("cpu=%dm %s: reset %s", cpu, load.name, describeLatency(took))
			t.Logf("  pids %s peak %s, memory %s peak %s", m.cgroupFile(t, "pids.current"), m.cgroupFile(t, "pids.peak"), m.cgroupFile(t, "memory.current"), m.cgroupFile(t, "memory.peak"))
			t.Logf("  cpu.stat before %s", throttledBefore)
			t.Logf("  cpu.stat after  %s", m.cgroupFile(t, "cpu.stat"))
		}
	}

	m := mustGvisorMember(t, gvisorMemberOpts{seed: seedKeepFile})
	manifest := []byte(mustExec(t, m.backend, m.h, "/vora/vora-init", "capture"))
	phases := []struct {
		name  string
		stdin []byte
		args  []string
	}{
		{name: "runsc help, no sandbox", args: []string{"help"}},
		{name: "runsc ps", args: []string{"ps", "--format", "json", "--", m.id}},
		{name: "runsc exec /bin/true", args: []string{"exec", "-cwd", "/", "--", m.id, "/bin/true"}},
		{name: "runsc exec vora-init reset", stdin: manifest, args: []string{"exec", "-cwd", "/", "--", m.id, "/vora/vora-init", "reset"}},
	}
	took := make([][]time.Duration, len(phases))
	for range 200 {
		for i, p := range phases {
			out, d, err := m.runsc(p.stdin, p.args...)
			if err != nil {
				t.Fatalf("%s: %v: %s", p.name, err, out)
			}
			took[i] = append(took[i], d)
		}
	}
	for i, p := range phases {
		t.Logf("%s: %s", p.name, describeLatency(took[i]))
	}
	t.Logf("manifest %d bytes", len(manifest))
}

// A sandbox over its memory limit dies, because the guest's memory is the Sentry's and the
// host's OOM killer takes the Sentry. A member must not survive that (#120).
func TestGvisorDeadSandbox(t *testing.T) {
	requireGvisor(t)
	const oom = `b = bytearray(1 << 30); [b.__setitem__(i, 1) for i in range(0, len(b), 4096)]`
	kill := func(t *testing.T, m gvisorMember) {
		t.Helper()
		got := lastExecExit(t, m, "/usr/bin/python3", "-c", oom)
		if got.ExitCode == 0 {
			t.Fatal("the program finished, so this test proves nothing")
		}
		if got := m.cgroupFile(t, "memory.events"); !strings.Contains(got, "oom_kill 1") {
			t.Fatalf("memory.events %q, want one OOM kill", got)
		}
		// The exec the sandbox died under says so too, not only the next one.
		if got.Reason != runtime.ReasonRuntimeFailure {
			t.Errorf("the exec that took the sandbox down: %+v, want %s", got, runtime.ReasonRuntimeFailure)
		}
	}

	t.Run("after Prepare", func(t *testing.T) {
		m := mustGvisorMember(t, gvisorMemberOpts{memory: 128 << 20})
		kill(t, m)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := m.reset.Alive(ctx); err == nil {
			t.Error("Alive = nil on a dead sandbox")
		}
		if _, err := m.reset.Reset(ctx); !errors.Is(err, runtime.ErrUnproven) {
			t.Errorf("Reset = %v, want ErrUnproven", err)
		}
	})
	t.Run("before Prepare", func(t *testing.T) {
		m := mustGvisorMember(t, gvisorMemberOpts{memory: 128 << 20, skipPrepare: true})
		kill(t, m)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := m.backend.(runtime.Reusable).Prepare(ctx, m.h); err == nil {
			t.Error("Prepare = nil on a dead sandbox")
		}
	})
	// A dead sandbox says so at Exec, where a later exec used to read as the program exiting
	// 128. A pool retries the run on another member on this error.
	t.Run("later exec", func(t *testing.T) {
		m := mustGvisorMember(t, gvisorMemberOpts{memory: 128 << 20})
		kill(t, m)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		stream, err := m.backend.Exec(ctx, m.h, runtime.ExecRequest{Command: "/bin/echo", Args: []string{"hi"}, Cwd: "/workspace"})
		if err == nil {
			_ = stream.Close()
			t.Error("Exec in a dead sandbox returned a stream")
		}
	})
	t.Run("Sentry killed mid-exec", func(t *testing.T) {
		m := mustGvisorMember(t, gvisorMemberOpts{})
		pid := m.sentryPID(t)
		time.AfterFunc(300*time.Millisecond, func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
		got := lastExecExit(t, m, "/bin/sleep", "60")
		if got.ExitCode != 128 || got.Reason != runtime.ReasonRuntimeFailure {
			t.Errorf("an exec whose Sentry died: %+v, want 128 and %s", got, runtime.ReasonRuntimeFailure)
		}
	})
	// The control: a program may really exit 128.
	t.Run("a program exiting 128", func(t *testing.T) {
		m := mustGvisorMember(t, gvisorMemberOpts{})
		got := lastExecExit(t, m, "/bin/sh", "-c", "exit 128")
		if got.Reason != runtime.ReasonExited || got.ExitCode != 128 {
			t.Errorf("exit 128: %+v, want %s", got, runtime.ReasonExited)
		}
		if _, err := m.tryReset(); err != nil {
			t.Errorf("Reset after a program's own exit 128: %v", err)
		}
	})
}

func lastExecExit(t *testing.T, m gvisorMember, command string, args ...string) runtime.ExecResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	stream, err := m.backend.Exec(ctx, m.h, runtime.ExecRequest{Command: command, Args: args, Cwd: "/workspace"})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	defer func() { _ = stream.Close() }()
	for {
		chunk, err := stream.Recv()
		if err != nil {
			t.Fatalf("stream ended without an exit: %v", err)
		}
		if chunk.Exit != nil {
			return *chunk.Exit
		}
	}
}

// A stopped Sentry hangs every runsc subcommand. The deadline must hold regardless, or the
// pool waits on a member it should have destroyed.
func TestGvisorStuckSentryHoldsTheDeadline(t *testing.T) {
	requireGvisor(t)
	const deadline, grace = 500 * time.Millisecond, 200 * time.Millisecond
	m := mustGvisorMember(t, gvisorMemberOpts{})
	pid := m.sentryPID(t)
	if err := syscall.Kill(pid, syscall.SIGSTOP); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = syscall.Kill(pid, syscall.SIGCONT) }()

	ctx, cancel := context.WithTimeout(context.Background(), deadline)
	defer cancel()
	start := time.Now()
	err := m.reset.Alive(ctx)
	took := time.Since(start)
	if err == nil || took > deadline+grace {
		t.Errorf("Alive on a stopped Sentry: %v after %v", err, took)
	}

	ctx, cancel = context.WithTimeout(context.Background(), deadline)
	defer cancel()
	start = time.Now()
	_, err = m.reset.Reset(ctx)
	took = time.Since(start)
	if !errors.Is(err, runtime.ErrUnproven) || took > deadline+grace {
		t.Errorf("Reset on a stopped Sentry: %v after %v", err, took)
	}
	t.Logf("Reset returned %v past its deadline", took-deadline)
}

// The pool gives up on a slow Reset and destroys the member while the call still runs.
func TestGvisorDestroyDuringReset(t *testing.T) {
	requireGvisor(t)
	m := mustGvisorMember(t, gvisorMemberOpts{})
	mustExec(t, m.backend, m.h, "/bin/sh", "-c", `for i in 1 2 3 4 5 6 7 8; do /bin/sleep 300 </dev/null >/dev/null 2>&1 & done`)
	done := make(chan error, 1)
	go func() {
		_, err := m.tryReset()
		done <- err
	}()
	time.Sleep(8 * time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := m.backend.Destroy(ctx, m.h); err != nil {
		t.Errorf("Destroy: %v", err)
	}
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, runtime.ErrUnproven) {
			t.Errorf("Reset = %v, want nil or ErrUnproven", err)
		}
	case <-time.After(11 * time.Second):
		t.Fatal("Reset never returned")
	}
	if out, _, err := m.runsc(nil, "state", "--", m.id); err == nil {
		t.Errorf("the sandbox outlived Destroy: %s", out)
	}
}

// burners puts more CPU on a member than cpu=1000m gives it, so the cgroup throttles the
// Sentry and everything it does, control requests included, runs late.
const burners = `for i in 1 2 3 4; do (while :; do :; done) </dev/null >/dev/null 2>&1 & done`

// A client can disconnect while `runsc exec` is mid-start, with the pool resetting at once.
// The jitter puts the cancel across the start: a healthy exec answers in about 9 ms, and in
// about 90 ms on a throttled member. A loss is a nil Reset with anything left over. Losing
// takes a task that lands after the helper's walk, so a run of this rarely catches a backend
// that gets it wrong, and the fake in reuse_test.go is what fixes the order.
func TestGvisorAbandonedExecLeavesNothing(t *testing.T) {
	requireGvisor(t)
	for _, load := range []struct {
		name, burn string
		cycles     int
		jitter     time.Duration
	}{
		{"idle", "", 100, 12 * time.Millisecond},
		{"throttled", burners, 50, 100 * time.Millisecond},
	} {
		t.Run(load.name, func(t *testing.T) {
			m := mustGvisorMember(t, gvisorMemberOpts{})
			var landed int
			var took []time.Duration
			for i := range load.cycles {
				if load.burn != "" {
					mustExec(t, m.backend, m.h, "/bin/sh", "-c", load.burn)
				}
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				stream, err := m.backend.Exec(ctx, m.h, runtime.ExecRequest{Command: "/bin/sh", Args: []string{"-c", ": > /tmp/landed-$$"}, Cwd: "/"})
				if err != nil {
					cancel()
					t.Fatalf("cycle %d: Exec: %v", i, err)
				}
				time.Sleep(time.Duration(rand.Int64N(int64(load.jitter))))
				_ = stream.Close()
				start := time.Now()
				report, err := m.tryReset()
				took = append(took, time.Since(start))
				cancel()
				if err != nil {
					t.Fatalf("cycle %d: a member with a start this short should answer in time: %v", i, err)
				}
				if report.EntriesRemoved > 0 {
					landed++
				}
				if got := mustExec(t, m.backend, m.h, "/bin/ls", "-A", "/tmp"); got != "" {
					t.Fatalf("cycle %d: Reset = nil and the next run finds %q", i, got)
				}
				out, _, err := m.runsc(nil, "ps", "--format", "json", "--", m.id)
				if got := strings.TrimSpace(out); err != nil || got != "[1]" {
					t.Fatalf("cycle %d: Reset = nil and the host lists %q (%v)", i, got, err)
				}
			}
			t.Logf("%d cycles, %d commands wrote before they died, reset %s", load.cycles, landed, describeLatency(took))
			if landed == 0 {
				t.Fatal("no command ever ran, so this test proves nothing")
			}
		})
	}
}

// A client that cannot get an answer is killed once the grace ends, and its request may yet
// be served. A stopped Sentry is the way to make that happen every time.
func TestGvisorAbandonedExecFailsClosed(t *testing.T) {
	requireGvisor(t)
	m := mustGvisorMember(t, gvisorMemberOpts{})
	pid := m.sentryPID(t)
	if err := syscall.Kill(pid, syscall.SIGSTOP); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = syscall.Kill(pid, syscall.SIGCONT) }()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	stream, err := m.backend.Exec(ctx, m.h, runtime.ExecRequest{Command: "/bin/true", Cwd: "/"})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	_ = stream.Close()

	start := time.Now()
	_, err = m.tryReset()
	if !errors.Is(err, runtime.ErrUnproven) || !strings.Contains(err.Error(), "abandoned") {
		t.Fatalf("Reset = %v, want ErrUnproven naming the abandoned exec", err)
	}
	t.Logf("Reset refused after %v", time.Since(start))

	if err := syscall.Kill(pid, syscall.SIGCONT); err != nil {
		t.Fatal(err)
	}
	if _, err := m.tryReset(); !errors.Is(err, runtime.ErrUnproven) || !strings.Contains(err.Error(), "abandoned") {
		t.Fatalf("with the Sentry back, Reset = %v, want it still refused", err)
	}
}

// busyHost loads every host CPU for the rest of the test.
func busyHost(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	for range goruntime.NumCPU() {
		cmd := exec.CommandContext(ctx, "/bin/sh", "-c", "while :; do :; done")
		cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		go func() { _ = cmd.Wait() }()
	}
}

// holdMemory backgrounds a process that keeps sys.argv[1] bytes resident, then marks /tmp.
const holdMemory = `
import os, sys, time
n = int(sys.argv[1])
if os.fork() != 0:
    os._exit(0)
os.setsid()
for fd in (0, 1, 2):
    os.close(fd)
b = bytearray(n)
for i in range(0, n, 4096):
    b[i] = 1
open("/tmp/ready", "w").close()
time.sleep(300)
`

// A process with a lot of memory takes the Sentry a while to tear down, and more on a member
// out of CPU on a busy host. That must not read as a process that survived the kill passes.
func TestGvisorResetOfALargeProcess(t *testing.T) {
	requireGvisor(t)
	busyHost(t)
	m := mustGvisorMember(t, gvisorMemberOpts{cpuMillis: 1000, memory: 4 << 30})
	var took []time.Duration
	for range 3 {
		mustExec(t, m.backend, m.h, "/usr/bin/python3", "-c", holdMemory, strconv.Itoa(2<<30))
		waitForOutput(t, func() string { return mustExec(t, m.backend, m.h, "/bin/ls", "/tmp") }, "ready")
		mustExec(t, m.backend, m.h, "/bin/sh", "-c", burners)
		rss := m.cgroupFile(t, "memory.current")
		start := time.Now()
		report, err := m.tryReset()
		if err != nil {
			t.Fatalf("a large process at memory %s cost the member: %v", rss, err)
		}
		took = append(took, time.Since(start))
		if report.ProcessesKilled != 5 {
			t.Fatalf("report %+v, want the holder and the 4 burners", report)
		}
	}
	t.Logf("reset %s", describeLatency(took))
}

// forkChains keeps moving: each process forks and exits, so a pid is stale by the time the
// host signals it. Guest code runs between a listing and its kills, so this is the workload
// that can outrun the loop. Either the loop catches them all, and the proof must hold, or
// the reset gives up. What may never happen is a nil with a process alive.
const forkChains = `
import os, sys, time
for fd in (0, 1, 2):
    os.close(fd)
for _ in range(int(sys.argv[1])):
    if os.fork() == 0:
        while True:
            if os.fork() != 0:
                os._exit(0)
os._exit(0)
`

func TestGvisorForkChainsFailClosedOrDie(t *testing.T) {
	requireGvisor(t)
	for _, chains := range []int{1, 4, 16, 40} {
		t.Run(strconv.Itoa(chains), func(t *testing.T) {
			m := mustGvisorMember(t, gvisorMemberOpts{memory: 2 << 30, pids: 256})
			_, stderr, code := execSandbox(t, m.backend, m.h, runtime.ExecRequest{Command: "/usr/bin/python3", Args: []string{"-W", "ignore", "-c", forkChains, strconv.Itoa(chains)}})
			before, _, psErr := m.runsc(nil, "ps", "--format", "json", "--", m.id)
			t.Logf("exit %d %.120s; before the reset: %.120s (%v), pids %s", code, stderr, strings.TrimSpace(before), psErr, m.cgroupFile(t, "pids.events"))
			// Long enough for the kill passes to run out before the deadline does.
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			start := time.Now()
			report, err := m.reset.Reset(ctx)
			t.Logf("Reset: %+v after %v: %.200v", report, time.Since(start), err)
			after, _, psErr := m.runsc(nil, "ps", "--format", "json", "--", m.id)
			t.Logf("after the reset: %.120s (%v)", strings.TrimSpace(after), psErr)
			if err == nil {
				if got := strings.TrimSpace(after); got != "[1]" {
					t.Fatalf("Reset = nil with processes %s", got)
				}
				return
			}
			if !errors.Is(err, runtime.ErrUnproven) {
				t.Fatalf("Reset = %v, want ErrUnproven", err)
			}
		})
	}
}

// The helper's proof reads /proc, so a thread listed there would read as another process,
// and every reset of a multi-threaded helper would fail.
func TestGvisorProcListsThreadGroupLeaders(t *testing.T) {
	requireGvisor(t)
	m := mustGvisorMember(t, gvisorMemberOpts{})
	out := mustExec(t, m.backend, m.h, "/usr/bin/python3", "-c", `
import os, threading, time
for _ in range(40):
    threading.Thread(target=time.sleep, args=(5,), daemon=True).start()
print(len(os.listdir("/proc/self/task")), sum(p.isdigit() for p in os.listdir("/proc")), flush=True)
os._exit(0)
`)
	var tasks, listed int
	if _, err := fmt.Sscan(out, &tasks, &listed); err != nil {
		t.Fatalf("printed %q: %v", out, err)
	}
	if tasks <= 40 || listed != 2 {
		t.Fatalf("%d threads and %d entries in /proc, want more than 40 and 2 (init and this one)", tasks, listed)
	}
	t.Logf("%d threads, %d processes in /proc", tasks, listed)
	m.mustReset(t)
}

// ordinaryWork is what a run with some parallelism does: every task is a Sentry goroutine,
// and any of them may need a host thread, which counts against pids.
const ordinaryWork = `
import subprocess, threading
def work():
    subprocess.run(["/bin/sh", "-c", "for i in 1 2 3 4 5 6 7 8; do /bin/true & done; wait"], check=True)
ts = [threading.Thread(target=work) for _ in range(4)]
[t.start() for t in ts]
[t.join() for t in ts]
`

// pids.max under gVisor bounds the Sentry's host threads, not the guest's tasks. A limit
// too low must show as a sandbox that fails to start or dies, never as one that resets
// clean with something wrong. The sizes a pool can use must get through a long run.
func TestGvisorPidsLimits(t *testing.T) {
	requireGvisor(t)
	for _, tc := range []struct {
		pids        int64
		mustSurvive bool
	}{{12, false}, {16, false}, {28, false}, {32, false}, {36, false}, {48, false}, {64, true}, {128, true}} {
		t.Run(fmt.Sprintf("pids=%d", tc.pids), func(t *testing.T) {
			m, err := newGvisorMember(t, gvisorMemberOpts{pids: tc.pids, memory: 1 << 30})
			if err != nil {
				if tc.mustSurvive {
					t.Fatal(err)
				}
				t.Logf("does not start: %v", err)
				return
			}
			idle := m.cgroupFile(t, "pids.current")
			const cycles = 60
			for i := range cycles {
				_, _, code := execSandbox(t, m.backend, m.h, runtime.ExecRequest{Command: "/usr/bin/python3", Args: []string{"-c", ordinaryWork}})
				_, err := m.tryReset()
				if err != nil {
					if !errors.Is(err, runtime.ErrUnproven) {
						t.Fatalf("Reset = %v, want ErrUnproven", err)
					}
					if tc.mustSurvive {
						t.Fatalf("cycle %d: exit %d, %v. pids %s", i, code, err, m.cgroupFile(t, "pids.events"))
					}
					t.Logf("idle %s tasks, died at cycle %d (exit %d): %.160s; %s", idle, i, code, err.Error(), m.cgroupFile(t, "pids.events"))
					return
				}
				if code != 0 && tc.mustSurvive {
					t.Fatalf("cycle %d: the work failed with exit %d", i, code)
				}
			}
			t.Logf("idle %s tasks, %d cycles passed, now %s, peak %s, %s", idle, cycles, m.cgroupFile(t, "pids.current"), m.cgroupFile(t, "pids.peak"), m.cgroupFile(t, "pids.events"))
		})
	}
}

// A guest can hold far more tasks than pids.max, which bounds the Sentry's threads. The
// host's kill needs no slot in the sandbox and takes one runsc per process, so this many
// must still fit the pool's 10 s.
func TestGvisorResetClearsManyProcesses(t *testing.T) {
	requireGvisor(t)
	const bomb = `
i=0
while [ $i -lt 400 ]; do /bin/sleep 300 </dev/null >/dev/null 2>&1 & i=$((i+1)); done
exit 0
`
	m := mustGvisorMember(t, gvisorMemberOpts{memory: 1 << 30})
	mustExec(t, m.backend, m.h, "/bin/sh", "-c", bomb)
	out, _, err := m.runsc(nil, "ps", "--format", "json", "--", m.id)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%d processes, pids %s, memory %s", strings.Count(out, ",")+1, m.cgroupFile(t, "pids.current"), m.cgroupFile(t, "memory.current"))
	start := time.Now()
	report, err := m.tryReset()
	if err != nil {
		t.Fatalf("Reset of 400 idle processes: %v", err)
	}
	t.Logf("%+v in %v", report, time.Since(start))
	if report.ProcessesKilled != 400 {
		t.Errorf("report %+v, want 400 killed", report)
	}
	if got := processes(t, m.backend, m.h); !slices.Equal(got, []int{1}) {
		t.Errorf("processes %v after the reset", got)
	}
}

// The helper is trusted because it is /vora/vora-init, so a workload must not be able to
// put another program at that path for the next `runsc exec`. In the sandbox's own mount
// namespace it lacks the capability, and a namespace of its own reaches only itself. The
// fake would print a valid report and clean nothing, so the planted marker tells them apart.
func TestGvisorWorkloadCannotReplaceTheHelper(t *testing.T) {
	requireGvisor(t)
	const attempt = `
import ctypes, errno, os, sys, time
libc = ctypes.CDLL(None, use_errno=True)
CLONE_NEWNS, CLONE_NEWUSER = 0x00020000, 0x10000000
def mount():
    if libc.mount(b"none", b"/vora", b"tmpfs", 0, None) == 0:
        return "ok"
    return errno.errorcode[ctypes.get_errno()]
print("sandbox namespace", mount(), flush=True)
if libc.unshare(CLONE_NEWUSER | CLONE_NEWNS) != 0:
    print("own namespace", errno.errorcode[ctypes.get_errno()], flush=True)
    sys.exit(0)
print("own namespace", mount(), flush=True)
with open("/vora/vora-init", "w") as f:
    f.write('#!/bin/sh\necho \'{"removed":0}\'\n')
os.chmod("/vora/vora-init", 0o755)
print("fake in place", open("/vora/vora-init").read().startswith("#!"), flush=True)
if os.fork() != 0:
    os._exit(0)
os.setsid()
for fd in (0, 1, 2):
    os.close(fd)
time.sleep(300)
`
	m := mustGvisorMember(t, gvisorMemberOpts{})
	mustExec(t, m.backend, m.h, "/bin/sh", "-c", ": > /tmp/marker")
	out := mustExec(t, m.backend, m.h, "/usr/bin/python3", "-c", attempt)
	t.Log(out)
	if !strings.Contains(out, "sandbox namespace EPERM") {
		t.Fatalf("a workload mounted over /vora in the sandbox's namespace:\n%s", out)
	}
	if !strings.Contains(out, "fake in place True") {
		t.Fatalf("the fake never got in place in the workload's own namespace, so this test proves less than it says:\n%s", out)
	}
	report := m.mustReset(t)
	if report.ProcessesKilled == 0 || report.EntriesRemoved == 0 {
		t.Errorf("report %+v, want the holder of the namespace killed and the marker removed", report)
	}
	if got := mustExec(t, m.backend, m.h, "/bin/sh", "-c", "ls -A /tmp"); got != "" {
		t.Errorf("the marker survived, so something other than the helper answered: %q", got)
	}
}
