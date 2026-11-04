package main

import (
	"os"
	"strings"
	"testing"
)

// workloadCommand re-executes /proc/self/exe, which under `go test` is this
// test binary, so it has to dispatch the shim the way main does.
func TestMain(m *testing.M) {
	if len(os.Args) > 3 && os.Args[1] == workloadArg {
		runWorkload(os.Args[2], os.Args[3:])
	}
	os.Exit(m.Run())
}

func readOOMScoreAdj(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("/proc/self/oom_score_adj")
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(b))
}

// With systemd's OOMPolicy=continue the kernel kills one process per OOM, the
// one with the highest badness. A workload of many small processes would make
// the agent the largest, and killing it ends the sandbox. The workload, and
// what it forks, must carry the raised score; the agent must not.
func TestWorkloadIsTheOOMVictim(t *testing.T) {
	before := readOOMScoreAdj(t)
	if before == workloadOOMScoreAdj {
		t.Skipf("test process already runs at oom_score_adj %s; nothing to tell apart", before)
	}

	// cat is a child of sh, so this also shows the workload's own forks inherit it.
	out, err := workloadCommand("sh", []string{"-c", "cat /proc/self/oom_score_adj"}, t.TempDir()).Output()
	if err != nil {
		t.Fatalf("workload: %v", err)
	}
	if got := strings.TrimSpace(string(out)); got != workloadOOMScoreAdj {
		t.Fatalf("workload oom_score_adj = %s, want %s", got, workloadOOMScoreAdj)
	}
	if after := readOOMScoreAdj(t); after != before {
		t.Fatalf("agent oom_score_adj changed from %s to %s; only the workload may be raised", before, after)
	}
}

// A command that cannot be started must still fail Start, so the agent
// reports RUNTIME_FAILURE instead of the shim's exit code.
func TestWorkloadMissingCommandFailsStart(t *testing.T) {
	dir := t.TempDir()
	for _, command := range []string{"vora-no-such-command", "/no/such/command", "./no-such-command"} {
		if err := workloadCommand(command, nil, dir).Start(); err == nil {
			t.Errorf("Start(%q) succeeded, want a lookup error", command)
		}
	}
}
