package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func ownOOMScore(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("/proc/self/oom_score_adj")
	if err != nil {
		t.Fatalf("read oom_score_adj: %v", err)
	}
	s := strings.TrimSpace(string(b))
	if s == workloadOOMScoreAdj {
		t.Skipf("test process already runs at oom_score_adj %s; nothing to tell apart", s)
	}
	return s
}

// With systemd's OOMPolicy=continue the kernel kills one process per OOM, the
// one with the highest badness. A workload of many small processes would make
// the agent the largest, and killing it ends the sandbox. The workload, and
// what it forks, must carry the raised score, and the agent's own must never
// move: an OOM during a fork that stalls at the memory limit picks whoever is
// largest at the raised score, which is the agent.
func TestWorkloadIsTheOOMVictim(t *testing.T) {
	baseline := ownOOMScore(t)

	// The child waits for stdin so the read happens after the raise, and cat is
	// a child of sh, so this also shows the workload's own forks inherit it.
	cmd := exec.Command("sh", "-c", "read go; cat /proc/self/oom_score_adj")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	cmd.Stdout = &out
	startErr, scoreErr := startWorkload(cmd)
	if startErr != nil || scoreErr != nil {
		t.Fatalf("startWorkload: start %v, score %v", startErr, scoreErr)
	}
	if got := ownOOMScore(t); got != baseline {
		t.Fatalf("agent oom_score_adj = %s after the start, want its baseline %s", got, baseline)
	}
	if _, err := io.WriteString(stdin, "\n"); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("workload: %v", err)
	}
	if got := strings.TrimSpace(out.String()); got != workloadOOMScoreAdj {
		t.Fatalf("workload oom_score_adj = %s, want %s", got, workloadOOMScoreAdj)
	}
}

// A command that is gone before its score can be raised is not an error: the
// exec ran, and there is nothing left to pick.
func TestWorkloadGoneBeforeTheRaise(t *testing.T) {
	cmd := exec.Command("true")
	if startErr, scoreErr := startWorkload(cmd); startErr != nil || scoreErr != nil {
		t.Fatalf("startWorkload: start %v, score %v", startErr, scoreErr)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("wait: %v", err)
	}
}

// A command that cannot start reports that and nothing about scores.
func TestFailedStartHasNoScore(t *testing.T) {
	startErr, scoreErr := startWorkload(exec.Command("/no/such/command"))
	if startErr == nil {
		t.Fatal("startWorkload of a missing command succeeded, want an error")
	}
	if scoreErr != nil {
		t.Fatalf("score error on a failed start: %v", scoreErr)
	}
}

// nonDumpableLikeTheAgent makes this process what main makes the agent, for one test.
func nonDumpableLikeTheAgent(t *testing.T) {
	t.Helper()
	if err := setNonDumpable(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := unix.Prctl(unix.PR_SET_DUMPABLE, 1, 0, 0, 0); err != nil {
			t.Fatal(err)
		}
	})
}

// The real agent is non-dumpable, and a child of a non-dumpable process keeps root-owned
// /proc files until the very end of its exec, which is after Start returns. Losing that
// race left the workload at the agent's score, and under load it lost it most of the time.
//
// Sharing one CPU with the child stands in for the load: the close that ends Start wakes
// this thread, which then runs while the child is still inside its exec.
func TestRaiseOutlastsTheEndOfTheExec(t *testing.T) {
	goruntime.LockOSThread()
	defer goruntime.UnlockOSThread()
	var all, one unix.CPUSet
	if err := unix.SchedGetaffinity(0, &all); err != nil {
		t.Fatal(err)
	}
	for cpu := range 1024 {
		if all.IsSet(cpu) {
			one.Set(cpu)
			break
		}
	}
	if err := unix.SchedSetaffinity(0, &one); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := unix.SchedSetaffinity(0, &all); err != nil {
			t.Fatal(err)
		}
	}()
	nonDumpableLikeTheAgent(t)
	for i := range 200 {
		cmd := exec.Command("sleep", "60")
		startErr, scoreErr := startWorkload(cmd)
		if startErr != nil || scoreErr != nil {
			t.Fatalf("start %d: start %v, score %v", i, startErr, scoreErr)
		}
		score, err := os.ReadFile(fmt.Sprintf("/proc/%d/oom_score_adj", cmd.Process.Pid))
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		if err != nil || strings.TrimSpace(string(score)) != workloadOOMScoreAdj {
			t.Fatalf("start %d: oom_score_adj = %q (%v), want %s", i, score, err, workloadOOMScoreAdj)
		}
	}
}

// Running a binary it cannot read leaves a process non-dumpable for good. The wait for
// the end of its exec has to end anyway, and the failure is still reported.
func TestChildThatStaysUnwritableIsReported(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can read the binary and write the score")
	}
	path, err := exec.LookPath("sleep")
	if err != nil {
		t.Fatal(err)
	}
	image, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	unreadable := filepath.Join(t.TempDir(), "sleep")
	if err := os.WriteFile(unreadable, image, 0o111); err != nil {
		t.Fatal(err)
	}
	// A dumpable parent's child is writable until late in its exec, so the raise could land.
	nonDumpableLikeTheAgent(t)
	cmd := exec.Command(unreadable, "60")
	startErr, scoreErr := startWorkload(cmd)
	if startErr != nil {
		t.Fatalf("start: %v", startErr)
	}
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	if !errors.Is(scoreErr, fs.ErrPermission) {
		t.Fatalf("score error = %v, want a permission error", scoreErr)
	}
}
