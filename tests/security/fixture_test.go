//go:build security

// Package security is the isolation test suite. Every test creates a real
// sandbox through the normal runner/backend path and asserts a specific
// boundary. Each test documents why the boundary matters, so weakening the
// boundary breaks the test.
package security

import (
	"context"
	"errors"
	"fmt"
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
	"github.com/vorash/vora/internal/runtime/bwrap"
	"github.com/vorash/vora/internal/workspace"
)

const testSecret = "VORA_SECURITY_TEST_SECRET"

// TestMain fails the suite if any runtime units leak, making cleanup a tested
// guarantee rather than an assumption.
func TestMain(m *testing.M) {
	code := m.Run()

	// Units may still be deactivating when the last test returns; retry briefly
	// before declaring a leak.
	var residual []string
	for i := 0; i < 25; i++ {
		residual = leakedUnits()
		if len(residual) == 0 {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if len(residual) > 0 {
		fmt.Fprintf(os.Stderr, "security suite leaked runtime units: %v\n", residual)
		for _, u := range residual {
			_ = exec.Command("systemctl", "--user", "reset-failed", u).Run()
		}
		if code == 0 {
			code = 1
		}
	}
	os.Exit(code)
}

// leakedUnits lists vora-managed systemd services that are still active or
// failed (i.e. not cleanly gone).
func leakedUnits() []string {
	out, err := exec.Command("systemctl", "--user", "list-units", "--type=service", "--all", "--no-legend", "--plain", "--no-pager").Output()
	if err != nil {
		return nil
	}
	var units []string
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 || !strings.HasPrefix(fields[0], cgroup.UnitPrefix) {
			continue
		}
		switch fields[2] { // ACTIVE column
		case "active", "failed", "activating", "reloading":
			units = append(units, fields[0])
		}
	}
	return units
}

// fixture owns a backend and the sandboxes created by a test.
type fixture struct {
	t        *testing.T
	backend  *bwrap.Backend
	ws       *workspace.Manager
	cfg      config.Config
	initPath string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	initBin := os.Getenv("VORA_INIT_BINARY")
	if initBin == "" {
		initBin = mustBuildInit(t)
	}
	t.Setenv("VORA_INIT_BINARY", initBin)

	cgroups, err := cgroup.New(config.Config{SystemdMode: config.SystemdUser})
	if err != nil {
		t.Skipf("systemd user manager unavailable: %v", err)
	}
	cfg := config.Config{StateDir: t.TempDir(), SystemdMode: config.SystemdUser}
	backend, err := bwrap.New(cfg, cgroups, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("bwrap backend: %v", err)
	}
	return &fixture{
		t:        t,
		backend:  backend,
		ws:       workspace.NewManager(cfg.SandboxesDir()),
		cfg:      cfg,
		initPath: initBin,
	}
}

// create starts a sandbox with the given limits and returns its handle.
func (f *fixture) create(id string, limits runtime.ResourceLimits) runtime.Handle {
	f.t.Helper()
	rootfs := imageRootfs(f.t, "base")
	ws := f.ws.Paths(id)
	ws.RootfsDir = rootfs
	if _, err := f.ws.Create(ws); err != nil {
		f.t.Fatalf("workspace: %v", err)
	}
	spec := runtime.SandboxSpec{
		ID:        id,
		Image:     "base",
		Resources: limits,
		Network:   runtime.NetworkPolicy{Mode: runtime.NetworkNone},
		TTL:       time.Minute,
		Workspace: ws,
	}
	ctx := context.Background()
	h, err := f.backend.Create(ctx, spec)
	if err != nil {
		f.t.Fatalf("Create: %v", err)
	}
	if err := f.backend.Start(ctx, h); err != nil {
		f.t.Fatalf("Start: %v", err)
	}
	f.t.Cleanup(func() { _ = f.backend.Destroy(context.Background(), h) })
	return h
}

// exec runs a command and returns stdout, stderr and the exit code.
func (f *fixture) exec(h runtime.Handle, command string, args ...string) (string, string, int32) {
	f.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	stream, err := f.backend.Exec(ctx, h, runtime.ExecRequest{
		ExecutionID: "exe_sec",
		Command:     command,
		Args:        args,
		Cwd:         "/workspace",
	})
	if err != nil {
		f.t.Fatalf("Exec %s: %v", command, err)
	}
	defer stream.Close()

	var out, errb strings.Builder
	var code int32
	for {
		chunk, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return out.String(), errb.String(), code
		}
		if err != nil {
			f.t.Fatalf("recv: %v", err)
		}
		out.Write(chunk.Stdout)
		errb.Write(chunk.Stderr)
		if chunk.Exit != nil {
			code = int32(chunk.Exit.ExitCode)
		}
	}
}

func (f *fixture) stats(h runtime.Handle) *runtime.Stats {
	st, err := f.backend.Stats(context.Background(), h)
	if err != nil {
		f.t.Fatalf("Stats: %v", err)
	}
	return st
}

// waitFor polls cond until it returns true or the timeout elapses.
func (f *fixture) waitFor(timeout time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false
}

func limits(memMB, pids int64) runtime.ResourceLimits {
	return runtime.ResourceLimits{CPUMillis: 1000, MemoryBytes: memMB << 20, Pids: pids}
}

func imageRootfs(t *testing.T, name string) string {
	t.Helper()
	stateHome := os.Getenv("XDG_STATE_HOME")
	if stateHome == "" {
		home, _ := os.UserHomeDir()
		stateHome = filepath.Join(home, ".local", "state")
	}
	rootfs := filepath.Join(stateHome, "vora", "images", name, "rootfs")
	if info, err := os.Stat(rootfs); err != nil || !info.IsDir() {
		t.Skipf("image %q not built (run 'make images')", name)
	}
	return rootfs
}

func mustBuildInit(t *testing.T) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "vora-init")
	cmd := exec.Command("go", "build", "-o", out, "./cmd/vora-init")
	cmd.Dir = filepath.Join("..", "..")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build vora-init: %v\n%s", err, b)
	}
	return out
}
