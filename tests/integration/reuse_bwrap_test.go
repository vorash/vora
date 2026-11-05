//go:build integration

package integration

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vorash/vora/internal/agentproto"
	"github.com/vorash/vora/internal/cgroup"
	"github.com/vorash/vora/internal/config"
	"github.com/vorash/vora/internal/egress"
	"github.com/vorash/vora/internal/network"
	"github.com/vorash/vora/internal/runtime"
	"github.com/vorash/vora/internal/runtime/bwrap"
	"github.com/vorash/vora/internal/ulid"
	"github.com/vorash/vora/internal/workspace"
)

// resetDeadline is the pool's default ResetTimeout.
const resetDeadline = 10 * time.Second

// bwrapInitPIDs is what a run sees in /proc under bwrap: the reaper and the agent.
var bwrapInitPIDs = []int{1, 2}

// member is a prepared bwrap pool member driven the way the pool drives it.
type member struct {
	backend *bwrap.Backend
	h       runtime.Handle
	ws      runtime.WorkspaceSpec
	r       runtime.Resettable
	// dir is the sandbox's cgroup on the host, for the tests that look at it.
	dir string
}

func (m *member) reset(ctx context.Context) (runtime.ResetReport, error) {
	ctx, cancel := context.WithTimeout(ctx, resetDeadline)
	defer cancel()
	return m.r.Reset(ctx)
}

func (m *member) mustReset(t *testing.T) runtime.ResetReport {
	t.Helper()
	report, err := m.reset(context.Background())
	if err != nil {
		t.Fatalf("Reset: %v", err)
	}
	return report
}

func (m *member) exec(t *testing.T, script string) string {
	t.Helper()
	return mustExec(t, m.backend, m.h, "/bin/sh", "-c", script)
}

func (m *member) leakSandbox() leakSandbox {
	return leakSandbox{backend: m.backend, h: m.h, reset: m.reset, writable: bwrapWritable, initPIDs: bwrapInitPIDs}
}

// procs is the host's view of the member's processes.
func (m *member) procs(t *testing.T) []int {
	t.Helper()
	pids, err := cgroup.Procs(m.dir)
	if err != nil {
		t.Fatalf("Procs: %v", err)
	}
	return pids
}

// destroyPromptly is what the pool does to a member whose reset failed, and
// what every failure must leave possible.
func (m *member) destroyPromptly(t *testing.T) time.Duration {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	start := time.Now()
	if err := m.backend.Destroy(ctx, m.h); err != nil {
		t.Fatalf("Destroy after a failed reset: %v", err)
	}
	took := time.Since(start)
	if took > 15*time.Second {
		t.Fatalf("Destroy after a failed reset took %v", took)
	}
	return took
}

func newMember(t *testing.T) *member {
	t.Helper()
	return newMemberWith(t, runtime.ResourceLimits{CPUMillis: 1000, MemoryBytes: 256 << 20, Pids: 64})
}

// newMemberWith starts a sandbox as a pool would: IPC denied, prepared before
// any workload runs.
func newMemberWith(t *testing.T, limits runtime.ResourceLimits) *member {
	t.Helper()
	backend, h, ws := startBwrapSandbox(t, runtime.SandboxSpec{Resources: limits, DenyIPC: true})
	return prepareMember(t, backend, h, ws, unitCgroupDir(t, h.ID()))
}

func prepareMember(t *testing.T, backend *bwrap.Backend, h runtime.Handle, ws runtime.WorkspaceSpec, dir string) *member {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	r, err := backend.Prepare(ctx, h)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	return &member{backend: backend, h: h, ws: ws, r: r, dir: dir}
}

func unitCgroupDir(t *testing.T, id string) string {
	t.Helper()
	cgroups, err := cgroup.New(config.Config{SystemdMode: config.SystemdUser})
	if err != nil {
		t.Skipf("systemd user manager unavailable: %v", err)
	}
	unit, err := cgroups.Attach(context.Background(), cgroup.UnitName(id))
	if err != nil {
		t.Fatalf("attach unit: %v", err)
	}
	dir, err := unit.Dir(context.Background())
	if err != nil {
		t.Fatalf("unit cgroup: %v", err)
	}
	return dir
}

// Every row of the leak matrix the design lists for bwrap, on one member.
func TestBwrapReusableLeakMatrix(t *testing.T) {
	m := newMember(t)
	testLeakMatrix(t, m.leakSandbox())
}

func TestBwrapReusableControlSocketGone(t *testing.T) {
	m := newMember(t)
	testLeakControlSocketGone(t, m.leakSandbox())
	m.destroyPromptly(t)
}

// The host does the killing and counts it. The agent's own kill(-1) would also
// clean up, so a wrong baseline (one that includes what a run left) would still
// leave a clean sandbox; the count is what tells the two apart.
func TestBwrapReusableHostKillsAndCounts(t *testing.T) {
	m := newMember(t)
	baseline := m.procs(t)
	if len(baseline) != 3 {
		t.Fatalf("host sees %v in the cgroup, want three: outer bwrap, the reaper and the agent", baseline)
	}
	m.exec(t, `
setsid sleep 300 </dev/null >/dev/null 2>&1 &
setsid sleep 300 </dev/null >/dev/null 2>&1 &
setsid sleep 300 </dev/null >/dev/null 2>&1 &
echo run > /tmp/planted
`)
	if got := m.procs(t); len(got) != 6 {
		t.Fatalf("host sees %v, want the baseline plus three sleepers", got)
	}
	report := m.mustReset(t)
	if report.ProcessesKilled != 3 || report.EntriesRemoved != 1 {
		t.Fatalf("report = %+v, want 3 killed and 1 removed", report)
	}
	if got := m.procs(t); !slices.Equal(got, baseline) {
		t.Fatalf("host sees %v after the reset, want the baseline %v", got, baseline)
	}
	if got := processes(t, m.backend, m.h); !slices.Equal(got, bwrapInitPIDs) {
		t.Fatalf("the run sees %v, want %v", got, bwrapInitPIDs)
	}
	if report := m.mustReset(t); report != (runtime.ResetReport{}) {
		t.Fatalf("second reset = %+v, want nothing to do", report)
	}
}

// pids.current counts the agent's threads, which come and go. cgroup.procs
// counts processes, and the host's baseline must hold across many resets.
func TestBwrapReusableBaselineIsStable(t *testing.T) {
	m := newMember(t)
	baseline := m.procs(t)
	tasks := map[int64]int{}
	for i := range 30 {
		m.exec(t, `echo run > /tmp/f; (setsid sleep 300 </dev/null >/dev/null 2>&1 &)`)
		report := m.mustReset(t)
		if report.ProcessesKilled != 1 {
			t.Fatalf("reset %d killed %d, want 1", i, report.ProcessesKilled)
		}
		if got := m.procs(t); !slices.Equal(got, baseline) {
			t.Fatalf("reset %d: host sees %v, want %v", i, got, baseline)
		}
		st, err := m.backend.Stats(context.Background(), m.h)
		if err != nil {
			t.Fatal(err)
		}
		tasks[st.Pids]++
	}
	t.Logf("baseline %v held over 30 resets; idle pids.current seen: %v", baseline, tasks)
}

// A run that was OOM-killed, killed by its timeout, or used the whole pids
// budget leaves a member that resets and runs again.
func TestBwrapReusableAfterAbnormalRuns(t *testing.T) {
	const limit = 64 << 20
	m := newMemberWith(t, runtime.ResourceLimits{CPUMillis: 1000, MemoryBytes: limit, Pids: 16})

	runs := map[string]runtime.ExecRequest{
		"OOM":       {Command: "/usr/bin/python3", Args: []string{"-c", "x = b'x' * (256 << 20)"}},
		"timeout":   {Command: "/bin/sh", Args: []string{"-c", "sleep 300"}, Timeout: time.Second},
		"fork bomb": {Command: "/bin/sh", Args: []string{"-c", "bomb() { bomb | bomb & }; bomb; sleep 300"}, Timeout: 2 * time.Second},
	}
	for _, name := range []string{"OOM", "timeout", "fork bomb"} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			meter, err := m.backend.Meter(ctx, m.h)
			if err != nil {
				t.Fatal(err)
			}
			stdout, stderr, code := execSandbox(t, m.backend, m.h, runs[name])
			usage, err := meter.Stop(ctx)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("%s: exit %d, usage %+v, stdout %q, stderr %q", name, code, usage, stdout, stderr)
			if code == 0 {
				t.Fatalf("%s exited 0, so this proved nothing", name)
			}
			switch name {
			case "OOM":
				if usage.OOMKills == 0 {
					t.Fatal("no OOM kill counted")
				}
			case "fork bomb":
				if usage.PidsLimitHits == 0 {
					t.Fatal("the pids limit was never hit")
				}
			}
			report := m.mustReset(t)
			t.Logf("%s: reset killed %d, removed %d", name, report.ProcessesKilled, report.EntriesRemoved)
			if got := m.exec(t, "echo ok"); got != "ok\n" {
				t.Fatalf("exec after the reset = %q", got)
			}
			if got := processes(t, m.backend, m.h); !slices.Equal(got, bwrapInitPIDs) {
				t.Fatalf("the run sees %v, want %v", got, bwrapInitPIDs)
			}
		})
	}
}

// Each of these leaves a member that cannot be proven. The reset must say so
// promptly, and the destroy that follows must work.
func TestBwrapReusableFailsClosed(t *testing.T) {
	// A signal to the agent is sent after the exec has returned, from a process
	// that outlived it, or the exec's own stream would hang on the stopped agent.
	t.Run("the workload stopped the agent", func(t *testing.T) {
		m := newMember(t)
		m.exec(t, "setsid sh -c 'sleep 0.2; kill -STOP 2' </dev/null >/dev/null 2>&1 &")
		time.Sleep(time.Second)
		start := time.Now()
		_, err := m.reset(context.Background())
		took := time.Since(start)
		if !errors.Is(err, runtime.ErrUnproven) || !strings.Contains(err.Error(), "stopped") {
			t.Fatalf("Reset = %v, want ErrUnproven naming a stopped init process", err)
		}
		if took > 3*time.Second {
			t.Fatalf("Reset took %v to notice a stopped agent", took)
		}
		t.Logf("reset failed in %v: %v; destroy took %v", took, err, m.destroyPromptly(t))
	})

	t.Run("the workload killed the agent", func(t *testing.T) {
		m := newMember(t)
		m.exec(t, "setsid sh -c 'sleep 0.2; kill -KILL 2' </dev/null >/dev/null 2>&1 &")
		time.Sleep(time.Second)
		if _, err := cgroup.Procs(m.dir); !errors.Is(err, cgroup.ErrNotFound) {
			t.Fatalf("the sandbox's cgroup is still there (%v): the agent was not killed, so this proves nothing", err)
		}
		start := time.Now()
		_, err := m.reset(context.Background())
		took := time.Since(start)
		if !errors.Is(err, runtime.ErrUnproven) {
			t.Fatalf("Reset = %v, want ErrUnproven", err)
		}
		if took > 3*time.Second {
			t.Fatalf("Reset took %v with the agent dead", took)
		}
		actx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := m.r.Alive(actx); err == nil {
			t.Fatal("Alive succeeded with the agent dead")
		}
		t.Logf("reset failed in %v: %v; destroy took %v", took, err, m.destroyPromptly(t))
	})

	t.Run("the sandbox was destroyed", func(t *testing.T) {
		m := newMember(t)
		m.destroyPromptly(t)
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		start := time.Now()
		_, err := m.r.Reset(ctx)
		if !errors.Is(err, runtime.ErrUnproven) {
			t.Fatalf("Reset = %v, want ErrUnproven", err)
		}
		if err := m.r.Alive(ctx); err == nil {
			t.Fatal("Alive succeeded after the destroy")
		}
		if took := time.Since(start); took > 3*time.Second {
			t.Fatalf("Reset and Alive took %v after the destroy", took)
		}
	})

}

// A vfork parent waits in a killable, uninterruptible sleep (D) until its child
// execs or exits. On kernel 7.0 the cgroup freezer still reports the cgroup
// frozen, measured on vora-mini, and the parent dies of SIGKILL like any other
// task, so the reset passes. Python's ctypes gives a workload vfork without a
// compiler; the child then sleeps in the parent's memory, which is undefined
// for Python but is exactly what a hostile run could do. A task the freezer
// cannot freeze was not producible without privileges; the deadline path is
// covered in internal/runtime/bwrap with a cgroup that never freezes.
func TestBwrapReusableKillsAnUninterruptibleWait(t *testing.T) {
	m := newMember(t)
	m.exec(t, `setsid python3 -c '
import ctypes
libc = ctypes.CDLL(None)
if libc.vfork() == 0:
    libc.sleep(300)
    libc._exit(0)
' </dev/null >/dev/null 2>&1 &
sleep 0.5`)
	states := m.exec(t, `for p in /proc/[0-9]*; do awk '{print $1, $2, $3}' "$p/stat" 2>/dev/null; done`)
	t.Logf("before the reset:\n%s", states)
	if !strings.Contains(states, "(python3) D") {
		t.Fatal("no python3 in an uninterruptible wait, so this proves nothing")
	}
	start := time.Now()
	report := m.mustReset(t)
	t.Logf("reset in %v: %+v", time.Since(start), report)
	if report.ProcessesKilled != 2 {
		t.Fatalf("killed %d, want the vfork parent and its child", report.ProcessesKilled)
	}
	if got := processes(t, m.backend, m.h); !slices.Equal(got, bwrapInitPIDs) {
		t.Fatalf("the run sees %v, want %v", got, bwrapInitPIDs)
	}
}

// The kill reaches only this member's cgroup. Two members side by side, each
// with a sleeper, and a reset of one leaves the other's sleeper running.
func TestBwrapReusableNeighbourSurvives(t *testing.T) {
	a, b := newMember(t), newMember(t)
	const sleeper = `setsid sleep 300 </dev/null >/dev/null 2>&1 & echo run > /tmp/planted`
	a.exec(t, sleeper)
	b.exec(t, sleeper)
	bProcs := b.procs(t)

	if report := a.mustReset(t); report.ProcessesKilled != 1 {
		t.Fatalf("reset of a killed %d, want 1", report.ProcessesKilled)
	}
	if got := b.procs(t); !slices.Equal(got, bProcs) {
		t.Fatalf("b's processes went from %v to %v when a was reset", bProcs, got)
	}
	if got := b.exec(t, "cat /tmp/planted"); got != "run\n" {
		t.Fatalf("b's file = %q after a's reset", got)
	}
	if report := b.mustReset(t); report.ProcessesKilled != 1 || report.EntriesRemoved != 1 {
		t.Fatalf("reset of b = %+v, want 1 killed and 1 removed", report)
	}
}

// Resets of different members at once do not interfere.
func TestBwrapReusableConcurrentResets(t *testing.T) {
	members := []*member{newMember(t), newMember(t), newMember(t)}
	var wg sync.WaitGroup
	errs := make([]error, len(members))
	for i, m := range members {
		wg.Go(func() {
			for round := range 5 {
				m.exec(t, `echo run > /tmp/planted; (setsid sleep 300 </dev/null >/dev/null 2>&1 &)`)
				report, err := m.reset(context.Background())
				if err != nil {
					errs[i] = fmt.Errorf("round %d: %w", round, err)
					return
				}
				if report.ProcessesKilled != 1 || report.EntriesRemoved != 1 {
					errs[i] = fmt.Errorf("round %d: report %+v", round, report)
					return
				}
			}
		})
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("member %d: %v", i, err)
		}
	}
}

// A sandbox with a network of its own lives in the runner's cgroup subtree
// rather than a systemd unit, and the reset must find it there. Runs under
// rootlesskit, as `make test-network` does.
func TestBwrapReusableNetworkModes(t *testing.T) {
	requireNetnsTools(t)
	t.Setenv("VORA_INIT_BINARY", buildBinary(t, "vora-init"))
	subtree, err := cgroup.NewSubtree()
	if err != nil {
		t.Skipf("cgroup subtree unavailable: %v", err)
	}
	rootfs := imageRootfs(t, "base")
	stateDir, err := os.MkdirTemp("", "vora-it-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(stateDir) })
	cfg := config.Config{StateDir: stateDir, SystemdMode: config.SystemdUser, EgressMaxConnections: 32, EgressIdleTimeout: time.Minute}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	backend, err := bwrap.New(cfg, nil, logger)
	if err != nil {
		t.Fatalf("bwrap backend: %v", err)
	}
	backend.EnableNetns(subtree, network.NewManager(stateDir, network.ManagerConfig{}, logger), func(egress.Event) error { return nil })
	wsManager := workspace.NewManager(cfg.SandboxesDir())

	for _, mode := range []runtime.NetworkMode{runtime.NetworkRestricted, runtime.NetworkFull, runtime.NetworkPrivate} {
		t.Run(string(mode), func(t *testing.T) {
			id := ulid.NewSandbox()
			ws := wsManager.Paths(id)
			ws.RootfsDir = rootfs
			if _, err := wsManager.Create(ws); err != nil {
				t.Fatalf("workspace: %v", err)
			}
			t.Cleanup(func() { _ = wsManager.Destroy(id) })
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			h, err := backend.Create(ctx, runtime.SandboxSpec{
				ID: id, TenantID: "tenant-a", Image: "base",
				Resources: runtime.ResourceLimits{CPUMillis: 1000, MemoryBytes: 256 << 20, Pids: 64},
				Network:   runtime.NetworkPolicy{Mode: mode},
				TTL:       time.Minute, Workspace: ws, DenyIPC: true,
			})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			t.Cleanup(func() { _ = backend.Destroy(context.Background(), h) })
			if err := backend.Start(ctx, h); err != nil {
				t.Fatalf("Start: %v", err)
			}
			group, err := subtree.Attach(ctx, id)
			if err != nil {
				t.Fatalf("Attach: %v", err)
			}
			m := prepareMember(t, backend, h, ws, group.Path())
			baseline := m.procs(t)
			m.exec(t, `setsid sleep 300 </dev/null >/dev/null 2>&1 & echo run > /tmp/planted`)
			report := m.mustReset(t)
			if report.ProcessesKilled != 1 || report.EntriesRemoved != 1 {
				t.Fatalf("report = %+v, want 1 killed and 1 removed", report)
			}
			if got := m.procs(t); !slices.Equal(got, baseline) {
				t.Fatalf("host sees %v after the reset, want %v", got, baseline)
			}
			if got := processes(t, m.backend, m.h); !slices.Equal(got, bwrapInitPIDs) {
				t.Fatalf("the run sees %v, want %v", got, bwrapInitPIDs)
			}
			testLeakMatrix(t, m.leakSandbox())
		})
	}
}

// measuring gates the long runs below behind VORA_MEASURE, which CI does not set.
func measuring(t *testing.T) {
	t.Helper()
	if os.Getenv("VORA_MEASURE") == "" {
		t.Skip("set VORA_MEASURE=1 to run")
	}
}

// realisticFiles is what a pooled run leaves on disk: files in the writable
// places and the Python interpreter's own droppings. realisticRun adds a
// process that outlived the run.
const realisticFiles = `
python3 - <<'EOF'
import os, json, tempfile
with open("/workspace/result.json", "w") as f:
    json.dump({"answer": list(range(1000))}, f)
with tempfile.NamedTemporaryFile(dir="/tmp", delete=False) as f:
    f.write(b"x" * 65536)
os.makedirs("/root/.cache/run", exist_ok=True)
open("/root/.cache/run/state", "w").write("done")
EOF
`

const realisticRun = realisticFiles + `(setsid sleep 300 </dev/null >/dev/null 2>&1 &)
`

// TestBwrapReusableMeasure times the reset on a realistic run, and the parts
// that make it up: the host's kill alone and the agent's reset alone.
func TestBwrapReusableMeasure(t *testing.T) {
	measuring(t)
	const n = 200
	m := newMember(t)
	// Dialled before any workload runs, so it is as trusted as the backend's.
	client, err := agentproto.Dial(m.ws.ControlSocket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	if err := client.WaitReady(context.Background(), 15*time.Second); err != nil {
		t.Fatal(err)
	}
	manifest, err := client.Capture(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	baseline := m.procs(t)

	var full, hostOnly, agentOnly, idle, freezeOnly []time.Duration
	for range n {
		start := time.Now()
		if err := cgroup.Freeze(context.Background(), m.dir); err != nil {
			t.Fatal(err)
		}
		if err := cgroup.Thaw(m.dir); err != nil {
			t.Fatal(err)
		}
		freezeOnly = append(freezeOnly, time.Since(start))
	}
	t.Logf("freeze and thaw of the idle member alone: %s", describeLatency(freezeOnly))
	for i := range n {
		m.exec(t, realisticRun)
		start := time.Now()
		report := m.mustReset(t)
		full = append(full, time.Since(start))
		if report.ProcessesKilled != 1 || report.EntriesRemoved < 3 {
			t.Fatalf("reset %d = %+v, want 1 killed and the run's files removed", i, report)
		}
	}
	t.Logf("full reset after a realistic run: %s", describeLatency(full))
	for range n {
		m.exec(t, `(setsid sleep 300 </dev/null >/dev/null 2>&1 &)`)
		start := time.Now()
		if _, err := cgroup.KillOthers(context.Background(), m.dir, baseline); err != nil {
			t.Fatal(err)
		}
		hostOnly = append(hostOnly, time.Since(start))
	}
	t.Logf("host freeze, kill, thaw, proof alone: %s", describeLatency(hostOnly))
	m.mustReset(t)
	for i := range n {
		m.exec(t, realisticFiles)
		start := time.Now()
		removed, err := client.Reset(context.Background(), manifest)
		agentOnly = append(agentOnly, time.Since(start))
		if err != nil || removed < 3 {
			t.Fatalf("agent reset %d = %d, %v, want the run's files removed", i, removed, err)
		}
	}
	t.Logf("agent reset alone, after the run's files: %s", describeLatency(agentOnly))
	for range n {
		start := time.Now()
		m.mustReset(t)
		idle = append(idle, time.Since(start))
	}
	t.Logf("full reset with nothing to do: %s", describeLatency(idle))
}

// TestBwrapReusablePidsFloor runs exec and reset at pids limits around the
// pool's floor and reports where each workload stops working. The floor is
// about the exec: a reset adds no task on either side.
func TestBwrapReusablePidsFloor(t *testing.T) {
	measuring(t)
	workloads := []struct{ name, script string }{
		{"echo", "echo ok"},
		{"echo and a sleeper", "echo ok; setsid sleep 300 </dev/null >/dev/null 2>&1 &"},
	}
	for _, w := range workloads {
		for _, pids := range []int64{11, 12, 13, 14, 16} {
			t.Run(fmt.Sprintf("%s at pids %d", w.name, pids), func(t *testing.T) {
				m := newMemberWith(t, runtime.ResourceLimits{CPUMillis: 1000, MemoryBytes: 256 << 20, Pids: pids})
				var afterExec, afterReset int64
				for i := range 100 {
					stdout, stderr, code := execSandbox(t, m.backend, m.h, runtime.ExecRequest{Command: "/bin/sh", Args: []string{"-c", w.script}})
					if code != 0 || stdout != "ok\n" {
						t.Logf("round %d: exec failed: exit %d, stdout %q, stderr %q", i, code, stdout, stderr)
						return
					}
					afterExec = max(afterExec, m.tasks(t))
					if _, err := m.reset(context.Background()); err != nil {
						t.Fatalf("round %d: reset: %v", i, err)
					}
					afterReset = max(afterReset, m.tasks(t))
				}
				t.Logf("100 rounds; pids.current peaked at %d after the exec and %d after the reset", afterExec, afterReset)
			})
		}
	}
}

// tasks is pids.current: every thread in the sandbox.
func (m *member) tasks(t *testing.T) int64 {
	t.Helper()
	st, err := m.backend.Stats(context.Background(), m.h)
	if err != nil {
		t.Fatal(err)
	}
	return st.Pids
}
