package main

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
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
