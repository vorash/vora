//go:build integration

package integration

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vorash/vora/internal/cgroup"
	"github.com/vorash/vora/internal/config"
	"github.com/vorash/vora/internal/runtime"
	"github.com/vorash/vora/internal/runtime/bwrap"
	"github.com/vorash/vora/internal/workspace"
)

// imageRootfs returns the cached rootfs for an image, skipping the test if the
// image has not been built.
func imageRootfs(t *testing.T, name string) string {
	t.Helper()
	stateHome := os.Getenv("XDG_STATE_HOME")
	if stateHome == "" {
		home, _ := os.UserHomeDir()
		stateHome = filepath.Join(home, ".local", "state")
	}
	rootfs := filepath.Join(stateHome, "vora", "images", name, "rootfs")
	if info, err := os.Stat(rootfs); err != nil || !info.IsDir() {
		t.Skipf("image %q not built (run 'make images'): %v", name, err)
	}
	return rootfs
}

func TestBwrapSandboxLifecycle(t *testing.T) {
	initBin := buildBinary(t, "vora-init")
	t.Setenv("VORA_INIT_BINARY", initBin)

	cgroups, err := cgroup.New(config.Config{SystemdMode: config.SystemdUser})
	if err != nil {
		t.Skipf("systemd user manager unavailable: %v", err)
	}
	rootfs := imageRootfs(t, "base")

	stateDir := t.TempDir()
	cfg := config.Config{StateDir: stateDir, SystemdMode: config.SystemdUser}
	wsManager := workspace.NewManager(cfg.SandboxesDir())
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	backend, err := bwrap.New(cfg, cgroups, logger)
	if err != nil {
		t.Fatalf("new backend: %v", err)
	}

	id := "sbx_" + strings.ReplaceAll(time.Now().UTC().Format("20060102150405.000000000"), ".", "")
	ws := wsManager.Paths(id)
	ws.RootfsDir = rootfs
	if _, err := wsManager.Create(ws); err != nil {
		t.Fatalf("workspace: %v", err)
	}
	t.Cleanup(func() { _ = wsManager.Destroy(id) })

	spec := runtime.SandboxSpec{
		ID:        id,
		Image:     "base",
		Resources: runtime.ResourceLimits{CPUMillis: 1000, MemoryBytes: 256 << 20, Pids: 64},
		Network:   runtime.NetworkPolicy{Mode: runtime.NetworkNone},
		TTL:       time.Minute,
		Workspace: ws,
	}

	ctx := context.Background()
	h, err := backend.Create(ctx, spec)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := backend.Start(ctx, h); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = backend.Destroy(ctx, h) })

	// First exec writes a file.
	run(t, backend, h, "/bin/sh", []string{"-c", "echo data > /workspace/marker.txt"})

	// Second exec sees the same filesystem state (sandbox is long-lived).
	out := run(t, backend, h, "/bin/sh", []string{"-c", "cat /workspace/marker.txt"})
	if strings.TrimSpace(out) != "data" {
		t.Fatalf("marker content = %q, want data", out)
	}

	// Python works inside the sandbox.
	out = run(t, backend, h, "/usr/bin/python3", []string{"-c", "print(1 + 1)"})
	if strings.TrimSpace(out) != "2" {
		t.Fatalf("python output = %q, want 2", out)
	}

	// Stats reflect the running sandbox.
	st, err := backend.Stats(ctx, h)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if st.MemoryBytes <= 0 {
		t.Fatalf("expected positive memory usage, got %+v", st)
	}

	// Destroy removes the cgroup and process tree.
	if err := backend.Destroy(ctx, h); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if _, err := backend.Stats(ctx, h); err == nil {
		t.Fatal("Stats after Destroy should fail")
	}
}

// run executes a command and returns its combined stdout.
func run(t *testing.T, backend *bwrap.Backend, h runtime.Handle, command string, args []string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	stream, err := backend.Exec(ctx, h, runtime.ExecRequest{
		ExecutionID: "exe_test",
		Command:     command,
		Args:        args,
		Cwd:         "/workspace",
	})
	if err != nil {
		t.Fatalf("Exec %s: %v", command, err)
	}
	defer stream.Close()

	var out strings.Builder
	for {
		chunk, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Recv %s: %v", command, err)
		}
		out.Write(chunk.Stderr)
		out.Write(chunk.Stdout)
	}
	return out.String()
}
