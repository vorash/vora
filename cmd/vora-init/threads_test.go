package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	v1 "github.com/vorash/vora/api/proto/v1"
	"github.com/vorash/vora/internal/agentproto"
)

const runAgentEnv = "VORA_INIT_TEST_RUN_AGENT"

// TestMain lets a test start this binary as a real agent process, seccomp filter
// and all, so it can count that process's own threads.
func TestMain(m *testing.M) {
	if os.Getenv(runAgentEnv) != "" {
		main()
		return
	}
	os.Exit(m.Run())
}

// startAgentProcess runs the agent the way a sandbox does. A big host's default
// is what scales the scheduler, so the caller sets GOMAXPROCS to stand in for one.
func startAgentProcess(t *testing.T, gomaxprocs int) (*exec.Cmd, *agentproto.Client) {
	t.Helper()
	socket := filepath.Join(t.TempDir(), "control.sock")
	cmd := exec.Command(os.Args[0], "-socket", socket, "-grace", "1m")
	cmd.Env = []string{runAgentEnv + "=1", "GOMAXPROCS=" + strconv.Itoa(gomaxprocs)}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start agent: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	client, err := agentproto.Dial(socket)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	if err := client.WaitReady(context.Background(), 10*time.Second); err != nil {
		t.Fatalf("agent not ready: %v", err)
	}
	return cmd, client
}

func runTrue(ctx context.Context, client *agentproto.Client) error {
	stream, err := client.Exec(ctx, &v1.ExecRequest{Command: "/bin/true", Cwd: "/"})
	if err != nil {
		return err
	}
	defer stream.Close()
	for {
		chunk, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if chunk.Exit != nil && chunk.Exit.ExitCode != 0 {
			return fmt.Errorf("exit %d", chunk.Exit.ExitCode)
		}
	}
}

func threadsOf(t *testing.T, pid int) int {
	t.Helper()
	status, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		t.Fatalf("read status: %v", err)
	}
	for line := range strings.Lines(string(status)) {
		if v, ok := strings.CutPrefix(line, "Threads:"); ok {
			n, err := strconv.Atoi(strings.TrimSpace(v))
			if err != nil {
				t.Fatalf("parse %q: %v", line, err)
			}
			return n
		}
	}
	t.Fatal("no Threads line")
	return 0
}

// maxAgentThreads is what the agent may take out of a sandbox's pids budget. Every
// thread counts against the limit, so each one above this is a task the workload
// cannot have, and Go never gives a thread back once it has started it.
const maxAgentThreads = 8

// The agent's threads used to grow with the host's core count, over the execs a
// sandbox runs, until a limit that worked on a small host left a bigger host's
// workload unable to fork. GOMAXPROCS stands in for the bigger host, so this fails
// the same way on any machine.
func TestAgentThreadsDoNotGrowWithHostCores(t *testing.T) {
	cmd, client := startAgentProcess(t, 64)
	for i := range 300 {
		if err := runTrue(t.Context(), client); err != nil {
			t.Fatalf("exec %d: %v", i, err)
		}
		if n := threadsOf(t, cmd.Process.Pid); n > maxAgentThreads {
			t.Fatalf("agent has %d threads after %d execs, want at most %d", n, i+1, maxAgentThreads)
		}
	}
}
