//go:build integration

package integration

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	v1 "github.com/vorash/vora/api/proto/v1"
	"github.com/vorash/vora/internal/agentproto"
	"github.com/vorash/vora/internal/runtime"
)

func buildBinary(t *testing.T, pkg string) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), filepath.Base(pkg))
	cmd := exec.Command("go", "build", "-o", out, "./cmd/"+pkg)
	cmd.Dir = filepath.Join("..", "..")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build %s: %v\n%s", pkg, err, b)
	}
	return out
}

func TestVoraInitExecutesCommands(t *testing.T) {
	bin := buildBinary(t, "vora-init")
	dir := t.TempDir()
	socket := filepath.Join(dir, "control.sock")

	cmd := exec.Command(bin, "--socket", socket, "--grace", "5s")
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start vora-init: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})

	client, err := agentproto.Dial(socket)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := client.WaitReady(ctx, 5*time.Second); err != nil {
		t.Fatalf("WaitReady: %v", err)
	}

	stream, err := client.Exec(ctx, &v1.ExecRequest{
		Command: "/bin/echo",
		Args:    []string{"hello", "vora"},
		Cwd:     dir,
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	defer stream.Close()

	var stdout strings.Builder
	var exit *runtime.ExecResult
	for {
		chunk, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
		stdout.Write(chunk.Stdout)
		if chunk.Exit != nil {
			exit = chunk.Exit
		}
	}
	if got := strings.TrimSpace(stdout.String()); got != "hello vora" {
		t.Fatalf("stdout = %q, want %q", got, "hello vora")
	}
	if exit == nil || exit.ExitCode != 0 || exit.Reason != runtime.ReasonExited {
		t.Fatalf("exit = %+v", exit)
	}

	// A failing command reports its exit code.
	stream2, err := client.Exec(ctx, &v1.ExecRequest{Command: "/bin/false", Cwd: dir})
	if err != nil {
		t.Fatalf("Exec false: %v", err)
	}
	defer stream2.Close()
	for {
		chunk, err := stream2.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Recv false: %v", err)
		}
		if chunk.Exit != nil && chunk.Exit.ExitCode != 1 {
			t.Fatalf("false exit = %+v, want 1", chunk.Exit)
		}
	}

	if err := client.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("vora-init did not exit after Shutdown")
	}
}

// TestVoraInitPauseMode covers the gVisor init: `vora-init pause` must stay
// alive as PID 1 and exit cleanly on SIGTERM. It is what lets a shell-less
// image boot under gVisor (issue #40).
func TestVoraInitPauseMode(t *testing.T) {
	bin := buildBinary(t, "vora-init")
	cmd := exec.Command(bin, "pause")
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start vora-init pause: %v", err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	select {
	case err := <-exited:
		t.Fatalf("pause exited early: %v", err)
	case <-time.After(700 * time.Millisecond):
	}

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("signal: %v", err)
	}
	select {
	case err := <-exited:
		if err != nil {
			t.Fatalf("pause exited on SIGTERM with %v, want success", err)
		}
	case <-time.After(5 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("pause did not exit after SIGTERM")
	}
}
