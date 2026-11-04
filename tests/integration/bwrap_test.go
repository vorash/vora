//go:build integration

package integration

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/vorash/vora/internal/cgroup"
	"github.com/vorash/vora/internal/config"
	"github.com/vorash/vora/internal/runtime"
	"github.com/vorash/vora/internal/runtime/bwrap"
	"github.com/vorash/vora/internal/ulid"
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
	rootfs := filepath.Join(stateHome, "vora", "images", name, "latest", "rootfs")
	if info, err := os.Stat(rootfs); err != nil || !info.IsDir() {
		t.Skipf("image %q not built (run 'make images'): %v", name, err)
	}
	return rootfs
}

func TestBwrapSandboxLifecycle(t *testing.T) {
	backend, h := newBwrapSandbox(t)
	ctx := context.Background()

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

// idlePastGrace outlasts vora-init's watchdog grace. The runner does not pass
// --grace, so the agent runs with its 20 s default.
const idlePastGrace = 30 * time.Second

// A client may leave a sandbox idle between calls for longer than vora-init's
// watchdog grace. The runner must keep it alive, so the next exec still finds
// the agent (vorash/vora#113).
func TestBwrapIdleOutlivesGrace(t *testing.T) {
	backend, h := newBwrapSandbox(t)
	time.Sleep(idlePastGrace)
	if out := strings.TrimSpace(run(t, backend, h, "/bin/echo", []string{"alive"})); out != "alive" {
		t.Fatalf("exec after %s idle = %q, want alive", idlePastGrace, out)
	}
}

// newBwrapSandbox starts a bwrap sandbox on the cached base image and returns
// the backend and its handle, tearing the sandbox down at the end of the test.
func newBwrapSandbox(t *testing.T) (*bwrap.Backend, runtime.Handle) {
	t.Helper()
	return newBwrapSandboxWithEnv(t, nil)
}

func newBwrapSandboxWithEnv(t *testing.T, env map[string]string) (*bwrap.Backend, runtime.Handle) {
	t.Helper()

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

	id := ulid.NewSandbox()
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
		Env:       env,
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
	return backend, h
}

// Exec output must survive a command that writes and exits immediately. The
// in-sandbox agent drains stdout/stderr in goroutines, and it used to call
// cmd.Wait() before waiting for them: Go's Wait closes the read ends of the
// pipes StdoutPipe/StderrPipe handed out, so anything still unread — the tail of
// a burst, or all of a fast echo — was discarded while the exit chunk still
// arrived reporting success.
func TestBwrapExecDoesNotLoseOutput(t *testing.T) {
	backend, h := newBwrapSandbox(t)

	// Still draining when the writer exits: the pipe holds more than the reader
	// has taken, which is exactly what Wait-on-top-of-it throws away. Repeated,
	// because the window is narrow — a single burst only trips it about a third
	// of the time — so the run as a whole is what has to be reliable.
	const want = 1 << 20
	for i := range 8 {
		out := execStdout(t, backend, h, "/bin/sh", []string{"-c", "yes vora | head -c " + strconv.Itoa(want)})
		if len(out) != want {
			t.Fatalf("burst %d: stdout = %d bytes, want %d: output was truncated by the exit path", i, len(out), want)
		}
		if !strings.HasPrefix(out, "vora\nvora\n") {
			t.Fatalf("burst %d: stdout starts with %q, want the repeated 'vora' pattern", i, out[:min(32, len(out))])
		}
	}

	// The shape seen in the wild: a tiny command that exits at once.
	for i := range 25 {
		if got := execStdout(t, backend, h, "/bin/sh", []string{"-c", "echo api-e2e"}); got != "api-e2e\n" {
			t.Fatalf("iteration %d: stdout = %q, want %q", i, got, "api-e2e\n")
		}
	}
}

// execStdout runs a command and returns its stdout only.
func execStdout(t *testing.T, backend *bwrap.Backend, h runtime.Handle, command string, args []string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	stream, err := backend.Exec(ctx, h, runtime.ExecRequest{
		ExecutionID: "exe_output",
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
		out.Write(chunk.Stdout)
	}
	return out.String()
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

// Tenant env values must reach the sandbox byte for byte. systemd expands
// ${VAR} and $$ in the arguments of a transient service, so an unguarded
// FOO=${HOME} handed the tenant the host service manager's environment.
func TestBwrapEnvNotExpanded(t *testing.T) {
	backend, h := newBwrapSandboxWithEnv(t, map[string]string{
		"VORA_LEAK": "${HOME}",
		"VORA_COST": "$$5",
	})
	for name, want := range map[string]string{"VORA_LEAK": "${HOME}", "VORA_COST": "$$5"} {
		got := execStdout(t, backend, h, "/bin/sh", []string{"-c", `printf %s "$` + name + `"`})
		if got != want {
			t.Errorf("%s = %q, want %q: the service manager rewrote a tenant env value", name, got, want)
		}
	}
}
