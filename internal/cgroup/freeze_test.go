package cgroup

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// fakeCgroup is a directory with the files the freezer path reads and writes.
func fakeFreezer(t *testing.T, dir string, procs []int, frozen string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, pid := range procs {
		lines = append(lines, strconv.Itoa(pid))
	}
	writeFake(t, dir, "cgroup.procs", strings.Join(lines, "\n")+"\n")
	writeFake(t, dir, "cgroup.freeze", "0\n")
	writeFake(t, dir, "cgroup.events", "populated 1\nfrozen "+frozen+"\n")
}

func writeFake(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readFake(t *testing.T, dir, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(data))
}

// neverPID is a pid no process can have: the kernel allocates below pid_max.
func neverPID(t *testing.T) int {
	t.Helper()
	data, err := os.ReadFile("/proc/sys/kernel/pid_max")
	if err != nil {
		t.Skipf("pid_max: %v", err)
	}
	max, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatal(err)
	}
	return max
}

func TestProcsIsSortedAndFailsClosed(t *testing.T) {
	dir := t.TempDir()
	fakeFreezer(t, dir, []int{30, 10, 20}, "0")
	pids, err := Procs(dir)
	if err != nil || !slices.Equal(pids, []int{10, 20, 30}) {
		t.Fatalf("Procs = %v, %v", pids, err)
	}
	if _, err := Procs(filepath.Join(dir, "gone")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Procs of a missing cgroup = %v, want ErrNotFound", err)
	}
	writeFake(t, dir, "cgroup.procs", "12\nx\n")
	if _, err := Procs(dir); err == nil {
		t.Fatal("Procs parsed garbage")
	}
}

// A task in an uninterruptible wait holds the freeze off for as long as it
// likes. The caller's deadline must end the wait, and the cgroup must not be
// left frozen, or the destroy that follows would hang on it.
func TestKillOthersHonoursDeadlineAndThaws(t *testing.T) {
	dir := t.TempDir()
	fakeFreezer(t, dir, []int{os.Getpid()}, "0")
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := KillOthers(ctx, dir, []int{os.Getpid()})
	took := time.Since(start)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("KillOthers = %v, want the deadline", err)
	}
	if took > 500*time.Millisecond {
		t.Fatalf("KillOthers took %v past a 50ms deadline", took)
	}
	if got := readFake(t, dir, "cgroup.freeze"); got != "0" {
		t.Fatalf("cgroup.freeze = %q after a failed freeze, want 0", got)
	}
}

// A listed process that is gone before the kill is no failure, but one that
// stays listed is: the process set is not proven.
func TestKillOthersProvesTheProcessSet(t *testing.T) {
	dir := t.TempDir()
	keep := os.Getpid()
	fakeFreezer(t, dir, []int{keep, neverPID(t)}, "1")
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	killed, err := KillOthers(ctx, dir, []int{keep})
	if err == nil || !strings.Contains(err.Error(), "still present") {
		t.Fatalf("KillOthers = %d, %v, want a process still present", killed, err)
	}
	if killed != 0 {
		t.Fatalf("killed %d, want 0: the pid was already gone", killed)
	}
	if got := readFake(t, dir, "cgroup.freeze"); got != "0" {
		t.Fatalf("cgroup.freeze = %q after the kill, want 0", got)
	}
}

// sleeper starts a process this test owns and can check on.
func sleeper(t *testing.T) *exec.Cmd {
	t.Helper()
	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	return cmd
}

func ownCgroup(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		t.Skipf("/proc/self/cgroup: %v", err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		if rel, ok := strings.CutPrefix(line, "0::"); ok {
			return rel
		}
	}
	t.Skip("no cgroup v2 line")
	return ""
}

// The kill reaches a listed process only when the kernel says it is in this
// cgroup. A pid the list names but that lives elsewhere, as after a pid reuse,
// is left alone.
func TestKillOutsideSignalsOnlyMembers(t *testing.T) {
	root := t.TempDir()
	old := cgroupRoot
	cgroupRoot = root
	t.Cleanup(func() { cgroupRoot = old })
	rel := ownCgroup(t)

	member := sleeper(t)
	mine := filepath.Join(root, rel)
	fakeFreezer(t, mine, []int{os.Getpid(), member.Process.Pid, neverPID(t)}, "1")
	killed, err := killOutside(mine, []int{os.Getpid()})
	if err != nil || killed != 1 {
		t.Fatalf("killOutside = %d, %v, want 1 signalled", killed, err)
	}
	if err := member.Wait(); err == nil || !strings.Contains(err.Error(), "killed") {
		t.Fatalf("member exited with %v, want killed", err)
	}

	stranger := sleeper(t)
	other := filepath.Join(root, "elsewhere")
	fakeFreezer(t, other, []int{stranger.Process.Pid}, "1")
	killed, err = killOutside(other, nil)
	if err != nil || killed != 0 {
		t.Fatalf("killOutside = %d, %v, want nothing signalled", killed, err)
	}
	time.Sleep(50 * time.Millisecond)
	if err := syscall.Kill(stranger.Process.Pid, 0); err != nil {
		t.Fatalf("stranger is gone (%v): a process outside the cgroup was killed", err)
	}
}
