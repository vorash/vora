package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/vorash/vora/internal/sandboxreset"
)

const (
	// resetTimeout bounds one Capture or Reset, including the wait for execMu. A
	// caller's shorter deadline still applies, and running out destroys the sandbox.
	resetTimeout = 30 * time.Second
	// settleTimeout bounds how long a killed process may take to disappear. It is
	// shorter than resetTimeout so that a survivor fails the reset promptly.
	settleTimeout = 5 * time.Second
	// settlePoll is how often the wait for execMu and for killed processes retries.
	settlePoll = 5 * time.Millisecond

	// initPID is the sandbox's PID 1: bwrap's reaper, or `vora-init pause`.
	initPID = 1
	// agentPID is the agent's PID under bwrap, whose reaper is PID 1.
	agentPID = 2
	// bwrapComm is PID 1's command name under bwrap.
	bwrapComm = "bwrap"
)

// resetter is what a capture or a reset does to the sandbox it runs in. The agent
// and the helper modes share it, and a test replaces every field that touches the
// machine.
type resetter struct {
	timeout time.Duration
	settle  time.Duration
	self    int
	// killOthers is nil in a helper, where the host has already killed the workload
	// and has proved it.
	killOthers    func() error
	reap          func()
	listPIDs      func() ([]int, error)
	openMountinfo func() (io.ReadCloser, error)
	capture       func(context.Context, io.Reader) (sandboxreset.Manifest, error)
	restore       func(context.Context, sandboxreset.Manifest, io.Reader) (int, error)
}

// agentResetter is the resetter of the bwrap agent, which kills what is left of a
// run because it has a PID namespace of its own to do that in.
func agentResetter() *resetter {
	r := helperResetter()
	r.killOthers = killOtherProcesses
	return r
}

// helperResetter is the resetter of `vora-init capture` and `vora-init reset`.
func helperResetter() *resetter {
	return &resetter{
		timeout:       resetTimeout,
		settle:        settleTimeout,
		self:          os.Getpid(),
		reap:          reapChildren,
		listPIDs:      listPIDs,
		openMountinfo: func() (io.ReadCloser, error) { return os.Open("/proc/self/mountinfo") },
		capture:       sandboxreset.Capture,
		restore:       sandboxreset.Restore,
	}
}

// killOtherProcesses is the only kill(-1) in this program. Outside a PID namespace
// it would kill every process the user owns, so it decides from the real PID and
// takes nothing a caller could stub, and no test may call it.
func killOtherProcesses() error {
	comm, err := os.ReadFile("/proc/1/comm")
	if err != nil {
		return fmt.Errorf("read PID 1's name: %w", err)
	}
	return guardedKill(os.Getpid(), strings.TrimSpace(string(comm)), syscall.Kill)
}

// guardedKill runs kill(-1, SIGKILL) only as PID 2 under a PID 1 named bwrap.
//
// PID 2 of the initial PID namespace is always kthreadd, so a user process that sees
// itself as PID 2 is in a PID namespace of its own, and kill(-1) cannot leave one. It
// spares PID 1 and the caller. The name rules out being PID 2 of some other container.
func guardedKill(pid int, initComm string, kill func(int, syscall.Signal) error) error {
	if pid != agentPID {
		return fmt.Errorf("refusing to kill every process: running as PID %d, not PID %d", pid, agentPID)
	}
	if initComm != bwrapComm {
		return fmt.Errorf("refusing to kill every process: PID 1 is %q, not %q", initComm, bwrapComm)
	}
	// ESRCH means nothing but this process and PID 1 was left to signal.
	if err := kill(-1, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("kill every process: %w", err)
	}
	return nil
}

// captureManifest lists the sandbox from a fresh read of the mount table.
func (r *resetter) captureManifest(ctx context.Context) ([]byte, error) {
	mountinfo, err := r.openMountinfo()
	if err != nil {
		return nil, fmt.Errorf("capture: %w", err)
	}
	defer func() { _ = mountinfo.Close() }()
	m, err := r.capture(ctx, mountinfo)
	if err != nil {
		return nil, err
	}
	return json.Marshal(m)
}

// reset kills what is left of the run, proves that nothing is, and only then lets
// Restore remove files. It returns how many entries Restore removed.
func (r *resetter) reset(ctx context.Context, rawManifest []byte) (int, error) {
	var m sandboxreset.Manifest
	if err := json.Unmarshal(rawManifest, &m); err != nil {
		return 0, fmt.Errorf("reset: parse manifest: %w", err)
	}
	if r.killOthers != nil {
		if err := r.killOthers(); err != nil {
			return 0, fmt.Errorf("reset: %w", err)
		}
	}
	if err := r.proveOnlyInitAndSelf(ctx); err != nil {
		return 0, fmt.Errorf("reset: %w", err)
	}
	mountinfo, err := r.openMountinfo()
	if err != nil {
		return 0, fmt.Errorf("reset: %w", err)
	}
	defer func() { _ = mountinfo.Close() }()
	removed, err := r.restore(ctx, m, mountinfo)
	if err != nil {
		return 0, fmt.Errorf("reset: %w", err)
	}
	return removed, nil
}

// proveOnlyInitAndSelf waits for /proc to list PID 1, this process and nothing else.
// A killed process stays listed until its parent collects it, and that parent is PID 1
// for anything reparented, so a zombie gets the same wait as a live process and fails
// the same way if it stays.
func (r *resetter) proveOnlyInitAndSelf(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, r.settle)
	defer cancel()
	for {
		r.reap()
		pids, err := r.listPIDs()
		if err != nil {
			return fmt.Errorf("list processes: %w", err)
		}
		// A /proc that hides either of these is no proof that the rest are gone.
		if !slices.Contains(pids, initPID) || !slices.Contains(pids, r.self) {
			return fmt.Errorf("/proc does not list both PID %d and this process (PID %d)", initPID, r.self)
		}
		others := slices.DeleteFunc(pids, func(pid int) bool { return pid == initPID || pid == r.self })
		if len(others) == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("processes still present: %s", describeProcesses(others))
		case <-time.After(settlePoll):
		}
	}
}

func listPIDs() ([]int, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	var pids []int
	for _, e := range entries {
		if pid, err := strconv.Atoi(e.Name()); err == nil {
			pids = append(pids, pid)
		}
	}
	return pids, nil
}

// describeProcesses names a few processes in an error. It shows each one's /proc stat
// prefix, which holds its name and state, so a zombie can be told from a live process.
func describeProcesses(pids []int) string {
	const shown, statPrefix = 5, 48
	var out []string
	for _, pid := range pids[:min(shown, len(pids))] {
		stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		if err != nil {
			out = append(out, strconv.Itoa(pid))
			continue
		}
		out = append(out, strconv.Quote(string(stat[:min(statPrefix, len(stat))])))
	}
	if len(pids) > shown {
		out = append(out, fmt.Sprintf("and %d more", len(pids)-shown))
	}
	return strings.Join(out, ", ")
}

// lockExec takes execMu, or fails once ctx ends. sync.Mutex cannot wait with a
// deadline, and an exec can hold it for as long as its output stays open.
func (a *agent) lockExec(ctx context.Context) error {
	for !a.execMu.TryLock() {
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for the running exec: %w", ctx.Err())
		case <-time.After(settlePoll):
		}
	}
	if err := ctx.Err(); err != nil {
		a.execMu.Unlock()
		return err
	}
	return nil
}

// Capture implements agentproto.Handler. It stores nothing, so calling it from
// inside the sandbox only hands the caller a listing it could have made itself.
func (a *agent) Capture(ctx context.Context) ([]byte, error) {
	a.touch()
	ctx, cancel := context.WithTimeout(ctx, a.resetter.timeout)
	defer cancel()
	if err := a.lockExec(ctx); err != nil {
		return nil, fmt.Errorf("capture: %w", err)
	}
	defer a.execMu.Unlock()
	return a.resetter.captureManifest(ctx)
}

// Reset implements agentproto.Handler. The manifest comes with the call and is not
// kept, so a call from inside the sandbox cannot change what a later reset compares
// against, and validation ties it to the live mount table, so it can only remove what
// the caller could remove itself.
func (a *agent) Reset(ctx context.Context, manifest []byte) (int, error) {
	a.touch()
	arrived := a.resets.Load()
	ctx, cancel := context.WithTimeout(ctx, a.resetter.timeout)
	defer cancel()
	if err := a.lockExec(ctx); err != nil {
		return 0, fmt.Errorf("reset: %w", err)
	}
	defer a.execMu.Unlock()
	// Running now would kill the next run, or remove files from it.
	if a.resets.Load() != arrived {
		return 0, errors.New("reset: was queued across another reset")
	}
	defer a.resets.Add(1)
	return a.resetter.reset(ctx, manifest)
}

// resetReport is what `vora-init reset` prints on success. A failure prints nothing
// on stdout, so a report that does not parse and a nonzero exit say the same thing.
type resetReport struct {
	Removed int `json:"removed"`
}

// runCaptureMode is `vora-init capture`: the manifest on stdout, exit 0, or a message
// on stderr and exit 1.
func runCaptureMode(ctx context.Context, r *resetter, stdout, stderr io.Writer) int {
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	manifest, err := r.captureManifest(ctx)
	if err == nil {
		_, err = stdout.Write(manifest)
	}
	return helperExit(err, stderr)
}

// runResetMode is `vora-init reset`: the manifest on stdin, the report on stdout and
// exit 0, or a message on stderr and exit 1. It proves the process set itself and
// kills nothing, because the host has already killed the workload.
func runResetMode(ctx context.Context, r *resetter, stdin io.Reader, stdout, stderr io.Writer) int {
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	manifest, err := io.ReadAll(stdin)
	if err != nil {
		return helperExit(fmt.Errorf("read manifest: %w", err), stderr)
	}
	removed, err := r.reset(ctx, manifest)
	if err != nil {
		return helperExit(err, stderr)
	}
	report, err := json.Marshal(resetReport{Removed: removed})
	if err == nil {
		_, err = fmt.Fprintf(stdout, "%s\n", report)
	}
	return helperExit(err, stderr)
}

func helperExit(err error, stderr io.Writer) int {
	if err == nil {
		return 0
	}
	_, _ = fmt.Fprintln(stderr, "vora-init:", err)
	return 1
}
