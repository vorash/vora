package main

import (
	"bytes"
	"os/exec"
	"strings"
	"testing"
)

func openTestOOMScore(t *testing.T) *oomScore {
	t.Helper()
	o, err := openOOMScore()
	if err != nil {
		t.Fatalf("openOOMScore: %v", err)
	}
	t.Cleanup(func() { _ = o.file.Close() })
	if o.baseline == workloadOOMScoreAdj {
		t.Skipf("test process already runs at oom_score_adj %s; nothing to tell apart", o.baseline)
	}
	return o
}

func currentOOMScore(t *testing.T, o *oomScore) string {
	t.Helper()
	buf := make([]byte, 16)
	n, err := o.file.ReadAt(buf, 0)
	if n == 0 {
		t.Fatalf("read oom_score_adj: %v", err)
	}
	return strings.TrimSpace(string(buf[:n]))
}

// With systemd's OOMPolicy=continue the kernel kills one process per OOM, the
// one with the highest badness. A workload of many small processes would make
// the agent the largest, and killing it ends the sandbox. The workload, and
// what it forks, must carry the raised score; the agent must get its own back.
func TestWorkloadIsTheOOMVictim(t *testing.T) {
	o := openTestOOMScore(t)

	// cat is a child of sh, so this also shows the workload's own forks inherit it.
	cmd := exec.Command("sh", "-c", "cat /proc/self/oom_score_adj")
	var out bytes.Buffer
	cmd.Stdout = &out
	startErr, scoreErr := o.startWorkload(cmd)
	if startErr != nil || scoreErr != nil {
		t.Fatalf("startWorkload: start %v, score %v", startErr, scoreErr)
	}
	if got := currentOOMScore(t, o); got != o.baseline {
		t.Fatalf("agent oom_score_adj = %s after the fork, want its baseline %s", got, o.baseline)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("workload: %v", err)
	}
	if got := strings.TrimSpace(out.String()); got != workloadOOMScoreAdj {
		t.Fatalf("workload oom_score_adj = %s, want %s", got, workloadOOMScoreAdj)
	}
}

// A command that cannot start must not leave the agent as the OOM killer's first
// choice for the rest of the sandbox's life.
func TestFailedStartRestoresTheAgentScore(t *testing.T) {
	o := openTestOOMScore(t)
	startErr, scoreErr := o.startWorkload(exec.Command("/no/such/command"))
	if startErr == nil {
		t.Fatal("startWorkload of a missing command succeeded, want an error")
	}
	if scoreErr != nil {
		t.Fatalf("restore: %v", scoreErr)
	}
	if got := currentOOMScore(t, o); got != o.baseline {
		t.Fatalf("agent oom_score_adj = %s after a failed start, want its baseline %s", got, o.baseline)
	}
}

// An agent that could not open the file still runs commands.
func TestNilOOMScoreStillStarts(t *testing.T) {
	var o *oomScore
	cmd := exec.Command("true")
	if startErr, scoreErr := o.startWorkload(cmd); startErr != nil || scoreErr != nil {
		t.Fatalf("startWorkload: start %v, score %v", startErr, scoreErr)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("wait: %v", err)
	}
}
