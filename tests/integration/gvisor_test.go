//go:build integration

package integration

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vorash/vora/internal/cgroup"
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

	cfg := config.Config{StateDir: t.TempDir(), GvisorPlatform: "kvm", GvisorRunRoot: filepath.Join(os.Getenv("XDG_RUNTIME_DIR"), "vora-gvisor-itest")}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	subtree, err := cgroup.NewSubtree()
	if err != nil {
		t.Skipf("cgroup subtree unavailable: %v", err)
	}
	backend, err := gvisor.New(cfg, logger, subtree)
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

// TestGvisorShellLessImage starts a sandbox whose rootfs has no shell at all.
// The init is Vora's own binary, so the image never needs /bin/sh (issue #40):
// before the fix this failed with "failed to load /bin/sh".
func TestGvisorShellLessImage(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("gVisor Method 2 requires a rootlesskit userns or root (run under rootlesskit)")
	}
	// Use the platform the host can run: systrap needs no /dev/kvm, which is
	// what a KVM-less host (for example a mini-PC) has.
	platform := "systrap"
	if _, err := os.Stat("/dev/kvm"); err == nil {
		platform = "kvm"
	}

	cfg := config.Config{
		StateDir:       t.TempDir(),
		GvisorPlatform: platform,
		GvisorRunRoot:  filepath.Join(os.Getenv("XDG_RUNTIME_DIR"), "vora-gvisor-shellless"),
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	subtree, err := cgroup.NewSubtree()
	if err != nil {
		t.Skipf("cgroup subtree unavailable: %v", err)
	}
	backend, err := gvisor.New(cfg, logger, subtree)
	if err != nil {
		t.Skipf("gvisor backend unavailable: %v", err)
	}

	// A rootfs with no shell: mount points plus one static binary.
	rootfs := t.TempDir()
	for _, d := range []string{"workspace", "root", "vora", "tmp", "proc", "dev", "etc"} {
		if err := os.MkdirAll(filepath.Join(rootfs, d), 0o755); err != nil {
			t.Fatalf("rootfs %s: %v", d, err)
		}
	}
	if _, err := os.Stat(filepath.Join(rootfs, "bin/sh")); err == nil {
		t.Fatal("test rootfs unexpectedly has a shell")
	}
	buildHello(t, filepath.Join(rootfs, "hello"))

	id := "sbx_gv_shellless_" + strings.ReplaceAll(time.Now().UTC().Format("150405.000000000"), ".", "")
	ws := workspace.NewManager(cfg.SandboxesDir())
	wsp := ws.Paths(id)
	wsp.RootfsDir = rootfs
	if _, err := ws.Create(wsp); err != nil {
		t.Fatalf("workspace: %v", err)
	}
	t.Cleanup(func() { _ = ws.Destroy(id) })

	spec := runtime.SandboxSpec{
		ID:        id,
		Image:     "shell-less",
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
		t.Fatalf("Start shell-less image: %v", err)
	}
	t.Cleanup(func() { _ = backend.Destroy(context.Background(), h) })

	// exec runs with no shell anywhere in the sandbox.
	if got := strings.TrimSpace(runGvisor(t, backend, h, "/hello", nil)); got != "shell-less-ok" {
		t.Fatalf("exec output = %q, want shell-less-ok", got)
	}
}

// buildHello compiles a tiny static binary so the shell-less rootfs can run a
// command without any shell or libc.
func buildHello(t *testing.T, out string) {
	t.Helper()
	src := filepath.Join(t.TempDir(), "main.go")
	const prog = "package main\n\nimport \"fmt\"\n\nfunc main() { fmt.Println(\"shell-less-ok\") }\n"
	if err := os.WriteFile(src, []byte(prog), 0o644); err != nil {
		t.Fatalf("write hello source: %v", err)
	}
	cmd := exec.Command("go", "build", "-o", out, src)
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build hello: %v\n%s", err, b)
	}
}
