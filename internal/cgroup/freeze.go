package cgroup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

const (
	// freezeTimeout bounds the wait for a cgroup to report itself frozen. A healthy
	// cgroup freezes in microseconds; a task in an uninterruptible wait holds the
	// freeze off until it returns, and the caller's sandbox is destroyed instead.
	freezeTimeout = 2 * time.Second
	// settleTimeout bounds how long killed processes may take to leave cgroup.procs.
	settleTimeout = 5 * time.Second
	// firstDelay and maxDelay pace a poll on a cgroup file.
	firstDelay = 200 * time.Microsecond
	maxDelay   = 5 * time.Millisecond
)

// Procs lists the processes in the cgroup at dir, sorted. Threads are not listed.
// It returns ErrNotFound when the cgroup is gone.
func Procs(dir string) ([]int, error) {
	data, err := os.ReadFile(filepath.Join(dir, "cgroup.procs"))
	if err != nil {
		return nil, notFound(dir, err)
	}
	var pids []int
	for _, line := range strings.Split(string(data), "\n") {
		if line == "" {
			continue
		}
		pid, err := strconv.Atoi(line)
		if err != nil {
			return nil, fmt.Errorf("cgroup: parse cgroup.procs line %q", line)
		}
		pids = append(pids, pid)
	}
	slices.Sort(pids)
	return pids, nil
}

// notFound turns a missing cgroup file into ErrNotFound, which means the
// sandbox is gone.
func notFound(dir string, err error) error {
	if os.IsNotExist(err) {
		return fmt.Errorf("%w: %s", ErrNotFound, dir)
	}
	return err
}

// Freeze freezes the cgroup at dir and waits until the kernel reports every task
// in it frozen. A frozen task cannot fork, so cgroup.procs read after this is the
// complete process set until Thaw. On any error the cgroup may still be marked
// frozen, so the caller thaws.
func Freeze(ctx context.Context, dir string) error {
	if err := writeCgroupFile(dir, "cgroup.freeze", "1"); err != nil {
		return fmt.Errorf("cgroup: freeze: %w", notFound(dir, err))
	}
	ctx, cancel := context.WithTimeout(ctx, freezeTimeout)
	defer cancel()
	delay := firstDelay
	for {
		frozen, err := readFrozen(dir)
		if err != nil {
			return fmt.Errorf("cgroup: freeze: %w", err)
		}
		if frozen {
			return nil
		}
		if err := pause(ctx, &delay); err != nil {
			return fmt.Errorf("cgroup: not frozen: %w", err)
		}
	}
}

func readFrozen(dir string) (bool, error) {
	data, err := os.ReadFile(filepath.Join(dir, "cgroup.events"))
	if err != nil {
		return false, notFound(dir, err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		if fields := strings.Fields(line); len(fields) == 2 && fields[0] == "frozen" {
			return fields[1] == "1", nil
		}
	}
	return false, errors.New("cgroup.events has no frozen line")
}

// pause waits out one step of a poll on a cgroup file, doubling delay up to
// maxDelay. It fails once ctx ends.
func pause(ctx context.Context, delay *time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(*delay):
		*delay = min(*delay*2, maxDelay)
		return nil
	}
}

// Thaw unfreezes the cgroup at dir. A cgroup that is gone needs no thaw: the
// open fails with ENOENT, or the write with ENODEV if it went between the two.
func Thaw(dir string) error {
	err := writeCgroupFile(dir, "cgroup.freeze", "0")
	if err == nil || os.IsNotExist(err) || errors.Is(err, unix.ENODEV) {
		return nil
	}
	return fmt.Errorf("cgroup: thaw: %w", err)
}

// KillOthers kills every process in the cgroup at dir that is not in keep, and
// waits until cgroup.procs lists exactly keep. It freezes the cgroup around the
// kill, so nothing can fork between the list and the signals and no pid in the
// list can be reused, and it thaws on every path. Each signal goes through a
// pidfd to a process it has confirmed is in this cgroup, so no process outside
// it can be hit. It returns how many processes it signalled. Any error means
// the process set is not proven.
func KillOthers(ctx context.Context, dir string, keep []int) (int, error) {
	killed, err := killFrozen(ctx, dir, keep)
	if err != nil {
		return killed, err
	}
	return killed, waitProcs(ctx, dir, keep)
}

func killFrozen(ctx context.Context, dir string, keep []int) (killed int, err error) {
	defer func() {
		if thawErr := Thaw(dir); err == nil {
			err = thawErr
		}
	}()
	if err := Freeze(ctx, dir); err != nil {
		return 0, err
	}
	return killOutside(dir, keep)
}

// killOutside signals every listed process not in keep. The caller holds the
// cgroup frozen.
func killOutside(dir string, keep []int) (int, error) {
	pids, err := Procs(dir)
	if err != nil {
		return 0, err
	}
	killed := 0
	for _, pid := range pids {
		if slices.Contains(keep, pid) {
			continue
		}
		fd, err := unix.PidfdOpen(pid, 0)
		if errors.Is(err, unix.ESRCH) {
			continue
		}
		if err != nil {
			return killed, fmt.Errorf("cgroup: pidfd_open %d: %w", pid, err)
		}
		// The pidfd pins the task the pid named when it was opened. If the pid
		// has since been reused, the cgroup read below is of the new task and
		// does not match, so nothing is sent.
		member, err := Holds(dir, pid)
		if err != nil {
			_ = unix.Close(fd)
			return killed, err
		}
		if !member {
			_ = unix.Close(fd)
			continue
		}
		err = unix.PidfdSendSignal(fd, unix.SIGKILL, nil, 0)
		_ = unix.Close(fd)
		if err != nil && !errors.Is(err, unix.ESRCH) {
			return killed, fmt.Errorf("cgroup: kill %d: %w", pid, err)
		}
		killed++
	}
	return killed, nil
}

// Holds reports whether the kernel places pid in the cgroup at dir, by comparing
// /proc/<pid>/cgroup with dir's path under the mount. A pid that is gone is not
// held. In a cgroup namespace over a non-namespaced mount the two paths differ
// for every process, so callers that rely on it check a known member first.
func Holds(dir string, pid int) (bool, error) {
	rel := strings.TrimPrefix(dir, cgroupRoot)
	if rel == "" {
		rel = "/"
	}
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cgroup", pid))
	if err != nil {
		if os.IsNotExist(err) || errors.Is(err, unix.ESRCH) {
			return false, nil
		}
		return false, fmt.Errorf("cgroup: read cgroup of %d: %w", pid, err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		if path, ok := strings.CutPrefix(line, "0::"); ok {
			return path == rel, nil
		}
	}
	return false, fmt.Errorf("cgroup: no cgroup v2 line for %d", pid)
}

// waitProcs waits until cgroup.procs lists exactly keep.
func waitProcs(ctx context.Context, dir string, keep []int) error {
	ctx, cancel := context.WithTimeout(ctx, settleTimeout)
	defer cancel()
	want := slices.Sorted(slices.Values(keep))
	delay := firstDelay
	for {
		pids, err := Procs(dir)
		if err != nil {
			return err
		}
		if slices.Equal(pids, want) {
			return nil
		}
		// A kept process that is gone cannot come back, so waiting proves nothing.
		for _, pid := range want {
			if !slices.Contains(pids, pid) {
				return fmt.Errorf("cgroup: process %d is gone: cgroup holds %v, want %v", pid, pids, want)
			}
		}
		if err := pause(ctx, &delay); err != nil {
			return fmt.Errorf("cgroup: processes still present: %v, want %v: %w", pids, want, err)
		}
	}
}
