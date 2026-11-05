package bwrap

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/vorash/vora/internal/agentproto"
	"github.com/vorash/vora/internal/cgroup"
	"github.com/vorash/vora/internal/runtime"
)

// resettable is what Prepare returns for one bwrap sandbox. It keeps its own
// copies of what it needs, so a reset that outlives the handle's teardown fails
// on a closed client or a missing cgroup instead of reading a cleared handle.
type resettable struct {
	id     string
	client *agentproto.Client
	// dir is the sandbox's cgroup directory: the systemd unit's for network
	// none, the runner's subtree group for a sandbox in its own netns.
	dir      string
	manifest []byte
	// baseline is the host's view of the sandbox's init processes, sorted: the
	// outer bwrap, bwrap's PID 1 reaper and vora-init. The host kills every
	// other process in the cgroup and proves that only these remain.
	baseline []int
}

// Prepare implements runtime.Reusable. It records the sandbox's process
// baseline and captures its manifest through the connection Start made, then
// runs one full reset as an admission test: a sandbox that cannot be reset
// while fresh is not pooled.
func (b *Backend) Prepare(ctx context.Context, h runtime.Handle) (runtime.Resettable, error) {
	hd, err := b.handle(h)
	if err != nil {
		return nil, err
	}
	dir, err := hd.cgroupDir(ctx)
	if err != nil {
		return nil, fmt.Errorf("bwrap: prepare: %w", err)
	}
	client := hd.client
	if client == nil {
		return nil, fmt.Errorf("bwrap: prepare: sandbox %s is not running", hd.id)
	}
	// The connection's peer is the agent as the host sees it. A baseline that
	// does not hold it is some other cgroup's, and the kill would take the agent.
	peer, ok := client.Peer()
	if !ok {
		return nil, fmt.Errorf("bwrap: prepare: sandbox %s has no agent connection", hd.id)
	}
	baseline, err := cgroup.Procs(dir)
	if err != nil {
		return nil, fmt.Errorf("bwrap: prepare: %w", err)
	}
	if !slices.Contains(baseline, int(peer.Pid)) {
		return nil, fmt.Errorf("bwrap: prepare: agent pid %d is not in cgroup %s (%v)", peer.Pid, dir, baseline)
	}
	// The kill signals only what /proc places in this cgroup. Where /proc and the
	// mount disagree, as in a cgroup namespace over a non-namespaced mount, it would
	// signal nothing, and this sandbox's first run would be its last.
	held, err := b.holds(dir, int(peer.Pid))
	if err != nil {
		return nil, fmt.Errorf("bwrap: prepare: %w", err)
	}
	if !held {
		return nil, fmt.Errorf("bwrap: prepare: /proc does not place agent pid %d in cgroup %s", peer.Pid, dir)
	}
	manifest, err := client.Capture(ctx)
	if err != nil {
		return nil, fmt.Errorf("bwrap: prepare: capture: %w", err)
	}
	r := &resettable{id: hd.id, client: client, dir: dir, manifest: manifest, baseline: baseline}
	report, err := r.Reset(ctx)
	if err != nil {
		return nil, fmt.Errorf("bwrap: prepare: admission reset: %w", err)
	}
	// Nothing has run yet, so a reset that found something to do means the
	// baseline or the manifest is wrong, and this sandbox cannot be proven.
	if report.ProcessesKilled != 0 || report.EntriesRemoved != 0 {
		return nil, fmt.Errorf("bwrap: prepare: admission reset of a fresh sandbox killed %d processes and removed %d entries: %w",
			report.ProcessesKilled, report.EntriesRemoved, runtime.ErrUnproven)
	}
	return r, nil
}

// cgroupDir is the directory of the sandbox's cgroup.
func (hd *handle) cgroupDir(ctx context.Context) (string, error) {
	switch {
	case hd.group != nil:
		return hd.group.Path(), nil
	case hd.unit != nil:
		return hd.unit.Dir(ctx)
	default:
		return "", fmt.Errorf("sandbox %s is not running", hd.id)
	}
}

// Reset implements runtime.Resettable. The host kills and proves the process
// set, the agent restores the mounts and verifies from inside, and the host
// proves the process set again. Any failure wraps ErrUnproven.
func (r *resettable) Reset(ctx context.Context) (runtime.ResetReport, error) {
	report, err := r.reset(ctx)
	if err != nil {
		return runtime.ResetReport{}, fmt.Errorf("%w: bwrap %s: %w", runtime.ErrUnproven, r.id, err)
	}
	return report, nil
}

func (r *resettable) reset(ctx context.Context) (runtime.ResetReport, error) {
	killed, err := cgroup.KillOthers(ctx, r.dir, r.baseline)
	if err != nil {
		return runtime.ResetReport{}, err
	}
	if err := r.proveBaseline(); err != nil {
		return runtime.ResetReport{}, err
	}
	removed, err := r.client.Reset(ctx, r.manifest)
	if err != nil {
		return runtime.ResetReport{}, fmt.Errorf("agent reset: %w", err)
	}
	if err := r.proveBaseline(); err != nil {
		return runtime.ResetReport{}, err
	}
	return runtime.ResetReport{ProcessesKilled: killed, EntriesRemoved: removed}, nil
}

// proveBaseline checks that the cgroup holds exactly the baseline processes and
// that none of them is stopped. A workload can SIGSTOP the agent, which the
// freezer does not undo, and a stopped agent would hold the reset to its
// deadline for a sandbox that is going to be destroyed anyway.
func (r *resettable) proveBaseline() error {
	pids, err := cgroup.Procs(r.dir)
	if err != nil {
		return err
	}
	if !slices.Equal(pids, r.baseline) {
		return fmt.Errorf("cgroup holds %v, want %v", pids, r.baseline)
	}
	for _, pid := range r.baseline {
		state, err := processState(pid)
		if err != nil {
			return err
		}
		if state == 'T' || state == 't' {
			return fmt.Errorf("init process %d is stopped", pid)
		}
	}
	return nil
}

// processState is the state letter from /proc/<pid>/stat.
func processState(pid int) (byte, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, fmt.Errorf("read state of %d: %w", pid, err)
	}
	// The command name is in parentheses and may hold spaces, so the state is
	// what follows the last closing one.
	rest := data[strings.LastIndexByte(string(data), ')')+1:]
	fields := strings.Fields(string(rest))
	if len(fields) == 0 {
		return 0, fmt.Errorf("read state of %d: unparsable stat %q", pid, data)
	}
	return fields[0][0], nil
}

// Alive implements runtime.Resettable: one Ping on the connection Start made.
func (r *resettable) Alive(ctx context.Context) error {
	if _, err := r.client.Ping(ctx); err != nil {
		return fmt.Errorf("bwrap %s: %w", r.id, err)
	}
	return nil
}

var _ runtime.Reusable = (*Backend)(nil)
