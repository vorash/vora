package cgroup

import (
	"context"
	"errors"
	"fmt"
	"io"
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
	// pollSlice is the longest a wait goes without checking its context.
	pollSlice = 100 * time.Millisecond
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
	events, err := os.Open(filepath.Join(dir, "cgroup.events"))
	if err != nil {
		return fmt.Errorf("cgroup: freeze: %w", notFound(dir, err))
	}
	defer func() { _ = events.Close() }()
	ctx, cancel := context.WithTimeout(ctx, freezeTimeout)
	defer cancel()
	for {
		// A read arms the next poll: kernfs reports a change only against the
		// file's state at its last read.
		frozen, err := readFrozen(events)
		if err != nil {
			return fmt.Errorf("cgroup: freeze: %w", err)
		}
		if frozen {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("cgroup: not frozen: %w", err)
		}
		if err := pollChange(events, pollSlice); err != nil {
			return fmt.Errorf("cgroup: freeze: %w", err)
		}
	}
}

func readFrozen(events *os.File) (bool, error) {
	if _, err := events.Seek(0, io.SeekStart); err != nil {
		return false, err
	}
	data, err := io.ReadAll(events)
	if err != nil {
		return false, err
	}
	for _, line := range strings.Split(string(data), "\n") {
		if fields := strings.Fields(line); len(fields) == 2 && fields[0] == "frozen" {
			return fields[1] == "1", nil
		}
	}
	return false, errors.New("cgroup.events has no frozen line")
}

// pollChange waits until the kernel notifies a change to a cgroup file or the
// slice passes. A regular file, as a test uses, is always ready without ever
// reporting a change, so that case re-reads at a walking pace instead.
func pollChange(f *os.File, slice time.Duration) error {
	fds := []unix.PollFd{{Fd: int32(f.Fd()), Events: unix.POLLPRI}}
	for {
		_, err := unix.Poll(fds, int(slice/time.Millisecond))
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return err
		}
		if fds[0].Revents&unix.POLLPRI == 0 {
			time.Sleep(time.Millisecond)
		}
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
	rel := cgroupRel(dir)
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
		member, err := inCgroup(pid, rel)
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

// cgroupRel is dir as /proc/<pid>/cgroup names it: relative to the cgroup
// mount, with a leading slash.
func cgroupRel(dir string) string {
	rel := strings.TrimPrefix(dir, cgroupRoot)
	if rel == "" {
		return "/"
	}
	return rel
}

// inCgroup reports whether pid's cgroup, as this process sees it, is rel. A pid
// that is gone is not a member.
func inCgroup(pid int, rel string) (bool, error) {
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
	delay := 200 * time.Microsecond
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
		select {
		case <-ctx.Done():
			return fmt.Errorf("cgroup: processes still present: %v, want %v: %w", pids, want, ctx.Err())
		case <-time.After(delay):
			delay = min(delay*2, 5*time.Millisecond)
		}
	}
}
