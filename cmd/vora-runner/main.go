// Command vora-runner is the sandbox runner daemon.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	v1 "github.com/vorash/vora/api/proto/v1"
	"github.com/vorash/vora/internal/audit/postgres"
	"github.com/vorash/vora/internal/cgroup"
	"github.com/vorash/vora/internal/config"
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
	"github.com/vorash/vora/internal/ulid"
	"github.com/vorash/vora/internal/version"
)

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

	cfg, err := config.Default()
	if err != nil {
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
		Insecure:    true,
	})
	if err != nil {
		return fmt.Errorf("tracing: %w", err)
	}
	defer func() { _ = shutdownTracing(context.Background()) }()

	if err := os.MkdirAll(cfg.SandboxesDir(), 0o755); err != nil {
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
	runnerID := ulid.NewRunner()
	server := runner.NewServer(cfg, store, registry, runnerID, logger)
	server.SetMetrics(metrics)

	// Configure durable audit before exposing any runner endpoint or starting a
	// restricted-network proxy.
	if dsn := os.Getenv("VORA_AUDIT_DSN"); dsn != "" {
		auditCtx, auditCancel := context.WithTimeout(context.Background(), 15*time.Second)
		auditStore, err := postgres.Open(auditCtx, dsn)
		if err != nil {
			logger.Warn("audit: postgres unavailable, using jsonl", "error", err)
		} else if err := auditStore.Migrate(auditCtx); err != nil {
			logger.Warn("audit: migration failed, using jsonl", "error", err)
			auditStore.Close()
		} else {
			server.SetAudit(auditStore)
			defer auditStore.Close()
			logger.Info("audit store: postgres")
		}
		auditCancel()
	}

	manager := registerBackends(cfg, registry, logger, server.RecordEgress)
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

	// Optional TCP gRPC endpoint so a remote control plane can reach this
	// runner. The Unix socket remains the local CLI path.
	var tcpServer *grpc.Server
	if addr := os.Getenv("VORA_RUNNER_ADDR"); addr != "" {
		tcpLn, err := net.Listen("tcp", addr)
		if err != nil {
			return fmt.Errorf("listen tcp %s: %w", addr, err)
		}
		defer tcpLn.Close()
		runnerTLS := tlsutil.Config{
			CertFile: os.Getenv("VORA_RUNNER_TLS_CERT"),
			KeyFile:  os.Getenv("VORA_RUNNER_TLS_KEY"),
			CAFile:   os.Getenv("VORA_RUNNER_TLS_CA"),
		}
		var tcpOpts []grpc.ServerOption
		if runnerTLS.Enabled() {
			cfg, err := runnerTLS.Server()
			if err != nil {
				return fmt.Errorf("runner tls: %w", err)
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
			reporter := &runregistry.Reporter{
				Client:   rc,
				RunnerID: runnerID,
				Address:  advertise,
				Version:  version.String(),
				Backends: registry.Names(),
				Running:  func() int32 { return int32(server.RunningCount()) },
				Logger:   logger,
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
				if err := server.EnforceDiskQuota(ctx); err != nil {
					logger.Warn("disk quota enforcer failed", "error", err)
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

// registerBackends registers the isolation backends available on this host.
// A missing backend is not fatal: the runner still serves create/get/list, and
// start returns a clear error.
func registerBackends(cfg config.Config, registry *runtime.Registry, logger *slog.Logger, egressSink func(egress.Event) error) cgroup.Manager {
	manager, err := cgroup.New(cfg)
	if err != nil {
		logger.Warn("cgroup manager unavailable; bwrap sandboxes cannot be started", "error", err)
	}

	subtree, err := cgroup.NewSubtree()
	if err != nil {
		logger.Warn("cgroup subtree unavailable; sandbox resource limits cannot be enforced", "error", err)
		subtree = nil
	}

	networks := network.NewManager(cfg.StateDir, network.ManagerConfig{
		ProxyPort:  network.DefaultProxyPort,
		BrokerPort: cfg.EgressBrokerPort,
	}, logger)

	if manager != nil {
		if backend, err := bwrap.New(cfg, manager, logger); err != nil {
			logger.Warn("bubblewrap backend unavailable", "error", err)
		} else {
			if subtree != nil {
				backend.EnableNetns(subtree, networks, egressSink)
			}
			if err := registry.Register(backend); err != nil {
				logger.Warn("register backend failed", "backend", backend.Name(), "error", err)
			} else {
				logger.Info("registered runtime backend", "backend", backend.Name(), "network_modes", backend.Capabilities().NetworkModes)
			}
		}
	}

	if backend, err := gvisor.New(cfg, logger, subtree); err != nil {
		logger.Warn("gvisor backend unavailable", "error", err)
	} else {
		if subtree != nil {
			backend.EnableNetns(networks, egressSink)
		}
		if err := registry.Register(backend); err != nil {
			logger.Warn("register backend failed", "backend", backend.Name(), "error", err)
		} else {
			logger.Info("registered runtime backend", "backend", backend.Name(), "limits", backend.Capabilities().EnforcesResourceLimits)
		}
	}

	return manager
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
