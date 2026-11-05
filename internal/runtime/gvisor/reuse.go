package gvisor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"time"

	"github.com/vorash/vora/internal/runtime"
	"github.com/vorash/vora/internal/sandboxreset"
)

const (
	// killPasses bounds the host kill. A pass lists the sandbox and then signals each
	// process in turn, and guest code keeps running meanwhile, so one pass is not exact:
	// a process can fork between the list and its kill. Only a listing of PID 1 alone ends
	// the loop, because nothing is left then that could start anything.
	killPasses = 10
	// runscWaitDelay is how long a runsc call may outlive its context or its own exit
	// while something still holds its pipes. It stays under the pool's CallGrace.
	runscWaitDelay = 100 * time.Millisecond
	// initPID is `vora-init pause`, the one process that outlives a run.
	initPID = 1
)

var _ runtime.Reusable = (*Backend)(nil)

// resettable is one prepared sandbox. The pool may destroy the sandbox while a call it
// gave up on is still running, so this holds nothing Destroy changes: such a call only
// finds runsc refusing a container that is gone.
type resettable struct {
	b        *Backend
	id       string
	manifest []byte
	host     []hostListing
}

// hostListing is what a writable bind mount held at capture, as the host sees it.
type hostListing struct {
	dir   string
	names map[string]bool
}

// Prepare implements runtime.Reusable. A workload must not have run yet: the capture is
// the state every later reset returns to.
func (b *Backend) Prepare(ctx context.Context, h runtime.Handle) (runtime.Resettable, error) {
	hd, err := b.handle(h)
	if err != nil {
		return nil, err
	}
	r := &resettable{b: b, id: hd.id}
	pids, err := r.processes(ctx)
	if err != nil {
		return nil, fmt.Errorf("gvisor: prepare: %w", err)
	}
	if others := withoutInit(pids); len(others) > 0 {
		return nil, fmt.Errorf("gvisor: prepare: processes already running before capture: %v", others)
	}
	if r.manifest, err = b.helper(ctx, hd.id, nil, "capture"); err != nil {
		return nil, fmt.Errorf("gvisor: prepare: %w", err)
	}
	var m sandboxreset.Manifest
	if err := json.Unmarshal(r.manifest, &m); err != nil {
		return nil, fmt.Errorf("gvisor: prepare: parse manifest: %w", err)
	}
	ws := hd.spec.Workspace
	if r.host, err = hostListings(m, map[string]string{"/workspace": ws.WorkspaceDir, "/root": ws.HomeDir}); err != nil {
		return nil, fmt.Errorf("gvisor: prepare: %w", err)
	}
	if _, err := r.Reset(ctx); err != nil {
		return nil, fmt.Errorf("gvisor: admission reset: %w", err)
	}
	return r, nil
}

// Reset implements runtime.Resettable.
func (r *resettable) Reset(ctx context.Context) (runtime.ResetReport, error) {
	report, err := r.reset(ctx)
	if err != nil {
		return runtime.ResetReport{}, fmt.Errorf("%w: gvisor: %w", runtime.ErrUnproven, err)
	}
	return report, nil
}

// reset never trusts guest code before the host has proved that none but init exists,
// so the helper it then starts is the only other process, and it comes from the read-only
// bind at /vora.
func (r *resettable) reset(ctx context.Context) (runtime.ResetReport, error) {
	killed, err := r.killWorkload(ctx)
	if err != nil {
		return runtime.ResetReport{}, err
	}
	out, err := r.b.helper(ctx, r.id, r.manifest, "reset")
	if err != nil {
		return runtime.ResetReport{}, err
	}
	removed, err := parseResetReport(out)
	if err != nil {
		return runtime.ResetReport{}, err
	}
	if err := r.checkHost(); err != nil {
		return runtime.ResetReport{}, err
	}
	pids, err := r.processes(ctx)
	if err != nil {
		return runtime.ResetReport{}, err
	}
	if others := withoutInit(pids); len(others) > 0 {
		return runtime.ResetReport{}, fmt.Errorf("processes present after the helper: %v", others)
	}
	return runtime.ResetReport{ProcessesKilled: killed, EntriesRemoved: removed}, nil
}

// Alive implements runtime.Resettable. It asks the Sentry rather than `runsc state`, which
// costs the same and says nothing about PID 1.
func (r *resettable) Alive(ctx context.Context) error {
	if _, err := r.processes(ctx); err != nil {
		return fmt.Errorf("gvisor: %w", err)
	}
	return nil
}

// killWorkload kills every process but init from the host, so no guest code has to run
// and a workload that holds every task slot cannot stop it. It returns how many distinct
// processes it signalled. A failed kill is not an error: the target may have died with
// its parent, and the next listing decides.
func (r *resettable) killWorkload(ctx context.Context) (int, error) {
	killed := map[int]bool{}
	for pass := 0; ; pass++ {
		pids, err := r.processes(ctx)
		if err != nil {
			return 0, err
		}
		others := withoutInit(pids)
		if len(others) == 0 {
			return len(killed), nil
		}
		if pass == killPasses {
			return 0, fmt.Errorf("processes survived %d kill passes: %v", killPasses, others)
		}
		for _, pid := range others {
			if _, err := r.b.call(ctx, nil, "kill", "-pid", strconv.Itoa(pid), "--", r.id, "KILL"); err == nil {
				killed[pid] = true
			}
		}
	}
}

// processes lists the sandbox's thread-group leaders in its root PID namespace, nested
// namespaces included. An answer without PID 1 is no proof of anything.
func (r *resettable) processes(ctx context.Context) ([]int, error) {
	out, err := r.b.call(ctx, nil, "ps", "--format", "json", "--", r.id)
	if err != nil {
		return nil, err
	}
	var pids []int
	if err := json.Unmarshal(out, &pids); err != nil {
		return nil, fmt.Errorf("parse runsc ps output %q: %w", clip(out), err)
	}
	if !slices.Contains(pids, initPID) {
		return nil, fmt.Errorf("runsc ps does not list PID %d: %v", initPID, pids)
	}
	return pids, nil
}

func withoutInit(pids []int) []int {
	return slices.DeleteFunc(slices.Clone(pids), func(pid int) bool { return pid == initPID })
}

// checkHost compares the host's view of the bind mounts with the capture, so a Sentry
// whose cached view disagrees with the disk cannot pass as clean. Nothing runs in the
// sandbox now, and WalkDir does not follow links.
func (r *resettable) checkHost() error {
	for _, want := range r.host {
		got := map[string]bool{}
		err := filepath.WalkDir(want.dir, func(path string, _ fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(want.dir, path)
			if err != nil {
				return err
			}
			got[rel] = true
			return nil
		})
		if err != nil {
			return fmt.Errorf("host check: %w", err)
		}
		if !maps.Equal(got, want.names) {
			return fmt.Errorf("host check: %s holds %d entries, want %d", want.dir, len(got), len(want.names))
		}
	}
	return nil
}

// hostListings pairs each bind mount's listing with its directory on the host.
func hostListings(m sandboxreset.Manifest, dirs map[string]string) ([]hostListing, error) {
	var out []hostListing
	for _, inside := range slices.Sorted(maps.Keys(dirs)) {
		i := slices.IndexFunc(m.Mounts, func(l sandboxreset.MountListing) bool { return l.Path == inside })
		if i < 0 {
			return nil, fmt.Errorf("manifest does not list %s", inside)
		}
		names := make(map[string]bool, len(m.Mounts[i].Entries))
		for _, e := range m.Mounts[i].Entries {
			names[e.Rel] = true
		}
		out = append(out, hostListing{dir: dirs[inside], names: names})
	}
	return out, nil
}

// parseResetReport reads what `vora-init reset` prints. Exit 0 alone proves nothing: the
// report must be complete and be the only thing on stdout.
func parseResetReport(out []byte) (int, error) {
	dec := json.NewDecoder(bytes.NewReader(out))
	dec.DisallowUnknownFields()
	var report struct {
		Removed *int `json:"removed"`
	}
	if err := dec.Decode(&report); err != nil {
		return 0, fmt.Errorf("parse helper report %q: %w", clip(out), err)
	}
	if dec.More() {
		return 0, fmt.Errorf("helper report %q has trailing output", clip(out))
	}
	if report.Removed == nil || *report.Removed < 0 {
		return 0, fmt.Errorf("helper report %q has no removed count", clip(out))
	}
	return *report.Removed, nil
}

// helper runs a `vora-init` helper mode. The working directory is / because the sandbox's
// own, /workspace, is a mount the helper is about to walk.
func (b *Backend) helper(ctx context.Context, id string, stdin []byte, mode string) ([]byte, error) {
	return b.call(ctx, stdin, "exec", "-cwd", "/", "--", id, initInside, mode)
}

// call runs one runsc subcommand and returns its stdout. Unlike run, it can feed stdin,
// keeps stdout apart from stderr, and returns soon after ctx ends even if runsc is stuck
// on a stopped Sentry or a child of it keeps the pipes open. Those pipes staying open past
// a successful exit fails the call too, because something is still running.
func (b *Backend) call(ctx context.Context, stdin []byte, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, b.runsc, append(b.globalFlags("none"), args...)...)
	cmd.WaitDelay = runscWaitDelay
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			err = errors.Join(ctxErr, err)
		}
		return nil, fmt.Errorf("runsc %s: %w: %s", args[0], err, clip(stderr.Bytes()))
	}
	return stdout.Bytes(), nil
}

// clip keeps an error message short when a tool prints a lot.
func clip(b []byte) string {
	const limit = 512
	if len(b) > limit {
		return string(b[:limit]) + "..."
	}
	return string(b)
}
