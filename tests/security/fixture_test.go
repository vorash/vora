//go:build security

// Package security is the isolation test suite. Every test creates a real
// sandbox through the normal runner/backend path and asserts a specific
// boundary. Each test documents why the boundary matters, so weakening it
// breaks the test.
//
// Tests run against every available backend (bwrap and gVisor) as subtests.
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
	"github.com/vorash/vora/internal/runtime/gvisor"
	"github.com/vorash/vora/internal/workspace"
)

const testSecret = "VORA_SECURITY_TEST_SECRET"

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// backendCase is one isolation backend under test.
type backendCase struct {
	name      string
	available func(*testing.T) bool
	new       func(*testing.T, config.Config) runtime.Backend
}

func backends() []backendCase {
	return []backendCase{
		{
			name:      "bwrap",
			available: func(*testing.T) bool { return true },
			new: func(t *testing.T, cfg config.Config) runtime.Backend {
				m, err := cgroup.New(cfg)
				if err != nil {
					t.Skipf("cgroup manager unavailable: %v", err)
				}
				b, err := bwrap.New(cfg, m, discardLogger())
				if err != nil {
					t.Skipf("bwrap backend unavailable: %v", err)
				}
				return b
			},
		},
		{
			name: "gvisor",
			available: func(*testing.T) bool {
				if _, err := os.Stat("/dev/kvm"); err != nil {
					return false
				}
				if !runscPresent() {
					return false
				}
				// gVisor Method 2 needs root or a rootlesskit user namespace.
				return os.Getuid() == 0
			},
			new: func(t *testing.T, cfg config.Config) runtime.Backend {
				subtree, err := cgroup.NewSubtree()
				if err != nil {
					t.Skipf("cgroup subtree unavailable: %v", err)
				}
				b, err := gvisor.New(cfg, discardLogger(), subtree)
				if err != nil {
					t.Skipf("gvisor backend unavailable: %v", err)
				}
				return b
			},
		},
	}
}

func runscPresent() bool {
	if _, err := exec.LookPath("runsc"); err == nil {
		return true
	}
	if home, err := os.UserHomeDir(); err == nil {
		if _, err := os.Stat(filepath.Join(home, ".local", "bin", "runsc")); err == nil {
			return true
		}
	}
	return false
}

// forEachBackend runs fn against every available backend as a subtest.
func forEachBackend(t *testing.T, fn func(*testing.T, *fixture)) {
	t.Helper()
	for _, bc := range backends() {
		t.Run(bc.name, func(t *testing.T) {
			if !bc.available(t) {
				t.Skipf("backend %s unavailable on this host (needs runsc+KVM+uid 0 for gVisor)", bc.name)
			}
			f := newFixture(t, bc)
			fn(t, f)
		})
	}
}

// fixture owns a backend and the sandboxes created by a test.
type fixture struct {
	t       *testing.T
	backend runtime.Backend
	ws      *workspace.Manager
	cfg     config.Config
}

func newFixture(t *testing.T, bc backendCase) *fixture {
	t.Helper()
	if os.Getenv("VORA_INIT_BINARY") == "" {
		// bwrap needs vora-init; harmless for gVisor.
		t.Setenv("VORA_INIT_BINARY", mustBuildInit(t))
	}
	cfg := config.Config{
		StateDir:       t.TempDir(),
		SystemdMode:    config.SystemdUser,
		GvisorPlatform: "kvm",
		GvisorRunRoot:  gvisorTestRoot(),
	}
	return &fixture{t: t, backend: bc.new(t, cfg), ws: workspace.NewManager(cfg.SandboxesDir()), cfg: cfg}
}

// create starts a sandbox with the given limits and returns its handle.
func (f *fixture) create(id string, limits runtime.ResourceLimits) runtime.Handle {
	f.t.Helper()
	ws := f.ws.Paths(id)
	ws.RootfsDir = imageRootfs(f.t, "base")
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
	f.t.Cleanup(func() {
		_ = f.backend.Destroy(context.Background(), h)
		_ = f.ws.Destroy(id)
	})
	return h
}

// exec runs a command and returns stdout, stderr and the exit code.
func (f *fixture) exec(h runtime.Handle, command string, args ...string) (string, string, int32) {
	f.t.Helper()
	out, errb, code, err := f.execStream(h, command, args...)
	if err != nil {
		f.t.Fatalf("recv: %v", err)
	}
	return out, errb, code
}

// execAllowDisconnect is exec, but a dropped stream (OOM killing the agent)
// is a non-zero exit rather than a test failure.
func (f *fixture) execAllowDisconnect(h runtime.Handle, command string, args ...string) (string, string, int32) {
	f.t.Helper()
	out, errb, code, err := f.execStream(h, command, args...)
	if err != nil {
		return out, errb, -1
	}
	return out, errb, code
}

func (f *fixture) execStream(h runtime.Handle, command string, args ...string) (string, string, int32, error) {
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
			return out.String(), errb.String(), code, nil
		}
		if err != nil {
			return out.String(), errb.String(), code, err
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

// gvisorTestRoot is a per-process runsc state root, so tests do not share state
// with a running vora-runner service (two runsc instances must not share a root).
func gvisorTestRoot() string {
	rd := os.Getenv("XDG_RUNTIME_DIR")
	if rd == "" {
		rd = os.TempDir()
	}
	return filepath.Join(rd, fmt.Sprintf("vora-gvisor-sec-%d", os.Getpid()))
}

// cleanupGvisorTestRoot best-effort unmounts and removes the test runsc root.
func cleanupGvisorTestRoot() {
	root := gvisorTestRoot()
	_ = exec.Command("umount", "-l", filepath.Join(root, "null-netns")).Run()
	_ = os.RemoveAll(root)
}

func imageRootfs(t *testing.T, name string) string {
	t.Helper()
	stateHome := os.Getenv("XDG_STATE_HOME")
	if stateHome == "" {
		home, _ := os.UserHomeDir()
		stateHome = filepath.Join(home, ".local", "state")
	}
	rootfs := filepath.Join(stateHome, "vora", "images", name, "latest", "rootfs")
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

// TestMain fails the suite if any runtime units leak, making cleanup a tested
// guarantee rather than an assumption.
func TestMain(m *testing.M) {
	// The integration suite runs before this one in CI and shares the systemd
	// user session, so units it left behind must not be attributed to this
	// suite: only units created *during this run* count as leaks.
	preexisting := leakedUnitSet()

	code := m.Run()

	// Units may still be deactivating when the last test returns; retry briefly
	// before declaring a leak.
	var residual []string
	for i := 0; i < 25; i++ {
		residual = newLeakedUnits(preexisting)
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
	cleanupGvisorTestRoot()
	os.Exit(code)
}

// leakedUnitSet snapshots the current sandbox units.
func leakedUnitSet() map[string]bool {
	set := map[string]bool{}
	for _, u := range leakedUnits() {
		set[u] = true
	}
	return set
}

// newLeakedUnits returns sandbox units that are present now but were not in the
// pre-run snapshot.
func newLeakedUnits(preexisting map[string]bool) []string {
	var out []string
	for _, u := range leakedUnits() {
		if !preexisting[u] {
			out = append(out, u)
		}
	}
	return out
}

// leakedUnits lists bwrap-managed systemd services that are still active or
// failed (i.e. not cleanly gone). gVisor uses runsc state, not systemd units.
func leakedUnits() []string {
	out, err := exec.Command("systemctl", "--user", "list-units", "--type=service", "--all", "--no-legend", "--plain", "--no-pager").Output()
	if err != nil {
		return nil
	}
	var units []string
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 || !cgroup.IsSandboxUnit(fields[0]) {
			continue
		}
		switch fields[2] { // ACTIVE column
		case "active", "failed", "activating", "reloading":
			units = append(units, fields[0])
		}
	}
	return units
}
