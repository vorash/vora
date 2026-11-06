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
	"maps"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"

	v1 "github.com/vorash/vora/api/proto/v1"
	"github.com/vorash/vora/internal/cgroup"
	"github.com/vorash/vora/internal/config"
	"github.com/vorash/vora/internal/runtime"
	"github.com/vorash/vora/internal/runtime/bwrap"
	"github.com/vorash/vora/internal/runtime/gvisor"
)

// This file is the leak matrix through a real pool: a client's run A plants through vora-api
// and the runner's RunInPool, and run B on the same member finds nothing. What a backend does
// differently is all in poolHost, so the tests read the same for both.

// poolHost is one backend as a real runner runs it.
type poolHost struct {
	name string
	// open builds the backend, or skips the test on a host that cannot run it. crash ends
	// a member from the host, as a crash would.
	open func(t *testing.T, cfg *config.Config, logger *slog.Logger) (backend runtime.Backend, crash func(t *testing.T, sandboxID string))
	// writable and initPIDs mean what they do in leakSandbox.
	writable []string
	initPIDs []int
	// disconnectKeepsMember is whether a run abandoned mid-fork must leave its member usable.
	// It holds where one kill pass is exact, because no process can fork while frozen. On
	// gVisor guest code runs between passes, so a member may fail closed instead.
	disconnectKeepsMember bool
	// endings are the ways a workload can end or wedge its member here.
	endings []poolEnding
	// missingCommandExit is the exit code of a command that does not exist.
	missingCommandExit int32
}

// poolEnding is one workload that attacks the member it runs on.
type poolEnding struct {
	name string
	run  map[string]any
	// memoryBytes is 0 for leakPoolMemory.
	memoryBytes int64
	// costsMember is whether the member must be gone afterwards. Otherwise either outcome
	// is right, as long as the next run is clean.
	costsMember bool
	check       func(t *testing.T, res runResult)
	// leavesProcesses is a workload that exits at once and leaves processes behind, so its
	// exit says nothing. The reset must have killed some, or the member must be gone.
	leavesProcesses bool
}

const leakPoolMemory = 256 << 20

// forkBombScript stops only when its timeout does.
const forkBombScript = "bomb() { bomb | bomb & }; echo started; bomb; sleep 300"

var bwrapPoolHost = poolHost{
	name:                  "bwrap",
	open:                  openBwrapPoolBackend,
	writable:              bwrapWritable,
	initPIDs:              bwrapInitPIDs,
	disconnectKeepsMember: true,
	endings: []poolEnding{
		{
			name:        "kills_its_agent",
			run:         shellRun("echo started; kill -9 2; sleep 30", 20),
			costsMember: true,
			check: func(t *testing.T, res runResult) {
				t.Helper()
				if res.exit.GetReason() == v1.ExitReason_EXITED {
					t.Fatalf("a run that took its agent down reads as a normal exit: %v", res.exit)
				}
			},
		},
		{
			name:        "stops_its_agent",
			run:         shellRun("echo started; kill -STOP 2; sleep 30", 1),
			costsMember: true,
			check: func(t *testing.T, res runResult) {
				t.Helper()
				if res.exit.GetReason() != v1.ExitReason_TIMEOUT_EXCEEDED {
					t.Fatalf("exit = %v, want the runner's hard deadline to end it", res.exit)
				}
			},
		},
		{
			name: "fork_bomb",
			run:  shellRun(forkBombScript, 3),
			check: func(t *testing.T, res runResult) {
				t.Helper()
				// The agent can die at the limit (#147), which ends the run in an error line.
				if res.errLine == "" && res.exit.GetUsage().GetPidsLimitHits() == 0 {
					t.Fatalf("the bomb neither reached pids.max nor took its sandbox down, so this proved nothing: %v", res.exit)
				}
			},
		},
	},
	missingCommandExit: 127,
}

var gvisorPoolHost = poolHost{
	name: "gvisor",
	open: openGvisorPoolBackend,
	// No /vora: it is read-only here, and TestGvisorVoraReadOnly covers what a run can do to it.
	writable: []string{"/tmp", "/dev", "/dev/shm", "/workspace", "/root"},
	initPIDs: []int{1},
	endings: []poolEnding{
		{
			name:        "out_of_memory",
			run:         map[string]any{"command": "/usr/bin/python3", "args": []string{"-c", `b = bytearray(1 << 30); [b.__setitem__(i, 1) for i in range(0, len(b), 4096)]`}, "timeoutSeconds": "30"},
			memoryBytes: 128 << 20,
			costsMember: true,
			check: func(t *testing.T, res runResult) {
				t.Helper()
				if res.exit.GetReason() == v1.ExitReason_EXITED {
					t.Fatalf("a run that took its sandbox down reads as a normal exit: %v", res.exit)
				}
			},
		},
		{
			name:            "fork_chains",
			run:             map[string]any{"command": "/usr/bin/python3", "args": []string{"-W", "ignore", "-c", forkChains, "16"}, "timeoutSeconds": "20"},
			leavesProcesses: true,
		},
		// pids.max holds Sentry threads here, so nothing but memory.max is left to bound this one.
		{
			name:        "fork_bomb",
			run:         shellRun(forkBombScript, 30),
			costsMember: true,
			check: func(t *testing.T, res runResult) {
				t.Helper()
				if !strings.Contains(string(res.stdout), "started") {
					t.Fatalf("the bomb never started, so this proved nothing: stdout %q, stderr %q, exit %v", res.stdout, res.stderr, res.exit)
				}
				if res.exit.GetReason() != v1.ExitReason_OOM_KILLED || res.exit.GetUsage().GetOomKills() == 0 {
					t.Fatalf("exit %v, want memory.max to have stopped the bomb", res.exit)
				}
			},
		},
	},
	missingCommandExit: 128,
}

var poolHosts = []poolHost{bwrapPoolHost, gvisorPoolHost}

func openBwrapPoolBackend(t *testing.T, cfg *config.Config, logger *slog.Logger) (runtime.Backend, func(*testing.T, string)) {
	t.Helper()
	cgroups, err := cgroup.New(*cfg)
	if err != nil {
		t.Skipf("systemd user manager unavailable: %v", err)
	}
	backend, err := bwrap.New(*cfg, cgroups, logger)
	if err != nil {
		t.Fatalf("bwrap backend: %v", err)
	}
	return backend, func(t *testing.T, id string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		unit, err := cgroups.Attach(ctx, cgroup.UnitName(id))
		if err != nil {
			t.Fatalf("attach unit of %s: %v", id, err)
		}
		dir, err := unit.Dir(ctx)
		if err != nil {
			t.Fatalf("cgroup of %s: %v", id, err)
		}
		if err := os.WriteFile(filepath.Join(dir, "cgroup.kill"), []byte("1"), 0o644); err != nil {
			t.Fatalf("kill %s: %v", id, err)
		}
	}
}

// unlinkImages replaces the images symlink with a tree of hard links. runsc's gofer
// refuses a mount whose target path crosses a symlink, so a rootfs reached through one
// cannot start. Removing the tree at the end leaves the linked files where they were.
func unlinkImages(t *testing.T, images string) {
	t.Helper()
	target, err := os.Readlink(images)
	if err != nil {
		t.Fatalf("read images link: %v", err)
	}
	if err := os.Remove(images); err != nil {
		t.Fatalf("remove images link: %v", err)
	}
	if out, err := exec.Command("cp", "-al", target, images).CombinedOutput(); err != nil {
		t.Fatalf("link images: %v: %s", err, out)
	}
}

func openGvisorPoolBackend(t *testing.T, cfg *config.Config, logger *slog.Logger) (runtime.Backend, func(*testing.T, string)) {
	t.Helper()
	requireGvisor(t)
	unlinkImages(t, cfg.ImagesDir())
	cfg.GvisorPlatform = "kvm"
	cfg.GvisorRunRoot = filepath.Join(os.Getenv("XDG_RUNTIME_DIR"), "vora-gvisor-pool")
	subtree, err := cgroup.NewSubtree()
	if err != nil {
		t.Fatalf("cgroup subtree: %v", err)
	}
	backend, err := gvisor.New(*cfg, logger, subtree)
	if err != nil {
		t.Fatalf("gvisor backend: %v", err)
	}
	root := cfg.GvisorRunRoot
	return backend, func(t *testing.T, id string) {
		t.Helper()
		m := gvisorMember{root: root, id: id}
		if err := syscall.Kill(m.sentryPID(t), syscall.SIGKILL); err != nil {
			t.Fatalf("kill the Sentry of %s: %v", id, err)
		}
	}
}

func forEachPoolHost(t *testing.T, test func(t *testing.T, p *poolPlane)) {
	t.Helper()
	for _, host := range poolHosts {
		t.Run(host.name, func(t *testing.T) { test(t, startPoolPlaneOn(t, host, false)) })
	}
}

// in is the plane for a subtest, so a failure and a cleanup land on the subtest.
func (p *poolPlane) in(t *testing.T) *poolPlane {
	c := *p
	c.t = t
	return &c
}

// declareOne declares a pool with one member, so the next run always lands on it, and gives
// it runs enough to outlast the test.
func (p *poolPlane) declareOne(name string, memoryBytes int64, maxRuns int) {
	p.t.Helper()
	shape := poolShapeOn(p.host.name, memoryBytes, 1, 1)
	shape["maxRuns"] = maxRuns
	p.declarePool(name, shape)
	p.waitIdle(name, 1)
}

// runBody is what a client posts for an exec request.
func runBody(req runtime.ExecRequest) map[string]any {
	timeout := req.Timeout
	if timeout == 0 {
		timeout = time.Minute
	}
	body := map[string]any{"command": req.Command, "args": req.Args, "timeoutSeconds": strconv.Itoa(int(timeout / time.Second))}
	if req.Cwd != "" {
		body["cwd"] = req.Cwd
	}
	if len(req.Env) > 0 {
		var env []string
		for _, k := range slices.Sorted(maps.Keys(req.Env)) {
			env = append(env, k+"="+req.Env[k])
		}
		body["env"] = env
	}
	return body
}

// memberRuns makes a member look like a backend to the matrix's helpers: every Exec is one
// whole run on the pool, and every run must land on the same member, warm, one run later, or
// what it finds says nothing about what the run before it left.
type memberRuns struct {
	// Exec is the only method the matrix's helpers call.
	runtime.Backend
	p      *poolPlane
	pool   string
	member string
	last   uint32
}

func (m *memberRuns) run(req runtime.ExecRequest) runResult {
	m.p.t.Helper()
	res := m.p.runRetrying(m.pool, runBody(req))
	res.mustComplete(m.p.t)
	got, n := res.run.GetSandboxId(), res.run.GetSandboxRun()
	switch {
	case !res.run.GetWarm():
		m.p.t.Fatalf("run %v was not warm, so it proves nothing about reuse", res.run)
	case m.member != "" && got != m.member:
		m.p.t.Fatalf("run landed on %s, not on %s, whose earlier run's reset was %v", got, m.member, res.done)
	case m.member != "" && n != m.last+1:
		m.p.t.Fatalf("sandbox_run = %d after %d", n, m.last)
	case res.done.GetOutcome() != v1.PoolResetOutcome_POOL_RESET_OK:
		m.p.t.Fatalf("this run's reset was %v, so the member will not come back", res.done)
	}
	m.member, m.last = got, n
	return res
}

func (m *memberRuns) Exec(_ context.Context, _ runtime.Handle, req runtime.ExecRequest) (runtime.ExecStream, error) {
	res := m.run(req)
	return &replayedRun{chunks: []runtime.ExecChunk{
		{Stdout: res.stdout},
		{Stderr: res.stderr},
		{Exit: &runtime.ExecResult{ExitCode: int(res.exit.GetExitCode())}},
	}}, nil
}

type replayedRun struct{ chunks []runtime.ExecChunk }

func (r *replayedRun) Recv() (runtime.ExecChunk, error) {
	if len(r.chunks) == 0 {
		return runtime.ExecChunk{}, io.EOF
	}
	c := r.chunks[0]
	r.chunks = r.chunks[1:]
	return c, nil
}

func (r *replayedRun) Close() error { return nil }

// memberState is what a run finds on a member.
type memberState struct {
	files string
	pids  []int
}

// probeProgram reads the state in one run, so a reset cannot come between the two reads.
func (p *poolPlane) probeProgram() runtime.ExecRequest {
	return runtime.ExecRequest{
		Command: "/bin/sh",
		Args:    append([]string{"-c", `snapshot=$1; pids=$2; shift 2; /usr/bin/python3 -c "$snapshot" "$@"; exec /usr/bin/python3 -c "$pids"`, "probe", fsSnapshot, printPIDs}, p.host.writable...),
		Timeout: 30 * time.Second,
	}
}

// parseProbe reads a probe's output: the listing, then one line of PIDs. It reports false for
// output that stopped before the PIDs.
func parseProbe(stdout []byte) (memberState, bool) {
	out := strings.TrimRight(string(stdout), "\n")
	cut := strings.LastIndexByte(out, '\n')
	var got struct {
		Self int   `json:"self"`
		PIDs []int `json:"pids"`
	}
	if err := json.Unmarshal([]byte(out[cut+1:]), &got); err != nil {
		return memberState{}, false
	}
	return memberState{files: out[:cut+1], pids: slices.DeleteFunc(got.PIDs, func(pid int) bool { return pid == got.Self })}, true
}

// stateOf is what a probe found. An exec left running on the member holds its exec lock, so a
// probe that never got to run is a leak too.
func (p *poolPlane) stateOf(res runResult) memberState {
	p.t.Helper()
	if res.exit.GetExitCode() != 0 || res.exit.GetReason() != v1.ExitReason_EXITED {
		p.t.Fatalf("the probe on %s ended with %v, stderr %q", res.run.GetSandboxId(), res.exit, res.stderr)
	}
	state, ok := parseProbe(res.stdout)
	if !ok {
		p.t.Fatalf("parse the probe %q (stderr %q)", res.stdout, res.stderr)
	}
	return state
}

// mustMatch fails unless a next run finds the member as it was prepared.
func (p *poolPlane) mustMatch(prepared, got memberState, after string) {
	p.t.Helper()
	if got.files != prepared.files {
		p.t.Fatalf("after %s the writable mounts are not back to the prepared state:\n%s", after, lineDiff(prepared.files, got.files))
	}
	if !slices.Equal(got.pids, p.host.initPIDs) {
		p.t.Fatalf("after %s a next run sees processes %v, want %v", after, got.pids, p.host.initPIDs)
	}
}

// withoutIdentity drops the inode and mtime of each listed path, which differ between two
// fresh members.
func (m memberState) withoutIdentity() string {
	var out strings.Builder
	for line := range strings.Lines(m.files) {
		fields := strings.Fields(line)
		fmt.Fprintln(&out, strings.Join(fields[:len(fields)-2], " "))
	}
	return out.String()
}

// mustMatchFresh fails unless a replacement member looks as the first one did when prepared.
func (p *poolPlane) mustMatchFresh(prepared, got memberState, after string) {
	p.t.Helper()
	if a, b := prepared.withoutIdentity(), got.withoutIdentity(); a != b {
		p.t.Fatalf("after %s the replacement is not a fresh member:\n%s", after, lineDiff(a, b))
	}
	if !slices.Equal(got.pids, p.host.initPIDs) {
		p.t.Fatalf("after %s the replacement runs processes %v, want %v", after, got.pids, p.host.initPIDs)
	}
}

var resetEvent = regexp.MustCompile(`reset: ok: killed (\d+), removed (\d+) entries`)

// resetsOf reads what each of a member's resets reported, once it has this many.
func (p *poolPlane) resetsOf(pool, member string, want int) (killed, removed []int) {
	p.t.Helper()
	waitFor(p.t, 5*time.Second, func() bool {
		_, data := p.call(http.MethodGet, "/v1/pools/"+pool+"/sandboxes/"+member+"/events", p.tenant, nil)
		matches := resetEvent.FindAllStringSubmatch(string(data), -1)
		killed, removed = nil, nil
		for _, m := range matches {
			k, _ := strconv.Atoi(m[1])
			r, _ := strconv.Atoi(m[2])
			killed, removed = append(killed, k), append(removed, r)
		}
		return len(matches) >= want
	})
	return killed, removed
}

// TestPoolLeakMatrix runs every row twice on one member, as TestLeakMatrix does on a backend.
func TestPoolLeakMatrix(t *testing.T) {
	forEachPoolHost(t, func(t *testing.T, p *poolPlane) {
		p.declareOne("leak", leakPoolMemory, 1000)
		runs := &memberRuns{p: p, pool: "leak"}
		s := leakSandbox{backend: runs, writable: p.host.writable, initPIDs: p.host.initPIDs}
		prepared := p.stateOf(runs.run(p.probeProgram()))
		p.mustMatch(prepared, prepared, "nothing")

		for round := range 2 {
			for _, row := range leakRows {
				t.Run(fmt.Sprintf("%s/%d", row.name, round), func(t *testing.T) {
					runs.p = p.in(t)
					a := runs.run(row.pool.runA(s))
					if !row.pool.endsAtTimeout && (a.exit.GetExitCode() != 0 || a.exit.GetReason() != v1.ExitReason_EXITED) {
						t.Fatalf("run A failed, so it may have planted nothing: %v\nstdout: %s\nstderr: %s", a.exit, a.stdout, a.stderr)
					}
					if row.pool.took != nil {
						row.pool.took(t, a)
					}
					killed, removed := runs.p.resetsOf("leak", runs.member, int(runs.last))
					if row.pool.killed && killed[len(killed)-1] == 0 {
						t.Errorf("run A's reset killed nothing, so this row proves nothing about processes")
					}
					if row.pool.removed && removed[len(removed)-1] == 0 {
						t.Errorf("run A's reset removed nothing, so this row proves nothing about files")
					}
					if row.pool.clean != nil {
						row.pool.clean(t, s, string(a.stdout))
					}
					runs.p.mustMatch(prepared, runs.p.stateOf(runs.run(runs.p.probeProgram())), "run A")
				})
			}
		}
		runs.p = p
		if st := p.getPool("leak").GetStats(); st.GetResetFailuresTotal() != 0 || st.GetDestroyedTotal() != 0 {
			t.Errorf("stats %v, want every reset to have passed and no member lost", st)
		}
	})
}

// The filter a resetting pool asks for, proven by a run through the pool, as a reset could not
// clean what these calls leave. SysV IPC and POSIX queues both, by their exact errnos.
func TestPoolMembersDenyIPC(t *testing.T) {
	forEachPoolHost(t, func(t *testing.T, p *poolPlane) {
		p.declareOne("ipc", leakPoolMemory, 10)
		res := p.run("ipc", p.tenant, map[string]any{"command": "/usr/bin/python3", "args": []string{"-c", ipcProbe, ipcSyscallNumbers(t), "calls", ipcProbe}, "timeoutSeconds": "30"})
		res.mustComplete(t)
		var got map[string]int
		if err := json.Unmarshal(res.stdout, &got); err != nil {
			t.Fatalf("probe printed %q (stderr %q): %v", res.stdout, res.stderr, err)
		}
		wantProbe(t, got, ipcDenied)
	})
}

// A command that does not exist is the caller's mistake. Retiring the member would replace a
// sandbox for every typo, and a retry would run the typo twice. memberRuns checks each run
// landed on the same member, warm, with a reset that passed.
func TestPoolMissingCommandCostsNoMember(t *testing.T) {
	forEachPoolHost(t, func(t *testing.T, p *poolPlane) {
		p.declareOne("typo", leakPoolMemory, 10)
		runs := &memberRuns{p: p, pool: "typo"}
		for range 2 {
			res := runs.run(runtime.ExecRequest{Command: "/no-such-command"})
			if res.exit.GetExitCode() != p.host.missingCommandExit || res.exit.GetReason() != v1.ExitReason_EXITED {
				t.Fatalf("exit = %v, stderr %q, want %d with EXITED", res.exit, res.stderr, p.host.missingCommandExit)
			}
		}
		if st := p.getPool("typo").GetStats(); st.GetDestroyedTotal() != 0 {
			t.Errorf("stats %v, want no member lost", st)
		}
	})
}

// The PID counter is a residual channel the design accepts: a next run can tell a run came
// before it. If this fails because the channel closed, say so in ADR 028 and delete the test.
func TestPoolPIDCounterIsAnAcceptedResidual(t *testing.T) {
	forEachPoolHost(t, func(t *testing.T, p *poolPlane) {
		p.declareOne("pids", leakPoolMemory, 10)
		runs := &memberRuns{p: p, pool: "pids"}
		pid := func() int {
			res := runs.run(runtime.ExecRequest{Command: "/bin/sh", Args: []string{"-c", "echo $$"}})
			n, err := strconv.Atoi(strings.TrimSpace(string(res.stdout)))
			if err != nil {
				t.Fatalf("output %q: %v", res.stdout, err)
			}
			return n
		}
		if a, b := pid(), pid(); b <= a {
			t.Fatalf("run A had pid %d and run B has %d, which the ADR says cannot be", a, b)
		}
	})
}

// A listed entry that changed is what a reset refuses to repair. The failed member must end
// for good, and no later run may land on it.
func TestPoolFailedResetNeverReturns(t *testing.T) {
	forEachPoolHost(t, func(t *testing.T, p *poolPlane) {
		p.declareOne("failing", leakPoolMemory, 1000)
		res := p.run("failing", p.tenant, shellRun("chmod 0701 /dev/shm", 10))
		res.mustComplete(t)
		if res.exit.GetExitCode() != 0 {
			t.Fatalf("the plant failed, so this proves nothing: %v %s", res.exit, res.stderr)
		}
		if res.done.GetOutcome() != v1.PoolResetOutcome_POOL_RESET_FAILED || res.done.GetRetireCause() != "reset_failed" {
			t.Fatalf("run_done = %v, want a failed reset retiring the member", res.done)
		}
		failed := res.run.GetSandboxId()
		for range 3 {
			next := p.runRetrying("failing", shellRun("stat -c %a /dev/shm", 10))
			next.mustComplete(t)
			if next.run.GetSandboxId() == failed {
				t.Fatalf("member %s came back after its reset failed", failed)
			}
			if strings.TrimSpace(string(next.stdout)) == "701" {
				t.Fatalf("a next run reads what the failed one changed: %s", next.stdout)
			}
		}
		if st := p.getPool("failing").GetStats(); st.GetResetFailuresTotal() != 1 {
			t.Errorf("stats %v, want one failed reset", st)
		}
	})
}

// A workload that kills or wedges its member costs that member at most, and the next client
// finds a clean sandbox.
func TestPoolWorkloadThatEndsItsMember(t *testing.T) {
	forEachPoolHost(t, func(t *testing.T, plane *poolPlane) {
		for _, ending := range plane.host.endings {
			t.Run(ending.name, func(t *testing.T) {
				p := plane.in(t)
				memory := ending.memoryBytes
				if memory == 0 {
					memory = leakPoolMemory
				}
				pool := strings.ReplaceAll(ending.name, "_", "-")
				p.declareOne(pool, memory, 1000)
				first := p.run(pool, p.tenant, runBody(p.probeProgram()))
				first.mustComplete(t)
				prepared, member := p.stateOf(first), first.run.GetSandboxId()

				res := p.runRetrying(pool, ending.run)
				if res.status != http.StatusOK || res.run.GetSandboxId() != member {
					t.Fatalf("the workload did not run on %s: %d %s, %v", member, res.status, res.errBody, res.run)
				}
				t.Logf("run on %s: exit %v, run_done %v, error line %q", member, res.exit, res.done, res.errLine)
				if ending.check != nil {
					ending.check(t, res)
				}

				next := p.runRetrying(pool, runBody(p.probeProgram()))
				next.mustComplete(t)
				gone := next.run.GetSandboxId() != member
				if gone {
					p.mustMatchFresh(prepared, p.stateOf(next), ending.name)
				} else {
					p.mustMatch(prepared, p.stateOf(next), ending.name)
				}
				if ending.costsMember && !gone {
					t.Fatalf("member %s survived this, on run %d", member, next.run.GetSandboxRun())
				}
				if ending.leavesProcesses && !gone {
					run := int(res.run.GetSandboxRun())
					if killed, _ := p.resetsOf(pool, member, run); killed[run-1] == 0 {
						t.Fatalf("the member stayed and the reset after the workload killed nothing, so it left nothing to clear (exit %v, stderr %q)", res.exit, res.stderr)
					}
				}
				if gone && next.run.GetSandboxRun() != 1 {
					t.Fatalf("the replacement's first run reads as run %d", next.run.GetSandboxRun())
				}
				if !gone && !next.run.GetWarm() {
					t.Fatalf("the member stayed, but the next run was not warm: %v", next.run)
				}
				t.Logf("next run on %s, warm %v, stats %v", next.run.GetSandboxId(), next.run.GetWarm(), p.getPool(pool).GetStats())
			})
		}
	})
}

// A member that died while idle must cost nobody a run: the client sees a normal result from
// another member, with the failed attempt on the audit trail. Without this a client pays for a
// crash that happened before it asked.
func TestPoolDeadMemberIsRetriedBeforeTheClientSeesIt(t *testing.T) {
	forEachPoolHost(t, func(t *testing.T, p *poolPlane) {
		p.declareOne("dead", leakPoolMemory, 1000)
		first := p.run("dead", p.tenant, shellRun("true", 10))
		first.mustComplete(t)
		dead := first.run.GetSandboxId()
		p.waitIdle("dead", 1)

		p.crash(t, dead)
		// Time for the host to notice the connection is gone, as the reused one would.
		time.Sleep(200 * time.Millisecond)

		res := p.run("dead", p.tenant, shellRun("echo alive", 10))
		res.mustComplete(t)
		if got := string(res.stdout); got != "alive\n" || res.exit.GetExitCode() != 0 || res.exit.GetReason() != v1.ExitReason_EXITED {
			t.Fatalf("the run failed: stdout %q, exit %v", got, res.exit)
		}
		if res.run.GetSandboxId() == dead {
			t.Fatalf("the run reads as landing on the dead member %s", dead)
		}
		code, data := p.call(http.MethodGet, "/v1/pools/dead/sandboxes/"+dead+"/execs", p.tenant, nil)
		if code != http.StatusOK {
			t.Fatalf("execs: %d %s", code, data)
		}
		execs := &v1.GetSandboxExecsResponse{}
		if err := protojson.Unmarshal(data, execs); err != nil {
			t.Fatal(err)
		}
		all := execs.GetExecs()
		if len(all) != 2 || all[1].GetReason() != v1.ExitReason_RUNTIME_FAILURE {
			t.Fatalf("records on the dead member = %v, want its good run and one RUNTIME_FAILURE", all)
		}
	})
}

// A workload that keeps forking and writing, with some of its processes in their own session
// holding its stdout.
const forkingWorkload = `
setsid sh -c 'while :; do : > /tmp/planted-setsid; sleep 0.01; done' &
setsid sh -c 'while :; do : > /workspace/planted-setsid; sleep 0.01; done' &
echo planted > /root/planted
echo planted > /dev/shm/planted
echo started
i=0
while :; do
	for j in 1 2 3 4; do
		( echo planted > /tmp/planted-$i-$j ) &
	done
	echo planted > /workspace/planted-$i
	wait $!
	i=$((i+1))
done
`

// disconnectRounds matches the 300 tries the hazard was measured over at the backend level:
// 198 leaked with an exec left live during a reset, and none once it was cancelled first.
const disconnectRounds = 300

// When a client disconnects, a run's exec has to be finished or cancelled before its reset,
// and none may start until the reset returns. This walks that through the whole path, at many
// moments of a run, on a pool of one so the very next run lands on the abandoned member. A
// client that skips the API and calls the runner directly is refused all the while.
func TestPoolDisconnectMidRun(t *testing.T) {
	forEachPoolHost(t, func(t *testing.T, p *poolPlane) {
		p.declareOne("solo", leakPoolMemory, 4*disconnectRounds)
		first := p.run("solo", p.tenant, runBody(p.probeProgram()))
		first.mustComplete(t)
		prepared := p.stateOf(first)
		p.mustMatch(prepared, prepared, "nothing")

		var member atomic.Value
		member.Store(first.run.GetSandboxId())
		stopPlain := p.refusePlainExecs(&member)
		defer stopPlain()

		moments := []struct {
			name  string
			lines int
			delay time.Duration
		}{
			{"before the first frame", 0, 0},
			{"before the first frame, a little later", 0, 3 * time.Millisecond},
			{"at the run frame", 1, 0},
			{"just after the run frame", 1, 2 * time.Millisecond},
			{"at the first output", 2, 0},
			{"well into the run", 2, 25 * time.Millisecond},
		}
		seen := map[string]bool{first.run.GetSandboxId(): true}
		current := first.run.GetSandboxId()
		var sawOutput, replaced, diedUnderProbe, reused int
		began := time.Now()
		for round := range disconnectRounds {
			m := moments[round%len(moments)]
			abandoned := p.abandonRun("solo", m.lines, m.delay+time.Duration(round%5)*time.Millisecond)
			if abandoned.sawOutput {
				sawOutput++
			}
			after := fmt.Sprintf("a disconnect %s (round %d)", m.name, round)
			next := p.runRetrying("solo", runBody(p.probeProgram()))
			if !p.host.disconnectKeepsMember && next.memberDied() {
				died := next.run.GetSandboxId()
				if died != current && seen[died] {
					t.Fatalf("%s: member %s came back after it was replaced", after, died)
				}
				if bytes.Contains(next.stdout, []byte("planted")) {
					t.Fatalf("%s: the probe died on %s, but first it read what the run left:\n%s", after, died, next.stdout)
				}
				if state, ok := parseProbe(next.stdout); ok {
					if died == current {
						p.mustMatch(prepared, state, after)
					} else {
						p.mustMatchFresh(prepared, state, after)
					}
				}
				t.Logf("%s: member %s died under the probe: exit %v, error line %q", after, died, next.exit, next.errLine)
				diedUnderProbe++
				seen[died] = true
				// Whatever the next probe lands on must be new.
				current = ""
				continue
			}
			next.mustComplete(t)
			got, state := next.run.GetSandboxId(), p.stateOf(next)
			if got == current {
				p.mustMatch(prepared, state, after)
				if abandoned.started {
					reused++
				}
				continue
			}
			if p.host.disconnectKeepsMember {
				t.Fatalf("%s replaced member %s with %s", after, current, got)
			}
			if seen[got] {
				t.Fatalf("%s: member %s came back after it was replaced", after, got)
			}
			p.mustMatchFresh(prepared, state, after)
			replaced++
			seen[got] = true
			current, prepared = got, state
			member.Store(got)
		}
		stats := p.getPool("solo").GetStats()
		t.Logf("%d disconnects in %v: %d saw output, %d replaced the member, %d members died under the probe, %d reused after a run known to have started, stats %v",
			disconnectRounds, time.Since(began), sawOutput, replaced, diedUnderProbe, reused, stats)
		if sawOutput < disconnectRounds/4 {
			t.Errorf("only %d of %d runs got as far as their output, so the disconnects tested little", sawOutput, disconnectRounds)
		}
		if reused < disconnectRounds/10 {
			t.Errorf("the member was reused after only %d of %d disconnects known to have started a run, so its reset was hardly tested", reused, disconnectRounds)
		}
		var killed, removed int
		for member := range seen {
			k, r := p.resetsOf("solo", member, 0)
			for i := range k {
				killed += k[i]
				removed += r[i]
			}
		}
		if killed == 0 || removed == 0 {
			t.Errorf("the resets of all %d members killed %d processes and removed %d entries, so there was nothing to leak", len(seen), killed, removed)
		}
	})
}

// memberDied reports a run that reached its member and found its sandbox going down. Usage can
// relabel a sandbox's death as the limit it hit, so those reasons count too.
func (r runResult) memberDied() bool {
	if r.status != http.StatusOK || r.run == nil {
		return false
	}
	switch r.exit.GetReason() {
	case v1.ExitReason_RUNTIME_FAILURE, v1.ExitReason_PIDS_LIMIT_EXCEEDED, v1.ExitReason_OOM_KILLED:
		return true
	}
	return r.errLine != ""
}

// abandonDeadline bounds how long a run may take to reach the point a disconnect is aimed at.
// Its own timeout is a minute, and a few runs waiting that out would hit the package's timeout
// as a panic.
const abandonDeadline = 20 * time.Second

// abandoned is how far an abandoned run got before its client left.
type abandoned struct {
	// started means the run frame arrived, so the member accepted the command.
	started bool
	// sawOutput means the run's first output arrived as well.
	sawOutput bool
}

// abandonRun starts a forking run, reads this many lines of it, waits, and disconnects.
func (p *poolPlane) abandonRun(pool string, lines int, delay time.Duration) abandoned {
	p.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), abandonDeadline)
	defer cancel()
	req := p.request(ctx, http.MethodPost, "/v1/pools/"+pool+"/run", p.tenant, shellRun(forkingWorkload, 60))
	if lines == 0 {
		time.AfterFunc(delay, cancel)
		if resp, err := http.DefaultClient.Do(req); err == nil {
			resp.Body.Close()
		}
		return abandoned{}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		p.t.Fatalf("run: no answer to read %d lines of: %v", lines, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return abandoned{}
	}
	scanner := bufio.NewScanner(resp.Body)
	var res runResult
	read := 0
	for read < lines && scanner.Scan() {
		res.absorb(p.t, scanner.Bytes(), 0)
		read++
	}
	if ctx.Err() != nil {
		p.t.Fatalf("run stalled: %d of %d lines in %v: %v", read, lines, abandonDeadline, res.frames)
	}
	time.Sleep(delay)
	return abandoned{started: res.run != nil, sawOutput: read == 2}
}

// refusePlainExecs calls the runner's ExecSandbox on the current member for as long as the
// test runs, and fails the test on anything but a refusal. It returns the function that stops.
func (p *poolPlane) refusePlainExecs(member *atomic.Value) (stop func()) {
	p.t.Helper()
	conn, err := grpc.NewClient(p.runnerAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		p.t.Fatalf("dial the runner: %v", err)
	}
	client := v1.NewRunnerServiceClient(conn)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		exec := &v1.ExecRequest{Command: "/bin/sh", Args: []string{"-c", "echo planted > /tmp/planted-plain"}, TimeoutSeconds: 5}
		for ctx.Err() == nil {
			stream, err := client.ExecSandbox(ctx, &v1.ExecSandboxRequest{SandboxId: member.Load().(string), Exec: exec})
			if err == nil {
				_, err = stream.Recv()
			}
			if code := status.Code(err); code != codes.FailedPrecondition && ctx.Err() == nil {
				p.t.Errorf("ExecSandbox on a pool member = %v, want FailedPrecondition", err)
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()
	return func() {
		cancel()
		<-done
		_ = conn.Close()
	}
}
