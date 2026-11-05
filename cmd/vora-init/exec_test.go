package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	v1 "github.com/vorash/vora/api/proto/v1"
)

// holderSeconds outlives every bound below, so a test that passes cannot have
// waited for the escaped process to exit on its own. It ends by itself shortly
// after the test.
const holderSeconds = "8"

type execResult struct {
	mu     sync.Mutex
	stdout bytes.Buffer
	exit   *v1.ExecExit
}

func (r *execResult) send(c *v1.ExecChunk) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stdout.Write(c.GetStdout())
	if e := c.GetExit(); e != nil {
		r.exit = e
	}
	return nil
}

func runAgentExec(t *testing.T, ctx context.Context, a *agent, req *v1.ExecRequest) (*execResult, time.Duration) {
	t.Helper()
	req.Command, req.Cwd = "/bin/sh", t.TempDir()
	res := &execResult{}
	start := time.Now()
	if err := a.Exec(ctx, req, res.send); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	return res, time.Since(start)
}

func testAgent() *agent {
	return newAgent("", time.Minute, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// A workload must not be able to outlive its timeout by moving a process out of
// the group the kill reaches. That process holds the pipes, so the agent used to
// read on until it exited, holding execMu against every other exec in the sandbox.
func TestExecTimeoutIgnoresEscapedPipeHolder(t *testing.T) {
	a := testAgent()
	res, took := runAgentExec(t, context.Background(), a, &v1.ExecRequest{
		Args:           []string{"-c", "setsid sleep " + holderSeconds + " & echo before; sleep " + holderSeconds},
		TimeoutSeconds: 1,
	})

	if took > 4*time.Second {
		t.Fatalf("exec with a 1s timeout took %v: it waited for the escaped process", took)
	}
	if res.exit == nil || res.exit.GetReason() != v1.ExitReason_TIMEOUT_EXCEEDED {
		t.Fatalf("exit = %v, want TIMEOUT_EXCEEDED", res.exit)
	}
	if !strings.Contains(res.stdout.String(), "before") {
		t.Fatalf("stdout = %q: output written before the kill was lost", res.stdout.String())
	}

	// execMu is free: the next exec does not queue behind the escaped process.
	next, took := runAgentExec(t, context.Background(), a, &v1.ExecRequest{Args: []string{"-c", "echo next"}})
	if took > 2*time.Second || next.stdout.String() != "next\n" {
		t.Fatalf("next exec took %v with stdout %q, want it to run at once", took, next.stdout.String())
	}
}

// The same holder when the caller goes away instead of the timeout firing.
func TestExecCancelIgnoresEscapedPipeHolder(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(200*time.Millisecond, cancel)
	_, took := runAgentExec(t, ctx, testAgent(), &v1.ExecRequest{
		Args: []string{"-c", "setsid sleep " + holderSeconds + " & sleep " + holderSeconds},
	})
	if took > 3*time.Second {
		t.Fatalf("cancelled exec took %v: it waited for the escaped process", took)
	}
}

// A holder of stdin alone leaves stdout at EOF, so the wait moves into cmd.Wait,
// which copies stdin. The stdin must exceed the pipe buffer for the copy to block.
func TestExecExitIgnoresEscapedStdinHolder(t *testing.T) {
	res, took := runAgentExec(t, context.Background(), testAgent(), &v1.ExecRequest{
		Args:  []string{"-c", "setsid sleep " + holderSeconds + " <&0 >/dev/null 2>&1 &"},
		Stdin: make([]byte, 1<<20),
	})
	if took > 4*time.Second {
		t.Fatalf("exec took %v: cmd.Wait waited for the stdin holder", took)
	}
	if res.exit == nil || res.exit.GetReason() != v1.ExitReason_EXITED || res.exit.GetExitCode() != 0 {
		t.Fatalf("exit = %v, want EXITED 0: the command itself succeeded", res.exit)
	}
}
