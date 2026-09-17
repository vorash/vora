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
	"strconv"
	"strings"
	"sync"
	"time"

	v1 "github.com/vorash/vora/api/proto/v1"
	"github.com/vorash/vora/internal/agentproto"
	"github.com/vorash/vora/internal/cgroup"
	"github.com/vorash/vora/internal/config"
	"github.com/vorash/vora/internal/egress"
	"github.com/vorash/vora/internal/network"
	"github.com/vorash/vora/internal/runtime"
)

const agentReadyTimeout = 15 * time.Second

// Backend is the Bubblewrap runtime backend.
type Backend struct {
	cfg      config.Config
	cgroups  cgroup.Manager
	logger   *slog.Logger
	initPath string
	bwrapBin string

	// Restricted-network path (NetworkRestricted). Enabled by SetRestricted.
	subtree    cgroup.Subtree
	networks   *network.Manager
	egressSink func(egress.Event) error

	mu      sync.Mutex
	handles map[string]*handle
}

type handle struct {
	id       string
	spec     runtime.SandboxSpec
	initPath string
	// NetworkNone path: a transient systemd unit + in-sandbox agent.
	unit   *cgroup.Unit
	client *agentproto.Client
	// NetworkRestricted path: a runner-launched sandbox in its own netns.
	group  *cgroup.Group
	proxy  *egress.Proxy
	broker *egress.Broker
	dns    *egress.DNS
	net    *network.Net
	cmd    *exec.Cmd
	done   chan error
	cancel context.CancelFunc
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
	bwrapBin, _ := exec.LookPath("bwrap")
	return &Backend{
		cfg:      cfg,
		cgroups:  cgroups,
		logger:   logger,
		initPath: initPath,
		bwrapBin: bwrapBin,
		handles:  make(map[string]*handle),
	}, nil
}

// SetRestricted enables the NetworkRestricted path: a per-sandbox network
// namespace, an SNI egress proxy, and cgroup limits from the delegated
// subtree. The sandbox is launched by the runner (not via systemd-run) so it
// can join the prepared netns. sink receives each egress decision and may be
// nil.
func (b *Backend) SetRestricted(subtree cgroup.Subtree, networks *network.Manager, sink func(egress.Event) error) {
	b.subtree = subtree
	b.networks = networks
	b.egressSink = sink
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
	modes := []runtime.NetworkMode{runtime.NetworkNone}
	if b.subtree != nil && b.networks != nil && b.bwrapBin != "" {
		modes = append(modes, runtime.NetworkRestricted)
	}
	return runtime.Capabilities{
		NativeExec:             false, // exec is mediated by vora-init
		OwnsCgroups:            false, // systemd-run applies limits
		EnforcesResourceLimits: true,
		NetworkModes:           modes,
	}
}

// Create validates the spec and reserves a handle.
func (b *Backend) Create(_ context.Context, spec runtime.SandboxSpec) (runtime.Handle, error) {
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	if spec.Network.Mode != runtime.NetworkNone && spec.Network.Mode != runtime.NetworkRestricted {
		return nil, fmt.Errorf("%w: network mode %q (bwrap supports %q and %q)",
			runtime.ErrUnsupported, spec.Network.Mode, runtime.NetworkNone, runtime.NetworkRestricted)
	}
	if spec.Network.Mode == runtime.NetworkRestricted && !b.Capabilities().SupportsNetwork(runtime.NetworkRestricted) {
		return nil, fmt.Errorf("%w: NetworkRestricted is not configured on this runner", runtime.ErrUnsupported)
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

// Start launches the sandbox and waits for the in-sandbox agent.
func (b *Backend) Start(ctx context.Context, h runtime.Handle) error {
	hd, err := b.handle(h)
	if err != nil {
		return err
	}
	if hd.spec.Network.Mode == runtime.NetworkRestricted {
		return b.startRestricted(ctx, hd, nil)
	}
	return b.startUnit(ctx, hd)
}

// StartWithCredentials implements runtime.BrokerAware: it starts a
// NetworkRestricted sandbox whose broker serves the resolved credentials.
// Credentials are used only to build the broker and are never stored on the
// handle or the sandbox spec.
func (b *Backend) StartWithCredentials(ctx context.Context, h runtime.Handle, creds []egress.Credential) error {
	hd, err := b.handle(h)
	if err != nil {
		return err
	}
	if len(creds) == 0 {
		return fmt.Errorf("%w: no broker credentials", runtime.ErrInvalidSpec)
	}
	if hd.spec.Network.Mode != runtime.NetworkRestricted {
		return fmt.Errorf("%w: credential brokering requires network mode %q", runtime.ErrInvalidSpec, runtime.NetworkRestricted)
	}
	return b.startRestricted(ctx, hd, creds)
}

// sandboxEnv is the environment the workload sees: the spec env plus the
// broker base URL (a non-secret gateway address) when the sandbox is granted
// broker aliases, so the workload does not have to guess the gateway.
func sandboxEnv(spec runtime.SandboxSpec, brokerAddr string) map[string]string {
	env := make(map[string]string, len(spec.Env)+1)
	for k, v := range spec.Env {
		env[k] = v
	}
	if len(spec.Network.BrokerAliases) > 0 {
		env["VORA_BROKER_URL"] = "http://" + brokerAddr
	}
	return env
}

// writeResolvConf points the sandbox at the gateway resolver and returns the
// host path to bind over /etc/resolv.conf. The image must provide that file as
// the bind target; a missing one fails the start loudly rather than leaving the
// sandbox unable to resolve names.
func writeResolvConf(spec runtime.SandboxSpec, netns *network.Net) (string, error) {
	imageFile := filepath.Join(spec.Workspace.RootfsDir, "etc", "resolv.conf")
	if _, err := os.Stat(imageFile); err != nil {
		return "", fmt.Errorf("bwrap: image %q must provide /etc/resolv.conf for NetworkRestricted: %w", spec.Image, err)
	}
	path := filepath.Join(spec.Workspace.RootDir, "resolv.conf")
	content := fmt.Sprintf("# Managed by Vora: the sandbox gateway is the only resolver (ADR 012).\nnameserver %s\noptions timeout:2 attempts:2\n", netns.GatewayIP)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return "", fmt.Errorf("bwrap: write resolv.conf: %w", err)
	}
	return path, nil
}

// startRestricted launches a NetworkRestricted sandbox from the runner: it
// joins the prepared netns via nsenter, applies cgroup limits from the
// delegated subtree, and starts the sandbox's egress proxy (and, when
// credentials are granted, the broker) before the agent is reachable.
func (b *Backend) startRestricted(ctx context.Context, hd *handle, creds []egress.Credential) error {
	ws := hd.spec.Workspace
	if err := os.MkdirAll(ws.IPCDir, 0o755); err != nil {
		return fmt.Errorf("bwrap: ipc dir: %w", err)
	}
	if ws.ControlSocket != "" {
		_ = os.Remove(ws.ControlSocket)
	}
	if err := installInit(hd.initPath, filepath.Join(ws.IPCDir, "vora-init")); err != nil {
		return err
	}

	netns, err := b.networks.Create(ctx, hd.id)
	if err != nil {
		return fmt.Errorf("bwrap: network: %w", err)
	}
	hd.net = netns

	// The sandbox outlives this RPC, so its proxy and process use a context
	// owned by the handle (cancelled on stop/destroy), not the request ctx.
	sandboxCtx, cancel := context.WithCancel(context.Background())
	hd.cancel = cancel

	proxy := egress.New(hd.id, egress.Policy{AllowedDomains: hd.spec.Network.AllowedDomains}, b.logger, b.egressSink, egress.Limits{
		MaxConnections: b.cfg.EgressMaxConnections,
		IdleTimeout:    b.cfg.EgressIdleTimeout,
	})
	if err := proxy.Listen(sandboxCtx, netns.ProxyAddr()); err != nil {
		b.cleanupRestricted(ctx, hd)
		return err
	}
	hd.proxy = proxy

	// The broker listens on its own gateway port and is reachable only from
	// the sandbox side of the veth. It is started before the workload so a
	// granted alias can never resolve to a dead route.
	if len(creds) > 0 {
		broker := egress.NewBroker(hd.id, egress.Policy{AllowedDomains: hd.spec.Network.AllowedDomains}, creds, b.logger, b.egressSink, egress.Limits{
			MaxConnections: b.cfg.EgressMaxConnections,
			IdleTimeout:    b.cfg.EgressIdleTimeout,
		})
		if err := broker.Listen(sandboxCtx, netns.ServiceAddr(b.cfg.EgressBrokerPort)); err != nil {
			b.cleanupRestricted(ctx, hd)
			return err
		}
		hd.broker = broker
	}

	// The sandbox's only resolver is on the gateway. It is started before the
	// workload so name resolution cannot hit a dead route, and the sandbox is
	// pointed at it through a bound resolv.conf.
	dns := egress.NewDNS(hd.id, egress.Policy{AllowedDomains: hd.spec.Network.AllowedDomains}, b.logger, b.egressSink, egress.Limits{
		MaxConnections: b.cfg.EgressMaxConnections,
	})
	if err := dns.Listen(sandboxCtx, netns.ServiceAddr(network.DefaultDNSPort)); err != nil {
		b.cleanupRestricted(ctx, hd)
		return err
	}
	hd.dns = dns
	resolvConf, err := writeResolvConf(hd.spec, netns)
	if err != nil {
		b.cleanupRestricted(ctx, hd)
		return err
	}

	group, err := b.subtree.Create(ctx, hd.id, hd.spec.Resources)
	if err != nil {
		b.cleanupRestricted(ctx, hd)
		return fmt.Errorf("bwrap: cgroup: %w", err)
	}
	hd.group = group

	argv, err := Args(BuildOptions{
		RootfsDir:    ws.RootfsDir,
		WorkspaceDir: ws.WorkspaceDir,
		HomeDir:      ws.HomeDir,
		IPCDir:       ws.IPCDir,
		Env:          sandboxEnv(hd.spec, netns.ServiceAddr(b.cfg.EgressBrokerPort)),
		ShareNet:     true,
		ResolvConf:   resolvConf,
	})
	if err != nil {
		b.cleanupRestricted(ctx, hd)
		return err
	}
	nsArgs := append([]string{"-t", strconv.Itoa(netns.NetnsPID), "-n", b.bwrapBin}, argv[1:]...)
	cmd := exec.CommandContext(sandboxCtx, "nsenter", nsArgs...)
	closer, err := group.ApplyTo(cmd)
	if err != nil {
		b.cleanupRestricted(ctx, hd)
		return fmt.Errorf("bwrap: cgroup apply: %w", err)
	}
	defer closer()
	if err := cmd.Start(); err != nil {
		b.cleanupRestricted(ctx, hd)
		return fmt.Errorf("bwrap: start: %w", err)
	}
	hd.cmd = cmd
	hd.done = make(chan error, 1)
	go func() { hd.done <- cmd.Wait() }()

	client, err := agentproto.Dial(ws.ControlSocket)
	if err != nil {
		b.cleanupRestricted(ctx, hd)
		return err
	}
	hd.client = client
	if err := waitReadyCmd(ctx, hd.done, client, agentReadyTimeout); err != nil {
		b.cleanupRestricted(ctx, hd)
		return err
	}
	b.logger.Info("sandbox started", "sandbox_id", hd.id, "backend", "bwrap", "network", "restricted", "gateway", netns.GatewayIP)
	return nil
}

// cleanupRestricted tears down every resource of a restricted start.
func (b *Backend) cleanupRestricted(ctx context.Context, hd *handle) {
	if hd.cancel != nil {
		hd.cancel()
		hd.cancel = nil
	}
	if hd.proxy != nil {
		_ = hd.proxy.Close()
		hd.proxy = nil
	}
	if hd.broker != nil {
		_ = hd.broker.Close()
		hd.broker = nil
	}
	if hd.dns != nil {
		_ = hd.dns.Close()
		hd.dns = nil
	}
	if hd.client != nil {
		_ = hd.client.Close()
		hd.client = nil
	}
	if hd.cmd != nil && hd.cmd.Process != nil {
		_ = hd.cmd.Process.Kill()
	}
	if hd.group != nil {
		_ = hd.group.Kill()
		_ = hd.group.Destroy()
		hd.group = nil
	}
	if b.networks != nil && hd.net != nil {
		_ = b.networks.Destroy(ctx, hd.id)
		hd.net = nil
	}
}

// waitReadyCmd is waitReady for a runner-launched process: it also fails fast
// when the sandbox exits before the agent is ready.
func waitReadyCmd(ctx context.Context, done <-chan error, client *agentproto.Client, timeout time.Duration) error {
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
		select {
		case werr := <-done:
			return fmt.Errorf("bwrap: sandbox exited before the agent was ready: %w", werr)
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	return fmt.Errorf("bwrap: agent not ready after %s: %w", timeout, lastErr)
}

// startUnit launches the sandbox as a transient systemd unit (NetworkNone).
func (b *Backend) startUnit(ctx context.Context, hd *handle) error {
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

// Stats reads resource usage from the sandbox cgroup. After an OOM the cgroup
// files may already be gone; systemd's Result=oom-kill is retained on the
// failed unit and is mapped onto OOMKills.
func (b *Backend) Stats(ctx context.Context, h runtime.Handle) (*runtime.Stats, error) {
	hd, err := b.handle(h)
	if err != nil {
		return nil, err
	}
	if hd.group != nil {
		return hd.group.Stats()
	}
	if hd.unit == nil {
		return &runtime.Stats{}, nil
	}
	st, err := hd.unit.Stats(ctx)
	if err != nil {
		st = &runtime.Stats{}
	}
	if st.OOMKills == 0 {
		if result, rerr := hd.unit.Result(ctx); rerr == nil && result == "oom-kill" {
			st.OOMKills = 1
		}
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
	if hd.group != nil || hd.net != nil || hd.proxy != nil {
		b.cleanupRestricted(ctx, hd)
		return nil
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
	if hd.group != nil || hd.net != nil || hd.proxy != nil {
		b.cleanupRestricted(ctx, hd)
		b.mu.Lock()
		delete(b.handles, hd.id)
		b.mu.Unlock()
		return nil
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

// List implements runtime.Sweeper.
func (b *Backend) List(ctx context.Context) ([]string, error) {
	seen := map[string]bool{}
	var ids []string
	add := func(id string) {
		if id != "" && !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	if b.cgroups != nil {
		units, err := b.cgroups.List(ctx)
		if err != nil {
			return nil, err
		}
		for _, name := range units {
			if cgroup.IsSandboxUnit(name) {
				add(strings.TrimPrefix(name, cgroup.UnitPrefix))
			}
		}
	}
	if b.networks != nil {
		for _, id := range b.networks.List() {
			add(id)
		}
	}
	return ids, nil
}

// Reap implements runtime.Sweeper.
func (b *Backend) Reap(ctx context.Context, id string) error {
	b.mu.Lock()
	hd, ok := b.handles[id]
	delete(b.handles, id)
	b.mu.Unlock()
	if ok {
		if hd.group != nil || hd.net != nil || hd.proxy != nil {
			b.cleanupRestricted(ctx, hd)
		} else {
			if hd.client != nil {
				_ = hd.client.Close()
			}
			if hd.unit != nil {
				_ = hd.unit.Stop(ctx)
				_ = hd.unit.Remove(ctx)
			}
		}
	}
	if b.networks != nil {
		_ = b.networks.Destroy(ctx, id)
	}
	if b.cgroups == nil {
		return nil
	}
	u, err := b.cgroups.Attach(ctx, cgroup.UnitName(id))
	if err != nil {
		return nil
	}
	_ = u.Stop(ctx)
	_ = u.Remove(ctx)
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
