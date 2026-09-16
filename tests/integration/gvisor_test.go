//go:build integration

package integration

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/vorash/vora/internal/config"
	"github.com/vorash/vora/internal/runtime"
	"github.com/vorash/vora/internal/runtime/gvisor"
	"github.com/vorash/vora/internal/workspace"
)

// TestGvisorBackendLifecycle exercises the direct-runsc backend. It must run
// inside a persistent rootlesskit user namespace (gVisor Method 2) or as root;
// otherwise it is skipped.
func TestGvisorBackendLifecycle(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("gVisor Method 2 requires a rootlesskit userns or root (run under rootlesskit)")
	}
	if _, err := os.Stat("/dev/kvm"); err != nil {
		t.Skipf("/dev/kvm unavailable: %v", err)
	}
	rootfs := imageRootfs(t, "base")

	cfg := config.Config{StateDir: t.TempDir(), GvisorPlatform: "kvm"}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	backend, err := gvisor.New(cfg, logger)
	if err != nil {
		t.Skipf("gvisor backend unavailable: %v", err)
	}

	id := "sbx_gv_" + strings.ReplaceAll(time.Now().UTC().Format("150405.000000000"), ".", "")
	ws := workspace.NewManager(cfg.SandboxesDir())
	wsp := ws.Paths(id)
	wsp.RootfsDir = rootfs
	if _, err := ws.Create(wsp); err != nil {
		t.Fatalf("workspace: %v", err)
	}
	t.Cleanup(func() { _ = ws.Destroy(id) })

	spec := runtime.SandboxSpec{
		ID:        id,
		Image:     "base",
		Resources: runtime.ResourceLimits{CPUMillis: 1000, MemoryBytes: 256 << 20, Pids: 64},
		Network:   runtime.NetworkPolicy{Mode: runtime.NetworkNone},
		TTL:       time.Minute,
		Workspace: wsp,
	}

	ctx := context.Background()
	h, err := backend.Create(ctx, spec)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := backend.Start(ctx, h); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = backend.Destroy(context.Background(), h) })

	// First exec writes a file; the second must see it (persistent sandbox).
	runGvisor(t, backend, h, "/bin/sh", []string{"-c", "echo persisted > /workspace/gv.txt"})
	out := runGvisor(t, backend, h, "/bin/sh", []string{"-c", "cat /workspace/gv.txt"})
	if strings.TrimSpace(out) != "persisted" {
		t.Fatalf("second exec output = %q, want persisted", out)
	}

	// Exit codes propagate.
	if code := runGvisorCode(t, backend, h, "/bin/sh", []string{"-c", "exit 7"}); code != 7 {
		t.Fatalf("exit code = %d, want 7", code)
	}

	if err := backend.Destroy(ctx, h); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
}

func runGvisor(t *testing.T, b *gvisor.Backend, h runtime.Handle, command string, args []string) string {
	t.Helper()
	out, _ := execGvisor(t, b, h, command, args)
	return out
}

func runGvisorCode(t *testing.T, b *gvisor.Backend, h runtime.Handle, command string, args []string) int {
	t.Helper()
	_, code := execGvisor(t, b, h, command, args)
	return code
}

func execGvisor(t *testing.T, b *gvisor.Backend, h runtime.Handle, command string, args []string) (string, int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	stream, err := b.Exec(ctx, h, runtime.ExecRequest{Command: command, Args: args, Cwd: "/workspace"})
	if err != nil {
		t.Fatalf("Exec %s: %v", command, err)
	}
	defer stream.Close()

	var out strings.Builder
	code := 0
	for {
		chunk, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return out.String(), code
		}
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
		out.Write(chunk.Stderr)
		out.Write(chunk.Stdout)
		if chunk.Exit != nil {
			code = chunk.Exit.ExitCode
		}
	}
}
