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

	"google.golang.org/grpc"

	v1 "github.com/vorash/vora/api/proto/v1"
	"github.com/vorash/vora/internal/audit/postgres"
	"github.com/vorash/vora/internal/cgroup"
	"github.com/vorash/vora/internal/config"
	"github.com/vorash/vora/internal/runner"
	"github.com/vorash/vora/internal/runtime"
	"github.com/vorash/vora/internal/runtime/bwrap"
	"github.com/vorash/vora/internal/runtime/gvisor"
	"github.com/vorash/vora/internal/sandbox"
	"github.com/vorash/vora/internal/telemetry"
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

	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

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
	manager := registerBackends(cfg, registry, logger)
	runnerID := ulid.NewRunner()
	server := runner.NewServer(cfg, store, registry, runnerID, logger)
	metrics := telemetry.New()
	server.SetMetrics(metrics)
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

	grpcServer := grpc.NewServer()
	v1.RegisterRunnerServiceServer(grpcServer, server)

	logger.Info("vora-runner listening",
		"version", version.String(),
		"socket", cfg.SocketPath,
		"state_dir", cfg.StateDir,
		"runner_id", runnerID,
		"systemd_mode", string(cfg.SystemdMode),
	)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// PostgreSQL/Supabase audit store, when a DSN is configured. Failure is not
	// fatal: the runner falls back to the JSONL store.
	if dsn := os.Getenv("VORA_AUDIT_DSN"); dsn != "" {
		auditStore, err := postgres.Open(ctx, dsn)
		if err != nil {
			logger.Warn("audit: postgres unavailable, using jsonl", "error", err)
		} else if err := auditStore.Migrate(ctx); err != nil {
			logger.Warn("audit: migration failed, using jsonl", "error", err)
			auditStore.Close()
		} else {
			server.SetAudit(auditStore)
			defer auditStore.Close()
			logger.Info("audit store: postgres")
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
func registerBackends(cfg config.Config, registry *runtime.Registry, logger *slog.Logger) cgroup.Manager {
	manager, err := cgroup.New(cfg)
	if err != nil {
		logger.Warn("cgroup manager unavailable; bwrap sandboxes cannot be started", "error", err)
	} else if backend, err := bwrap.New(cfg, manager, logger); err != nil {
		logger.Warn("bubblewrap backend unavailable", "error", err)
	} else if err := registry.Register(backend); err != nil {
		logger.Warn("register backend failed", "backend", backend.Name(), "error", err)
	} else {
		logger.Info("registered runtime backend", "backend", backend.Name())
	}

	subtree, err := cgroup.NewSubtree()
	if err != nil {
		logger.Warn("cgroup subtree unavailable; gVisor cannot enforce resource limits", "error", err)
		subtree = nil
	}
	if backend, err := gvisor.New(cfg, logger, subtree); err != nil {
		logger.Warn("gvisor backend unavailable", "error", err)
	} else if err := registry.Register(backend); err != nil {
		logger.Warn("register backend failed", "backend", backend.Name(), "error", err)
	} else {
		logger.Info("registered runtime backend", "backend", backend.Name(), "limits", backend.Capabilities().EnforcesResourceLimits)
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
