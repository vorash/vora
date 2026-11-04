// Command vora-runner is the sandbox runner daemon.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	goruntime "runtime"
	"sort"
	"syscall"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"golang.org/x/sys/unix"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	v1 "github.com/vorash/vora/api/proto/v1"
	"github.com/vorash/vora/internal/audit/postgres"
	"github.com/vorash/vora/internal/cgroup"
	"github.com/vorash/vora/internal/config"
	vctr "github.com/vorash/vora/internal/containerd"
	"github.com/vorash/vora/internal/egress"
	"github.com/vorash/vora/internal/network"
	runregistry "github.com/vorash/vora/internal/registry"
	"github.com/vorash/vora/internal/runner"
	"github.com/vorash/vora/internal/runtime"
	"github.com/vorash/vora/internal/runtime/bwrap"
	"github.com/vorash/vora/internal/runtime/gvisor"
	"github.com/vorash/vora/internal/sandbox"
	"github.com/vorash/vora/internal/telemetry"
	"github.com/vorash/vora/internal/tlsutil"
	"github.com/vorash/vora/internal/tunnel"
	"github.com/vorash/vora/internal/ulid"
	"github.com/vorash/vora/internal/version"
)

// auditPruneInterval is how often expired audit tombstones are removed.
const auditPruneInterval = 10 * time.Minute

// openAudit connects to the Postgres audit store and applies its schema.
func openAudit(ctx context.Context, dsn string) (*postgres.Store, error) {
	store, err := postgres.Open(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("audit: postgres unavailable: %w", err)
	}
	if err := store.Migrate(ctx); err != nil {
		store.Close()
		return nil, fmt.Errorf("audit: migration failed: %w", err)
	}
	return store, nil
}

// requireImageStore refuses to start in containerd images mode while the
// containerd socket is missing. The image resolver picks its store once, at
// startup; a system runner that came up before the vora user's rootless
// containerd (the unit cannot be ordered after a user manager's services)
// would otherwise refuse every image until restarted. Failing lets systemd's
// Restart= retry until the socket exists.
func requireImageStore(mode, address string, available func(string) bool) error {
	if mode != config.ImagesContainerd || available(address) {
		return nil
	}
	return fmt.Errorf("images: VORA_IMAGES_MODE=containerd but no containerd socket at %s", vctr.SocketPath(address))
}

// defaultRuntimeDir returns the runtime directory a non-root runner should use
// when XDG_RUNTIME_DIR is unset, or "" to leave it alone. The system unit runs
// the runner as the vora user but cannot name that user's runtime directory
// (%U is the service manager's uid, 0). systemd-run --user, the user bus and
// the rootless containerd socket all live there.
func defaultRuntimeDir(current string, euid int) string {
	if current != "" || euid == 0 {
		return ""
	}
	return fmt.Sprintf("/run/user/%d", euid)
}

// ensurePrivateDir creates dir, or tightens an existing one, to 0700: the
// runner's state holds workloads' files and control sockets.
func ensurePrivateDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return os.Chmod(dir, 0o700)
}

// runnerIDFromCerts returns the runner ID carried as a DNS SAN by the registry
// or tunnel client certificate, or "" when neither carries one. vora-api binds
// a runner's declared ID to that SAN (V-08), so a runner with a client CA in
// front of it must use the ID its certificate was issued for rather than a new
// one each start. More than one ID, or certificates that disagree, is an error.
func runnerIDFromCerts(paths ...string) (string, error) {
	id := ""
	for _, path := range paths {
		cert, err := readLeafCertificate(path)
		if err != nil {
			return "", err
		}
		var found []string
		for _, name := range cert.DNSNames {
			if ulid.ValidPrefix(name, ulid.PrefixRunner) {
				found = append(found, name)
			}
		}
		switch {
		case len(found) > 1:
			return "", fmt.Errorf("%s carries %d runner IDs as DNS SANs; want at most one", path, len(found))
		case len(found) == 1 && id != "" && found[0] != id:
			return "", fmt.Errorf("registry and tunnel client certificates name different runner IDs (%s, %s)", id, found[0])
		case len(found) == 1:
			id = found[0]
		}
	}
	return id, nil
}

func readLeafCertificate(path string) (*x509.Certificate, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read client certificate: %w", err)
	}
	for block, rest := pem.Decode(data); block != nil; block, rest = pem.Decode(rest) {
		if block.Type == "CERTIFICATE" {
			cert, err := x509.ParseCertificate(block.Bytes)
			if err != nil {
				return nil, fmt.Errorf("parse client certificate %s: %w", path, err)
			}
			return cert, nil
		}
	}
	return nil, fmt.Errorf("no certificate in %s", path)
}

func verifyAPIIdentity(cert *x509.Certificate, identity string) error {
	for _, name := range cert.DNSNames {
		if name == identity {
			return nil
		}
	}
	return fmt.Errorf("runner TCP client certificate must have DNS SAN %q", identity)
}

func validateRunnerTCP(addr string, cfg tlsutil.Config, insecure bool) error {
	if cfg.CAFile != "" && cfg.CertFile != "" && cfg.KeyFile != "" {
		return nil
	}
	if !insecure || cfg.Enabled() {
		return fmt.Errorf("runner TCP requires VORA_RUNNER_TLS_{CERT,KEY,CA}; for local plaintext use VORA_RUNNER_INSECURE=1 on loopback")
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("runner TCP address: %w", err)
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("insecure runner TCP must bind to a loopback IP")
	}
	return nil
}

func main() {
	if err := run(); err != nil {
		slog.Error("runner exited with error", "error", err)
		os.Exit(1)
	}
}

func run() error {
	socketFlag := flag.String("socket", "", "runner control socket (overrides VORA_SOCKET)")
	stateFlag := flag.String("state-dir", "", "state directory (overrides VORA_STATE_DIR)")
	flag.Parse()

	if dir := defaultRuntimeDir(os.Getenv("XDG_RUNTIME_DIR"), os.Geteuid()); dir != "" {
		if err := os.Setenv("XDG_RUNTIME_DIR", dir); err != nil {
			return err
		}
	}
	cfg, err := config.Default()
	if err != nil {
		return err
	}
	if cfg.AuditRetention, err = config.AuditRetentionFromEnv(); err != nil {
		return err
	}
	if cfg.Backends, err = config.BackendsFromEnv(); err != nil {
		return err
	}
	if *socketFlag != "" {
		cfg.SocketPath = *socketFlag
	}
	if *stateFlag != "" {
		cfg.StateDir = *stateFlag
	}

	logger := slog.New(telemetry.ContextHandler{Handler: slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})})
	slog.SetDefault(logger)

	shutdownTracing, err := telemetry.SetupTracing(context.Background(), telemetry.TracingConfig{
		Endpoint:    os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"),
		ServiceName: "vora-runner",
		Insecure:    os.Getenv("OTEL_EXPORTER_OTLP_INSECURE") == "true",
	})
	if err != nil {
		return fmt.Errorf("tracing: %w", err)
	}
	defer func() { _ = shutdownTracing(context.Background()) }()

	if err := ensurePrivateDir(cfg.SandboxesDir()); err != nil {
		return fmt.Errorf("create state dir: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(cfg.SocketPath), 0o755); err != nil {
		return fmt.Errorf("create socket dir: %w", err)
	}
	if err := removeStaleSocket(cfg.SocketPath); err != nil {
		return err
	}

	store := sandbox.NewStore(cfg.SandboxesDir())
	registry := runtime.NewRegistry()
	metrics := telemetry.New()
	// Only the client certificates of channels in use: VORA_RUNNER_TLS_CERT is
	// this runner's server certificate and names no runner.
	var idCerts []string
	if os.Getenv("VORA_API_GRPC") != "" && os.Getenv("VORA_API_REGISTRY_TLS_CERT") != "" {
		idCerts = append(idCerts, os.Getenv("VORA_API_REGISTRY_TLS_CERT"))
	}
	if os.Getenv("VORA_API_TUNNEL_ADDR") != "" && os.Getenv("VORA_API_TUNNEL_TLS_CERT") != "" {
		idCerts = append(idCerts, os.Getenv("VORA_API_TUNNEL_TLS_CERT"))
	}
	runnerID, err := runnerIDFromCerts(idCerts...)
	if err != nil {
		return fmt.Errorf("runner id: %w", err)
	}
	if runnerID == "" {
		runnerID = ulid.NewRunner()
		if len(idCerts) > 0 {
			logger.Warn("client certificate carries no runner ID DNS SAN; using a new ID for this start, which vora-api rejects when it requires runner identity",
				"runner_id", runnerID)
		}
	}
	if err := requireImageStore(cfg.ImagesMode, cfg.ContainerdAddress, vctr.Available); err != nil {
		return err
	}
	server := runner.NewServer(cfg, store, registry, runnerID, logger)
	server.SetMetrics(metrics)

	// Configure durable audit before exposing any runner endpoint or starting a
	// restricted-network proxy. With a DSN set, an unreachable or unmigratable
	// database is fatal: serving on the ephemeral JSONL store instead would hand
	// the operator a weaker audit guarantee than they configured.
	if dsn := os.Getenv("VORA_AUDIT_DSN"); dsn != "" {
		auditCtx, auditCancel := context.WithTimeout(context.Background(), 15*time.Second)
		auditStore, err := openAudit(auditCtx, dsn)
		auditCancel()
		if err != nil {
			return err
		}
		server.SetAudit(auditStore)
		defer auditStore.Close()
		logger.Info("audit store: postgres")
	}

	manager, err := registerBackends(cfg, registry, logger, server.RecordEgress)
	if err != nil {
		return err
	}
	if manager != nil {
		server.SetCgroups(manager)
	}

	lis, err := net.Listen("unix", cfg.SocketPath)
	if err != nil {
		return fmt.Errorf("listen %s: %w", cfg.SocketPath, err)
	}
	defer lis.Close()
	if err := os.Chmod(cfg.SocketPath, 0o600); err != nil {
		return fmt.Errorf("chmod socket: %w", err)
	}

	grpcServer := grpc.NewServer(grpc.StatsHandler(otelgrpc.NewServerHandler()))
	v1.RegisterRunnerServiceServer(grpcServer, server)

	// Optional reverse tunnel: connect out to vora-api and serve RunnerService
	// over it, so a runner behind NAT can be driven by the control plane. When
	// tunnelled the runner registers with no dialable address. The client is
	// started once the run context exists, below.
	tunnelAddr := os.Getenv("VORA_API_TUNNEL_ADDR")

	// Optional TCP gRPC endpoint so a remote control plane can reach this
	// runner. The Unix socket remains the local CLI path.
	var tcpServer *grpc.Server
	if addr := os.Getenv("VORA_RUNNER_ADDR"); addr != "" {
		runnerTLS := tlsutil.Config{
			CertFile: os.Getenv("VORA_RUNNER_TLS_CERT"),
			KeyFile:  os.Getenv("VORA_RUNNER_TLS_KEY"),
			CAFile:   os.Getenv("VORA_RUNNER_TLS_CA"),
		}
		if err := validateRunnerTCP(addr, runnerTLS, os.Getenv("VORA_RUNNER_INSECURE") == "1"); err != nil {
			return err
		}
		tcpLn, err := net.Listen("tcp", addr)
		if err != nil {
			return fmt.Errorf("listen tcp %s: %w", addr, err)
		}
		defer tcpLn.Close()
		var tcpOpts []grpc.ServerOption
		if runnerTLS.Enabled() {
			cfg, err := runnerTLS.Server()
			if err != nil {
				return fmt.Errorf("runner tls: %w", err)
			}
			identity := os.Getenv("VORA_RUNNER_CLIENT_IDENTITY")
			if identity == "" {
				identity = "vora-api"
			}
			cfg.VerifyConnection = func(cs tls.ConnectionState) error {
				if len(cs.PeerCertificates) == 0 {
					return fmt.Errorf("runner TCP requires an API client certificate")
				}
				return verifyAPIIdentity(cs.PeerCertificates[0], identity)
			}
			tcpOpts = append(tcpOpts, grpc.Creds(credentials.NewTLS(cfg)))
		} else if os.Getenv("VORA_ALLOW_INSECURE_BROKER") != "1" {
			// A plaintext remote endpoint must not carry brokered credentials.
			tcpOpts = append(tcpOpts, grpc.UnaryInterceptor(runner.RejectBrokerSecrets))
			logger.Warn("runner gRPC endpoint is plaintext; brokered create requests will be refused",
				"hint", "set VORA_RUNNER_TLS_{CERT,KEY,CA}")
		}
		tcpServer = grpc.NewServer(append(tcpOpts, grpc.StatsHandler(otelgrpc.NewServerHandler()))...)
		v1.RegisterRunnerServiceServer(tcpServer, server)
		go func() { _ = tcpServer.Serve(tcpLn) }()
		logger.Info("runner gRPC listening", "addr", addr, "tls", runnerTLS.Enabled())
	}
	defer func() {
		if tcpServer != nil {
			tcpServer.GracefulStop()
		}
	}()

	logger.Info("vora-runner listening",
		"version", version.String(),
		"socket", cfg.SocketPath,
		"state_dir", cfg.StateDir,
		"runner_id", runnerID,
		"systemd_mode", string(cfg.SystemdMode),
	)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Reverse tunnel: serve RunnerService to vora-api over a connection the
	// runner opens, for the NAT'd case.
	if tunnelAddr != "" {
		tunnelTLS := tlsutil.Config{CertFile: os.Getenv("VORA_API_TUNNEL_TLS_CERT"), KeyFile: os.Getenv("VORA_API_TUNNEL_TLS_KEY"), CAFile: os.Getenv("VORA_API_TUNNEL_TLS_CA")}
		if err := validateRunnerTCP(tunnelAddr, tunnelTLS, os.Getenv("VORA_TUNNEL_INSECURE") == "1"); err != nil {
			return fmt.Errorf("runner tunnel: %w", err)
		}
		tc := &tunnel.Client{
			Addr:      tunnelAddr,
			RunnerID:  runnerID,
			JoinToken: os.Getenv("VORA_API_JOIN_TOKEN"),
			Logger:    logger,
		}
		if tunnelTLS.Enabled() {
			host, _, err := net.SplitHostPort(tunnelAddr)
			if err != nil {
				return fmt.Errorf("runner tunnel address: %w", err)
			}
			tc.TLS, err = tunnelTLS.ClientFor(host)
			if err != nil {
				return fmt.Errorf("runner tunnel TLS: %w", err)
			}
		}
		if tunnelLn, err := tc.Listen(ctx); err != nil {
			logger.Warn("tunnel connect failed", "addr", tunnelAddr, "error", err)
		} else {
			tunnelOpts := []grpc.ServerOption{grpc.StatsHandler(otelgrpc.NewServerHandler())}
			if !tunnelTLS.Enabled() && os.Getenv("VORA_ALLOW_INSECURE_BROKER") != "1" {
				tunnelOpts = append(tunnelOpts, grpc.UnaryInterceptor(runner.RejectBrokerSecrets))
			}
			tunnelServer := grpc.NewServer(tunnelOpts...)
			v1.RegisterRunnerServiceServer(tunnelServer, server)
			go func() {
				if err := tunnelServer.Serve(tunnelLn); err != nil && !errors.Is(err, grpc.ErrServerStopped) {
					logger.Warn("tunnel serve stopped", "error", err)
				}
			}()
			logger.Info("tunnel connected", "addr", tunnelAddr)
		}
	}

	// Register with vora-api and heartbeat, when configured. The runner dials
	// out, so it can sit behind NAT.
	if apiAddr := os.Getenv("VORA_API_GRPC"); apiAddr != "" {
		apiTLS := tlsutil.Config{
			CertFile: os.Getenv("VORA_API_REGISTRY_TLS_CERT"),
			KeyFile:  os.Getenv("VORA_API_REGISTRY_TLS_KEY"),
			CAFile:   os.Getenv("VORA_API_REGISTRY_TLS_CA"),
		}
		var dialOpts []grpc.DialOption
		if apiTLS.Enabled() {
			cfg, err := apiTLS.Client()
			if err != nil {
				return fmt.Errorf("api client tls: %w", err)
			}
			dialOpts = append(dialOpts, grpc.WithTransportCredentials(credentials.NewTLS(cfg)))
		}
		rc, err := runregistry.Dial(apiAddr, os.Getenv("VORA_API_JOIN_TOKEN"), dialOpts...)
		if err != nil {
			logger.Warn("runner registry: dial failed", "error", err)
		} else {
			defer rc.Close()
			advertise := os.Getenv("VORA_RUNNER_ADVERTISE_ADDR")
			if advertise == "" {
				advertise = os.Getenv("VORA_RUNNER_ADDR")
			}
			if tunnelAddr != "" {
				advertise = "" // reached through the tunnel, not a dialable address
			}
			var hostInfo unix.Sysinfo_t
			if err := unix.Sysinfo(&hostInfo); err != nil {
				logger.Warn("cannot detect runner memory capacity", "error", err)
			}
			reporter := &runregistry.Reporter{
				Client:       rc,
				RunnerID:     runnerID,
				Address:      advertise,
				Version:      version.String(),
				Backends:     registry.Names(),
				NetworkModes: advertisedNetworkModes(registry),
				Running:      func() int32 { return int32(server.RunningCount()) },
				CPUMillis:    int64(goruntime.NumCPU()) * 1000,
				MemoryBytes:  int64(hostInfo.Totalram) * int64(hostInfo.Unit),
				MaxSandboxes: cfg.MaxSandboxes,
				Logger:       logger,
			}
			go reporter.Run(ctx)
			logger.Info("registering with vora-api", "api", apiAddr, "advertise", advertise)
		}
	}

	if err := server.Reconcile(ctx); err != nil {
		logger.Warn("startup reconciliation failed", "error", err)
	}
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := server.ReapExpired(ctx); err != nil {
					logger.Warn("ttl reaper failed", "error", err)
				}
				if err := server.FlushEgress(ctx); err != nil {
					logger.Warn("egress summary flush failed", "error", err)
				}
				if err := server.EnforceDiskQuota(ctx); err != nil {
					logger.Warn("disk quota enforcer failed", "error", err)
				}
			}
		}
	}()
	// Retention is measured in hours and scans every tombstone, so it runs on
	// its own slow ticker rather than with the 30s reaper.
	go func() {
		ticker := time.NewTicker(auditPruneInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := server.PruneAudit(ctx); err != nil {
					logger.Warn("audit retention failed", "error", err)
				}
			}
		}
	}()

	go func() {
		if err := metrics.Serve(ctx, cfg.MetricsAddr, logger); err != nil {
			logger.Warn("metrics server stopped", "error", err)
		}
	}()

	errCh := make(chan error, 1)
	go func() {
		if err := grpcServer.Serve(lis); err != nil && !errors.Is(err, grpc.ErrServerStopped) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return fmt.Errorf("grpc serve: %w", err)
	case <-ctx.Done():
		logger.Info("shutdown signal received, draining")
	}

	stopped := make(chan struct{})
	go func() {
		grpcServer.GracefulStop()
		close(stopped)
	}()
	select {
	case <-stopped:
		logger.Info("runner stopped cleanly")
	case <-time.After(10 * time.Second):
		logger.Warn("graceful shutdown timed out, forcing stop")
		grpcServer.Stop()
	}

	if err := os.Remove(cfg.SocketPath); err != nil && !os.IsNotExist(err) {
		logger.Warn("failed to remove socket", "error", err)
	}
	return nil
}

// registerBackends registers the isolation backends cfg.Backends selects. It
// returns the cgroup manager when bwrap was selected and systemd is usable.
func registerBackends(cfg config.Config, registry *runtime.Registry, logger *slog.Logger, egressSink func(egress.Event) error) (cgroup.Manager, error) {
	subtree, err := cgroup.NewSubtree()
	if err != nil {
		logger.Warn("cgroup subtree unavailable; sandbox resource limits cannot be enforced", "error", err)
		subtree = nil
	}

	networks := network.NewManager(cfg.StateDir, network.ManagerConfig{
		ProxyPort:  network.DefaultProxyPort,
		BrokerPort: cfg.EgressBrokerPort,
	}, logger)

	var manager cgroup.Manager
	builders := []backendBuilder{
		{config.BackendBwrap, func() (runtime.Backend, error) {
			m, err := cgroup.New(cfg)
			if err != nil {
				return nil, fmt.Errorf("cgroup manager unavailable: %w", err)
			}
			manager = m
			backend, err := bwrap.New(cfg, m, logger)
			if err != nil {
				return nil, err
			}
			if subtree != nil {
				backend.EnableNetns(subtree, networks, egressSink)
			}
			return backend, nil
		}},
		{config.BackendGvisor, func() (runtime.Backend, error) {
			backend, err := gvisor.New(cfg, logger, subtree)
			if err != nil {
				return nil, err
			}
			if subtree != nil {
				backend.EnableNetns(networks, egressSink)
			}
			return backend, nil
		}},
	}
	err = registerSelected(registry, cfg.Backends, builders, logger)
	return manager, err
}

// backendBuilder constructs one isolation backend. registerSelected calls
// build only for a selected backend, so an unselected one's host setup never
// runs.
type backendBuilder struct {
	name  string
	build func() (runtime.Backend, error)
}

// registerSelected builds and registers, in order, each backend that want
// allows; the first registered becomes the default. Without an allowlist a
// backend that fails is skipped: the runner still serves create/get/list, and
// start returns a clear error. With one, a listed backend that fails is an
// error, so the runner exits rather than register with vora-api without it.
func registerSelected(reg *runtime.Registry, want config.BackendSet, builders []backendBuilder, logger *slog.Logger) error {
	for _, b := range builders {
		if !want.Allows(b.name) {
			logger.Info("runtime backend not selected by VORA_BACKENDS", "backend", b.name)
			continue
		}
		backend, err := b.build()
		if err == nil {
			err = reg.Register(backend)
		}
		if err != nil {
			if want.Explicit() {
				return fmt.Errorf("backend %s, listed in VORA_BACKENDS, failed to start: %w", b.name, err)
			}
			logger.Warn("runtime backend unavailable", "backend", b.name, "error", err)
			continue
		}
		caps := backend.Capabilities()
		logger.Info("registered runtime backend", "backend", b.name, "network_modes", caps.NetworkModes, "limits", caps.EnforcesResourceLimits)
	}
	for _, name := range want.Names() {
		if _, ok := reg.Get(name); !ok {
			return fmt.Errorf("VORA_BACKENDS lists %s, but this runner has no such backend", name)
		}
	}
	return nil
}

// removeStaleSocket removes a leftover socket file if no process is listening.
func removeStaleSocket(path string) error {
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	conn, err := net.DialTimeout("unix", path, time.Second)
	if err == nil {
		conn.Close()
		return fmt.Errorf("socket %s is already in use by another runner", path)
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("remove stale socket %s: %w", path, err)
	}
	return nil
}

// advertisedNetworkModes is the union of the network modes the registered
// backends can enforce, for capability-aware placement. It is a per-runner
// signal: auto-select chooses a backend that supports the requested mode, and
// the runner validates the pair at start.
func advertisedNetworkModes(reg *runtime.Registry) []string {
	seen := map[string]bool{}
	var out []string
	for _, name := range reg.Names() {
		b, ok := reg.Get(name)
		if !ok {
			continue
		}
		for _, m := range b.Capabilities().NetworkModes {
			s := string(m)
			if !seen[s] {
				seen[s] = true
				out = append(out, s)
			}
		}
	}
	sort.Strings(out)
	return out
}
