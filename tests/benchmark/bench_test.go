//go:build benchmark

// Package benchmark measures sandbox create/exec latency, per-sandbox memory
// overhead and concurrency behaviour. Results are captured by scripts/bench.sh
// into docs/benchmarks/.
package benchmark

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/vorash/vora/internal/cgroup"
	"github.com/vorash/vora/internal/config"
	"github.com/vorash/vora/internal/runtime"
	"github.com/vorash/vora/internal/runtime/bwrap"
	"github.com/vorash/vora/internal/workspace"
)

var benchCounter int

type env struct {
	t       testing.TB
	backend *bwrap.Backend
	ws      *workspace.Manager
	cfg     config.Config
}

func newEnv(t testing.TB) *env {
	t.Helper()
	if os.Getenv("VORA_INIT_BINARY") == "" {
		t.Setenv("VORA_INIT_BINARY", buildInit(t))
	}
	cgroups, err := cgroup.New(config.Config{SystemdMode: config.SystemdUser})
	if err != nil {
		t.Skipf("systemd user manager unavailable: %v", err)
	}
	cfg := config.Config{StateDir: t.TempDir(), SystemdMode: config.SystemdUser, DefaultTTL: time.Minute}
	backend, err := bwrap.New(cfg, cgroups, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("bwrap backend: %v", err)
	}
	return &env{t: t, backend: backend, ws: workspace.NewManager(cfg.SandboxesDir()), cfg: cfg}
}

func (e *env) create(id string, limits runtime.ResourceLimits) runtime.Handle {
	e.t.Helper()
	ws := e.ws.Paths(id)
	ws.RootfsDir = imageRootfs(e.t, "base")
	if _, err := e.ws.Create(ws); err != nil {
		e.t.Fatalf("workspace: %v", err)
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
	h, err := e.backend.Create(ctx, spec)
	if err != nil {
		e.t.Fatalf("Create: %v", err)
	}
	if err := e.backend.Start(ctx, h); err != nil {
		e.t.Fatalf("Start: %v", err)
	}
	return h
}

func (e *env) exec(h runtime.Handle, command string, args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	stream, err := e.backend.Exec(ctx, h, runtime.ExecRequest{Command: command, Args: args, Cwd: "/workspace"})
	if err != nil {
		return err
	}
	defer stream.Close()
	for {
		_, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func limits() runtime.ResourceLimits {
	return runtime.ResourceLimits{CPUMillis: 1000, MemoryBytes: 256 << 20, Pids: 64}
}

// BenchmarkSandboxLifecycle measures end-to-end create + start + destroy.
func BenchmarkSandboxLifecycle(b *testing.B) {
	e := newEnv(b)
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		id := fmt.Sprintf("sbx_bench_%d", next())
		h := e.create(id, limits())
		_ = e.backend.Destroy(ctx, h)
	}
}

// BenchmarkExec measures exec round-trip latency on a running sandbox.
func BenchmarkExec(b *testing.B) {
	e := newEnv(b)
	h := e.create(fmt.Sprintf("sbx_bench_%d", next()), limits())
	defer e.backend.Destroy(context.Background(), h)

	// Warm up the agent.
	_ = e.exec(h, "/bin/true")

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := e.exec(h, "/bin/true"); err != nil {
			b.Fatalf("exec: %v", err)
		}
	}
}

// TestConcurrencyLadder creates sandboxes at increasing target counts and
// reports latency, memory overhead and failure rate. Targets are controlled by
// VORA_BENCH_TARGETS (default "10,50") and should only be raised after the host
// is confirmed stable.
func TestConcurrencyLadder(t *testing.T) {
	e := newEnv(t)
	targets := parseTargets(os.Getenv("VORA_BENCH_TARGETS"))
	ctx := context.Background()

	fmt.Printf("\n## Concurrency ladder\n\n")
	fmt.Printf("| target | created | create p50 | create p95 | create p99 | mem/sandbox | failures | cleanup total |\n")
	fmt.Printf("|--------|---------|-----------|-----------|-----------|-------------|----------|---------------|\n")

	for _, target := range targets {
		var handles []runtime.Handle
		var latencies []time.Duration
		failures := 0

		for i := 0; i < target; i++ {
			id := fmt.Sprintf("sbx_bench_%d", next())
			start := time.Now()
			h, err := e.tryCreate(id, limits())
			if err != nil {
				failures++
				continue
			}
			latencies = append(latencies, time.Since(start))
			handles = append(handles, h)
		}

		// Sample memory overhead from one sandbox's cgroup.
		var memPer int64
		if len(handles) > 0 {
			if st, err := e.backend.Stats(ctx, handles[0]); err == nil {
				memPer = st.MemoryBytes
			}
		}

		cleanupStart := time.Now()
		for _, h := range handles {
			_ = e.backend.Destroy(ctx, h)
		}
		cleanup := time.Since(cleanupStart)

		p50, p95, p99 := percentiles(latencies)
		fmt.Printf("| %d | %d | %s | %s | %s | %s | %d | %s |\n",
			target, len(handles), p50.Round(time.Millisecond), p95.Round(time.Millisecond),
			p99.Round(time.Millisecond), humanBytes(memPer), failures, cleanup.Round(time.Millisecond))
	}
}

func (e *env) tryCreate(id string, lim runtime.ResourceLimits) (h runtime.Handle, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
		}
	}()
	ws := e.ws.Paths(id)
	ws.RootfsDir = imageRootfs(e.t, "base")
	if _, err := e.ws.Create(ws); err != nil {
		return nil, err
	}
	spec := runtime.SandboxSpec{
		ID: id, Image: "base", Resources: lim,
		Network: runtime.NetworkPolicy{Mode: runtime.NetworkNone}, TTL: time.Minute, Workspace: ws,
	}
	ctx := context.Background()
	h, err = e.backend.Create(ctx, spec)
	if err != nil {
		return nil, err
	}
	if err := e.backend.Start(ctx, h); err != nil {
		return nil, err
	}
	return h, nil
}

func next() int { benchCounter++; return benchCounter }

func percentiles(d []time.Duration) (p50, p95, p99 time.Duration) {
	if len(d) == 0 {
		return 0, 0, 0
	}
	sort.Slice(d, func(i, j int) bool { return d[i] < d[j] })
	at := func(p float64) time.Duration {
		idx := int(p * float64(len(d)-1))
		return d[idx]
	}
	return at(0.50), at(0.95), at(0.99)
}

func humanBytes(n int64) string {
	switch {
	case n <= 0:
		return "-"
	case n < 1<<20:
		return fmt.Sprintf("%d KiB", n>>10)
	default:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	}
}

func parseTargets(s string) []int {
	if strings.TrimSpace(s) == "" {
		s = "10,50"
	}
	var out []int
	for _, part := range strings.Split(s, ",") {
		if n, err := strconv.Atoi(strings.TrimSpace(part)); err == nil && n > 0 {
			out = append(out, n)
		}
	}
	return out
}

func imageRootfs(t testing.TB, name string) string {
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

func buildInit(t testing.TB) string {
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
