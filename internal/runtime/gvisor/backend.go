// Package gvisor implements the runtime.Backend for gVisor (`runsc`).
//
// The backend drives the runsc CLI directly (no containerd shim). Persistent
// rootless sandboxes use gVisor "Method 2": the runner must run inside a
// persistent rootlesskit user namespace and runsc is invoked as UID 0 without
// --rootless. See docs/adr/008-gvisor-runsc.md.
package gvisor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/vorash/vora/internal/cgroup"
	"github.com/vorash/vora/internal/config"
	"github.com/vorash/vora/internal/runtime"
)

// Backend is the gVisor runtime backend.
type Backend struct {
	cfg      config.Config
	logger   *slog.Logger
	runsc    string
	root     string
	platform string
	initPath string
	subtree  cgroup.Subtree

	mu      sync.Mutex
	handles map[string]*handle
}

type handle struct {
	id        string
	spec      runtime.SandboxSpec
	bundleDir string
	group     *cgroup.Group
}

func (h *handle) ID() string { return h.id }

// New returns a gVisor backend. subtree may be nil; Start then fails rather
// than running an unlimited sandbox.
func New(cfg config.Config, logger *slog.Logger, subtree cgroup.Subtree) (*Backend, error) {
	if logger == nil {
		logger = slog.Default()
	}
	runsc, err := resolveRunsc(cfg)
	if err != nil {
		return nil, err
	}
	initPath, err := runtime.ResolveInitBinary()
	if err != nil {
		return nil, fmt.Errorf("gvisor: %w", err)
	}
	root := cfg.GvisorRunRoot
	if root == "" {
		if rd := os.Getenv("XDG_RUNTIME_DIR"); rd != "" {
			root = filepath.Join(rd, "vora-gvisor")
		} else {
			root = "/run/runsc"
		}
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("gvisor: state root: %w", err)
	}
	platform := cfg.GvisorPlatform
	if platform == "" {
		platform = "kvm"
	}
	b := &Backend{
		cfg:      cfg,
		logger:   logger,
		runsc:    runsc,
		root:     root,
		platform: platform,
		initPath: initPath,
		subtree:  subtree,
		handles:  make(map[string]*handle),
	}
	logger.Info("gvisor backend ready", "runsc", runsc, "platform", platform, "root", root)
	return b, nil
}

// resolveRunsc finds the runsc binary.
func resolveRunsc(cfg config.Config) (string, error) {
	var candidates []string
	if cfg.RunscPath != "" {
		candidates = append(candidates, cfg.RunscPath)
	}
	if p, err := exec.LookPath("runsc"); err == nil {
		candidates = append(candidates, p)
	}
	if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates, filepath.Join(home, ".local", "bin", "runsc"))
	}
	candidates = append(candidates, "/usr/local/bin/runsc", "/usr/bin/runsc")
	for _, c := range candidates {
		if info, err := os.Stat(c); err == nil && !info.IsDir() {
			return c, nil
		}
	}
	return "", fmt.Errorf("gvisor: runsc not found (set VORA_RUNSC or install runsc)")
}

// Name implements runtime.Backend.
func (b *Backend) Name() string { return "gvisor" }

// Capabilities implements runtime.Backend.
func (b *Backend) Capabilities() runtime.Capabilities {
	return runtime.Capabilities{
		NativeExec:             true, // runsc exec, no in-sandbox agent
		OwnsCgroups:            true, // delegated subtree under the runner
		EnforcesResourceLimits: b.subtree != nil,
		NetworkModes:           []runtime.NetworkMode{runtime.NetworkNone},
	}
}

// globalFlags are the runsc flags common to every subcommand.
//
// --ignore-cgroups is required for rootless mode (the systemd cgroup manager
// is denied by polkit); it is a flag, not a config-file setting.
func (b *Backend) globalFlags() []string {
	return []string{"--root", b.root, "--platform", b.platform, "--network", "none", "--ignore-cgroups"}
}

// run executes a runsc subcommand. Output is redirected to a temp file rather
// than a pipe: `runsc create`/`start` spawn a long-lived sandbox process that
// inherits the file descriptors, so an os.Pipe would never close and Wait would
// hang. The file is read back after the direct child exits.
func (b *Backend) run(ctx context.Context, args ...string) (string, error) {
	return b.runIn(ctx, nil, args...)
}

func (b *Backend) runIn(ctx context.Context, g *cgroup.Group, args ...string) (string, error) {
	f, err := os.CreateTemp("", "vora-runsc-out-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(f.Name())
	defer f.Close()

	cmd := exec.CommandContext(ctx, b.runsc, append(b.globalFlags(), args...)...)
	cmd.Stdout = f
	cmd.Stderr = f
	if g != nil {
		closer, err := g.ApplyTo(cmd)
		if err != nil {
			return "", err
		}
		defer closer()
	}
	runErr := cmd.Run()

	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return "", runErr
	}
	data, _ := io.ReadAll(f)
	return strings.TrimSpace(string(data)), runErr
}

// Create validates the spec and reserves a handle.
func (b *Backend) Create(_ context.Context, spec runtime.SandboxSpec) (runtime.Handle, error) {
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	if spec.Network.Mode != runtime.NetworkNone {
		return nil, fmt.Errorf("%w: network mode %q (gvisor supports %q)",
			runtime.ErrUnsupported, spec.Network.Mode, runtime.NetworkNone)
	}
	rootfs := spec.Workspace.RootfsDir
	if rootfs == "" {
		return nil, fmt.Errorf("%w: workspace rootfs is not set", runtime.ErrInvalidSpec)
	}
	if info, err := os.Stat(rootfs); err != nil || !info.IsDir() {
		return nil, fmt.Errorf("%w: rootfs %s not available", runtime.ErrInvalidSpec, rootfs)
	}
	if spec.Workspace.RootDir == "" {
		return nil, fmt.Errorf("%w: workspace root dir is not set", runtime.ErrInvalidSpec)
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	if _, exists := b.handles[spec.ID]; exists {
		return nil, fmt.Errorf("gvisor: sandbox %s already exists", spec.ID)
	}
	h := &handle{
		id:        spec.ID,
		spec:      spec,
		bundleDir: filepath.Join(spec.Workspace.RootDir, "bundle"),
	}
	b.handles[spec.ID] = h
	return h, nil
}

// Start writes the OCI bundle, creates the sandbox and starts it.
func (b *Backend) Start(ctx context.Context, h runtime.Handle) error {
	hd, err := b.handle(h)
	if err != nil {
		return err
	}
	if b.subtree == nil {
		return fmt.Errorf("gvisor: resource limits unavailable (cgroup subtree not writable)")
	}
	group, err := b.subtree.Create(ctx, hd.id, hd.spec.Resources)
	if err != nil {
		return fmt.Errorf("gvisor: cgroup: %w", err)
	}
	hd.group = group
	// The init is Vora's static binary, copied into the ipc directory (bound at
	// /vora) so the sandbox never depends on the image having a shell.
	ws := hd.spec.Workspace
	if err := os.MkdirAll(ws.IPCDir, 0o755); err != nil {
		_ = group.Destroy()
		hd.group = nil
		return fmt.Errorf("gvisor: ipc dir: %w", err)
	}
	if err := runtime.InstallInitBinary(b.initPath, filepath.Join(ws.IPCDir, "vora-init")); err != nil {
		_ = group.Destroy()
		hd.group = nil
		return fmt.Errorf("gvisor: %w", err)
	}
	if err := writeBundle(b.runsc, hd.bundleDir, hd.spec); err != nil {
		_ = group.Destroy()
		hd.group = nil
		return err
	}
	if out, err := b.runIn(ctx, group, "create", "--bundle", hd.bundleDir, hd.id); err != nil {
		_ = group.Destroy()
		hd.group = nil
		return fmt.Errorf("gvisor: create: %w: %s", err, out)
	}
	if out, err := b.runIn(ctx, group, "start", hd.id); err != nil {
		_, _ = b.run(ctx, "delete", "--force", hd.id)
		_ = group.Destroy()
		hd.group = nil
		return fmt.Errorf("gvisor: start: %w: %s", err, out)
	}
	b.logger.Info("sandbox started", "sandbox_id", hd.id, "backend", "gvisor", "platform", b.platform)
	return nil
}

// Exec runs a command in a running sandbox via `runsc exec`.
func (b *Backend) Exec(ctx context.Context, h runtime.Handle, req runtime.ExecRequest) (runtime.ExecStream, error) {
	hd, err := b.handle(h)
	if err != nil {
		return nil, err
	}
	if req.Command == "" {
		return nil, fmt.Errorf("gvisor: empty command")
	}

	execCtx, cancel := context.WithCancel(ctx)
	args := append(b.globalFlags(), "exec", hd.id, req.Command)
	args = append(args, req.Args...)
	cmd := exec.CommandContext(execCtx, b.runsc, args...)
	if len(req.Stdin) > 0 {
		cmd.Stdin = bytes.NewReader(req.Stdin)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		cancel()
		return nil, err
	}

	es := &execStream{ch: make(chan runtime.ExecChunk, 16), done: make(chan struct{}), cancel: cancel, cmd: cmd}
	go es.stream(stdout, stderr)
	return es, nil
}

// Signal sends a signal to a sandbox.
func (b *Backend) Signal(ctx context.Context, h runtime.Handle, sig int32) error {
	hd, err := b.handle(h)
	if err != nil {
		return err
	}
	out, err := b.run(ctx, "kill", hd.id, fmt.Sprintf("%d", sig))
	if err != nil && isMissing(out) {
		return nil
	}
	return err
}

// Stats returns resource usage from the sandbox cgroup.
func (b *Backend) Stats(ctx context.Context, h runtime.Handle) (*runtime.Stats, error) {
	hd, err := b.handle(h)
	if err != nil {
		return nil, err
	}
	if hd.group != nil {
		st, err := hd.group.Stats()
		if err != nil {
			return &runtime.Stats{}, nil
		}
		return st, nil
	}
	return &runtime.Stats{}, nil
}

// Stop kills the sandbox.
func (b *Backend) Stop(ctx context.Context, h runtime.Handle) error {
	hd, err := b.handle(h)
	if err != nil {
		return err
	}
	out, err := b.run(ctx, "kill", hd.id, "KILL")
	if err != nil && isMissing(out) {
		return nil
	}
	return err
}

// Destroy kills and deletes the sandbox and removes its bundle.
func (b *Backend) Destroy(ctx context.Context, h runtime.Handle) error {
	hd, err := b.handle(h)
	if err != nil {
		return err
	}
	_, _ = b.run(ctx, "kill", hd.id, "KILL")
	if out, err := b.run(ctx, "delete", "--force", hd.id); err != nil && !isMissing(out) {
		b.logger.Warn("gvisor delete failed", "sandbox_id", hd.id, "error", err, "output", out)
	}
	if hd.group != nil {
		_ = hd.group.Destroy()
		hd.group = nil
	}
	_ = os.RemoveAll(hd.bundleDir)

	b.mu.Lock()
	delete(b.handles, hd.id)
	b.mu.Unlock()
	return nil
}

// List implements runtime.Sweeper.
func (b *Backend) List(ctx context.Context) ([]string, error) {
	out, err := b.run(ctx, "list", "--quiet")
	if err != nil {
		if isMissing(out) {
			return nil, nil
		}
		return nil, err
	}
	var ids []string
	for _, line := range strings.Split(out, "\n") {
		if id := strings.TrimSpace(line); id != "" {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

// Reap implements runtime.Sweeper.
func (b *Backend) Reap(ctx context.Context, id string) error {
	_, _ = b.run(ctx, "kill", id, "KILL")
	_, err := b.run(ctx, "delete", "--force", id)
	if b.subtree != nil {
		if g, aerr := b.subtree.Attach(ctx, id); aerr == nil {
			_ = g.Destroy()
		}
	}
	return err
}

func (b *Backend) handle(h runtime.Handle) (*handle, error) {
	if h == nil {
		return nil, fmt.Errorf("gvisor: nil handle")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	hd, ok := b.handles[h.ID()]
	if !ok {
		return nil, fmt.Errorf("%w: %s", runtime.ErrNotFound, h.ID())
	}
	return hd, nil
}

// isMissing reports whether runsc output indicates the sandbox does not exist.
func isMissing(out string) bool {
	o := strings.ToLower(out)
	return strings.Contains(o, "not found") || strings.Contains(o, "does not exist") || strings.Contains(o, "no such")
}

// execStream adapts a runsc exec process to runtime.ExecStream.
type execStream struct {
	ch     chan runtime.ExecChunk
	done   chan struct{}
	cancel context.CancelFunc
	cmd    *exec.Cmd
	once   sync.Once
}

func (e *execStream) Recv() (runtime.ExecChunk, error) {
	chunk, ok := <-e.ch
	if !ok {
		return runtime.ExecChunk{}, io.EOF
	}
	return chunk, nil
}

func (e *execStream) Close() error {
	e.once.Do(func() {
		close(e.done)
		e.cancel()
	})
	return nil
}

func (e *execStream) send(chunk runtime.ExecChunk) {
	select {
	case e.ch <- chunk:
	case <-e.done:
	}
}

func (e *execStream) stream(stdout, stderr io.Reader) {
	var wg sync.WaitGroup
	wg.Add(2)
	go pipeExec(stdout, func(b []byte) { e.send(runtime.ExecChunk{Stdout: b}) }, &wg)
	go pipeExec(stderr, func(b []byte) { e.send(runtime.ExecChunk{Stderr: b}) }, &wg)
	wg.Wait()

	code := 0
	if err := e.cmd.Wait(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		} else {
			code = -1
		}
	}
	select {
	case e.ch <- runtime.ExecChunk{Exit: &runtime.ExecResult{ExitCode: code, Reason: runtime.ReasonExited}}:
	case <-e.done:
	}
	close(e.ch)
}

func pipeExec(r io.Reader, send func([]byte), wg *sync.WaitGroup) {
	defer wg.Done()
	buf := make([]byte, 32*1024)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			chunk := make([]byte, n)
			copy(chunk, buf[:n])
			send(chunk)
		}
		if err != nil {
			return
		}
	}
}
