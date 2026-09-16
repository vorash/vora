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
