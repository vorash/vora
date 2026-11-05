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
	"sync"
	"syscall"
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

func drainExec(ctx context.Context, client *agentproto.Client, command string, args ...string) error {
	stream, err := client.Exec(ctx, &v1.ExecRequest{Command: command, Args: args, Cwd: "/"})
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
// thread above this is a task the workload cannot have.
const maxAgentThreads = 9

// The agent must have every thread it will ever need by the time it is ready. A
// workload can use up the pids limit at any moment, and from then on a Go program
// that needs a new thread crashes with "fatal error: newosproc", ending the sandbox.
//
// GOMAXPROCS stands in for a big host, so this fails the same way on any machine.
// The load is what makes the agent reach for a thread: signals, an exec blocked in
// wait, a fork, heavy output, and RPCs arriving meanwhile.
func TestAgentNeedsNoThreadsAfterStartup(t *testing.T) {
	cmd, client := startAgentProcess(t, 64)
	ready := threadsOf(t, cmd.Process.Pid)
	t.Logf("agent ready with %d threads", ready)
	if ready > maxAgentThreads {
		t.Fatalf("agent has %d threads when ready, want at most %d", ready, maxAgentThreads)
	}

	ctx := t.Context()
	stop := make(chan struct{})
	var rpcs sync.WaitGroup
	rpcs.Go(func() {
		for {
			select {
			case <-stop:
				return
			default:
				_, _ = client.Ping(ctx)
				_ = client.Signal(ctx, int32(syscall.SIGCONT))
			}
		}
	})
	defer func() {
		close(stop)
		rpcs.Wait()
	}()

	check := func(when string) {
		t.Helper()
		if n := threadsOf(t, cmd.Process.Pid); n > ready {
			t.Fatalf("agent went from %d threads to %d %s, and that new thread would have crashed it at the pids limit", ready, n, when)
		}
	}
	load := "for i in 1 2 3 4 5 6 7 8; do true & done; wait; head -c 4194304 /dev/zero; sleep 0.02"
	for i := range 150 {
		if err := drainExec(ctx, client, "/bin/sh", "-c", load); err != nil {
			t.Fatalf("exec %d: %v", i, err)
		}
		check(fmt.Sprintf("after %d execs", i+1))
	}
}
