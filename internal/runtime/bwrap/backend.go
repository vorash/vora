// Package bwrap implements the runtime.Backend for Bubblewrap.
//
// A sandbox is launched as a transient systemd unit (for cgroup limits) whose
// command is bubblewrap, and vora-init runs as PID 1 inside. The runner talks
// to the agent over a per-sandbox Unix socket for exec, stats and shutdown.
package bwrap

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	v1 "github.com/vorash/vora/api/proto/v1"
	"github.com/vorash/vora/internal/agentproto"
	"github.com/vorash/vora/internal/cgroup"
	"github.com/vorash/vora/internal/config"
	"github.com/vorash/vora/internal/runtime"
)

const agentReadyTimeout = 15 * time.Second

// Backend is the Bubblewrap runtime backend.
type Backend struct {
	cfg      config.Config
	cgroups  cgroup.Manager
	logger   *slog.Logger
	initPath string

	mu      sync.Mutex
	handles map[string]*handle
}

type handle struct {
	id       string
	spec     runtime.SandboxSpec
	initPath string
	unit     *cgroup.Unit
	client   *agentproto.Client
}

func (h *handle) ID() string { return h.id }

// New returns a Bubblewrap backend.
func New(cfg config.Config, cgroups cgroup.Manager, logger *slog.Logger) (*Backend, error) {
	if logger == nil {
		logger = slog.Default()
	}
	initPath, err := resolveInit()
	if err != nil {
		return nil, err
	}
	return &Backend{
		cfg:      cfg,
		cgroups:  cgroups,
		logger:   logger,
		initPath: initPath,
		handles:  make(map[string]*handle),
	}, nil
}

// resolveInit finds the vora-init binary: VORA_INIT_BINARY, then next to the
// current executable, then PATH.
func resolveInit() (string, error) {
	if p := os.Getenv("VORA_INIT_BINARY"); p != "" {
		return p, nil
	}
	if exe, err := os.Executable(); err == nil {
		cand := filepath.Join(filepath.Dir(exe), "vora-init")
		if info, err := os.Stat(cand); err == nil && !info.IsDir() {
			return cand, nil
		}
	}
	if p, err := exec.LookPath("vora-init"); err == nil {
		return p, nil
	}
	return "", fmt.Errorf("bwrap: vora-init binary not found (set VORA_INIT_BINARY or build ./cmd/vora-init)")
}

// Name implements runtime.Backend.
func (b *Backend) Name() string { return "bwrap" }

// Capabilities implements runtime.Backend.
func (b *Backend) Capabilities() runtime.Capabilities {
	return runtime.Capabilities{
		NativeExec:   false, // exec is mediated by vora-init
		OwnsCgroups:  false, // the runner applies cgroup limits
		NetworkModes: []runtime.NetworkMode{runtime.NetworkNone},
	}
}

// Create validates the spec and reserves a handle.
func (b *Backend) Create(_ context.Context, spec runtime.SandboxSpec) (runtime.Handle, error) {
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	if spec.Network.Mode != runtime.NetworkNone {
		return nil, fmt.Errorf("%w: network mode %q (bwrap supports %q)",
			runtime.ErrUnsupported, spec.Network.Mode, runtime.NetworkNone)
	}
	rootfs := spec.Workspace.RootfsDir
	if rootfs == "" {
		return nil, fmt.Errorf("%w: workspace rootfs is not set", runtime.ErrInvalidSpec)
	}
	if info, err := os.Stat(rootfs); err != nil || !info.IsDir() {
		return nil, fmt.Errorf("%w: rootfs %s not available (run 'make images')", runtime.ErrInvalidSpec, rootfs)
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	if _, exists := b.handles[spec.ID]; exists {
		return nil, fmt.Errorf("bwrap: sandbox %s already exists", spec.ID)
	}
	h := &handle{id: spec.ID, spec: spec, initPath: b.initPath}
	b.handles[spec.ID] = h
	return h, nil
}

// Start launches the sandbox under a cgroup and waits for the agent.
func (b *Backend) Start(ctx context.Context, h runtime.Handle) error {
	hd, err := b.handle(h)
	if err != nil {
		return err
	}
	ws := hd.spec.Workspace
	if err := os.MkdirAll(ws.IPCDir, 0o755); err != nil {
		return fmt.Errorf("bwrap: ipc dir: %w", err)
	}
	// Remove a stale socket from a previous run.
	if ws.ControlSocket != "" {
		_ = os.Remove(ws.ControlSocket)
	}

	argv, err := Args(BuildOptions{
		RootfsDir:    ws.RootfsDir,
		WorkspaceDir: ws.WorkspaceDir,
		HomeDir:      ws.HomeDir,
		IPCDir:       ws.IPCDir,
		Env:          hd.spec.Env,
	})
	if err != nil {
		return err
	}
	if err := installInit(hd.initPath, filepath.Join(ws.IPCDir, "vora-init")); err != nil {
		return err
	}

	unit, err := b.cgroups.Start(ctx, cgroup.StartRequest{
		Name:   cgroup.UnitName(hd.id),
		Limits: hd.spec.Resources,
		Argv:   argv,
	})
	if err != nil {
		return fmt.Errorf("bwrap: start unit: %w", err)
	}
	hd.unit = unit

	client, err := agentproto.Dial(ws.ControlSocket)
	if err != nil {
		_ = unit.Stop(ctx)
		return err
	}
	hd.client = client

	if err := waitReady(ctx, unit, client, agentReadyTimeout); err != nil {
		_ = client.Close()
		_ = unit.Stop(ctx)
		_ = unit.Remove(ctx)
		hd.unit, hd.client = nil, nil
		return err
	}
	b.logger.Info("sandbox started", "sandbox_id", hd.id, "unit", unit.Name(), "cgroup", unit.CgroupPath())
	return nil
}

// waitReady polls the agent until it responds, the unit dies, or we time out.
func waitReady(ctx context.Context, unit *cgroup.Unit, client *agentproto.Client, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		pingCtx, cancel := context.WithTimeout(ctx, time.Second)
		_, err := client.Ping(pingCtx)
		cancel()
		if err == nil {
			return nil
		}
		lastErr = err
		if active, aerr := unit.Active(ctx); aerr == nil && !active {
			result, _ := unit.Result(ctx)
			logs := "see journalctl --user -u " + unit.Name()
			return fmt.Errorf("bwrap: sandbox exited before the agent was ready (result=%q; %s)", result, logs)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	return fmt.Errorf("bwrap: agent not ready after %s: %w", timeout, lastErr)
}

// Exec runs a command through the in-sandbox agent.
func (b *Backend) Exec(ctx context.Context, h runtime.Handle, req runtime.ExecRequest) (runtime.ExecStream, error) {
	hd, err := b.handle(h)
	if err != nil {
		return nil, err
	}
	if hd.client == nil {
		return nil, fmt.Errorf("bwrap: sandbox %s is not running", hd.id)
	}
	return hd.client.Exec(ctx, &v1.ExecRequest{
		ExecutionId:    req.ExecutionID,
		Command:        req.Command,
		Args:           req.Args,
		Cwd:            req.Cwd,
		Env:            envSlice(req.Env),
		TimeoutSeconds: int64(req.Timeout / time.Second),
		Stdin:          req.Stdin,
	})
}

// Signal forwards a signal to the sandbox process group.
func (b *Backend) Signal(ctx context.Context, h runtime.Handle, sig int32) error {
	hd, err := b.handle(h)
	if err != nil {
		return err
	}
	if hd.client == nil {
		return fmt.Errorf("bwrap: sandbox %s is not running", hd.id)
	}
	return hd.client.Signal(ctx, sig)
}

// Stats reads resource usage from the sandbox cgroup.
func (b *Backend) Stats(ctx context.Context, h runtime.Handle) (*runtime.Stats, error) {
	hd, err := b.handle(h)
	if err != nil {
		return nil, err
	}
	if hd.unit == nil {
		return &runtime.Stats{}, nil
	}
	st, err := hd.unit.Stats(ctx)
	if err != nil {
		return &runtime.Stats{}, nil
	}
	return st, nil
}

// Stop asks the agent to exit, then stops the cgroup unit.
func (b *Backend) Stop(ctx context.Context, h runtime.Handle) error {
	hd, err := b.handle(h)
	if err != nil {
		return err
	}
	if hd.client != nil {
		shutdownCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		_ = hd.client.Shutdown(shutdownCtx)
		cancel()
	}
	if hd.unit != nil {
		if err := hd.unit.Stop(ctx); err != nil {
			return err
		}
	}
	return nil
}

// Destroy stops the sandbox and releases all backend resources.
func (b *Backend) Destroy(ctx context.Context, h runtime.Handle) error {
	hd, err := b.handle(h)
	if err != nil {
		return err
	}
	if err := b.Stop(ctx, hd); err != nil {
		b.logger.Warn("stop during destroy failed", "sandbox_id", hd.id, "error", err)
	}
	if hd.client != nil {
		_ = hd.client.Close()
	}
	if hd.unit != nil {
		_ = hd.unit.Remove(ctx)
	}
	b.mu.Lock()
	delete(b.handles, hd.id)
	b.mu.Unlock()
	return nil
}

func (b *Backend) handle(h runtime.Handle) (*handle, error) {
	if h == nil {
		return nil, fmt.Errorf("bwrap: nil handle")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	hd, ok := b.handles[h.ID()]
	if !ok {
		return nil, fmt.Errorf("%w: %s", runtime.ErrNotFound, h.ID())
	}
	return hd, nil
}

func envSlice(env map[string]string) []string {
	if len(env) == 0 {
		return nil
	}
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	return out
}

// installInit copies the agent binary into the sandbox ipc directory, which is
// bound into the sandbox at /vora. The copy is atomic so a stale binary is
// never executed.
func installInit(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return fmt.Errorf("bwrap: read init binary: %w", err)
	}
	tmp := dst + ".tmp"
	if err := os.WriteFile(tmp, data, 0o755); err != nil {
		return fmt.Errorf("bwrap: install init binary: %w", err)
	}
	if err := os.Rename(tmp, dst); err != nil {
		return fmt.Errorf("bwrap: install init binary: %w", err)
	}
	return nil
}
