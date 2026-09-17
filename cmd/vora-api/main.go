// Command vora-api is the Vora control plane: an HTTP/JSON API for external
// clients plus a gRPC runner registry that runners dial out to.
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	v1 "github.com/vorash/vora/api/proto/v1"
	"github.com/vorash/vora/internal/api"
	"github.com/vorash/vora/internal/secret"
	"github.com/vorash/vora/internal/tlsutil"
	"github.com/vorash/vora/internal/version"
)

func main() {
	if err := run(); err != nil {
		slog.Error("vora-api exited with error", "error", err)
		os.Exit(1)
	}
}

func run() error {
	httpFlag := flag.String("http", env("VORA_API_HTTP_ADDR", "127.0.0.1:9300"), "HTTP/JSON listen address")
	grpcFlag := flag.String("grpc", env("VORA_API_GRPC_ADDR", "127.0.0.1:9301"), "gRPC runner registry listen address")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	var store api.Store = api.NewMemStore()
	dsn := os.Getenv("VORA_API_DSN")
	if dsn != "" {
		pg, err := api.OpenPostgres(ctx, dsn)
		if err != nil {
			return fmt.Errorf("api store: %w", err)
		}
		defer pg.Close()
		store = pg
		logger.Info("api store: postgres")
	} else {
		logger.Warn("VORA_API_DSN not set; using an in-memory store (keys and runners are not persisted)")
	}

	// Credential brokering is opt-in: without a master key the secret
	// endpoints report that brokering is not configured instead of storing
	// credentials under a weak key.
	var secrets secret.Store
	if masterKey := os.Getenv("VORA_MASTER_KEY"); masterKey != "" {
		sealer, err := secret.NewSealer(masterKey)
		if err != nil {
			return fmt.Errorf("VORA_MASTER_KEY: %w", err)
		}
		if dsn != "" {
			pgSecrets, err := secret.OpenPostgres(ctx, dsn, sealer)
			if err != nil {
				return fmt.Errorf("secret store: %w", err)
			}
			defer pgSecrets.Close()
			secrets = pgSecrets
			logger.Info("secret store: postgres")
		} else {
			secrets = secret.NewMemStore(sealer)
			logger.Warn("VORA_MASTER_KEY set without VORA_API_DSN; secrets are in-memory and will not survive a restart")
		}
	} else {
		logger.Warn("VORA_MASTER_KEY not set; credential brokering is disabled")
	}

	server := api.New(api.Options{
		Store:        store,
		Logger:       logger,
		JoinToken:    os.Getenv("VORA_API_JOIN_TOKEN"),
		BootstrapKey: os.Getenv("VORA_API_BOOTSTRAP_KEY"),
		Secrets:      secrets,
		RunnerTLS: tlsutil.Config{
			CertFile: os.Getenv("VORA_RUNNER_TLS_CERT"),
			KeyFile:  os.Getenv("VORA_RUNNER_TLS_KEY"),
			CAFile:   os.Getenv("VORA_RUNNER_TLS_CA"),
		},
	})
	defer server.Close()

	httpLn, err := net.Listen("tcp", *httpFlag)
	if err != nil {
		return fmt.Errorf("listen http %s: %w", *httpFlag, err)
	}
	defer httpLn.Close()
	grpcLn, err := net.Listen("tcp", *grpcFlag)
	if err != nil {
		return fmt.Errorf("listen grpc %s: %w", *grpcFlag, err)
	}
	defer grpcLn.Close()

	httpSrv := &http.Server{Handler: server.Handler(), ReadHeaderTimeout: 10 * time.Second}

	httpTLS := tlsutil.Config{
		CertFile: os.Getenv("VORA_API_HTTP_TLS_CERT"),
		KeyFile:  os.Getenv("VORA_API_HTTP_TLS_KEY"),
		CAFile:   os.Getenv("VORA_API_HTTP_TLS_CA"),
	}
	registryTLS := tlsutil.Config{
		CertFile: os.Getenv("VORA_API_REGISTRY_TLS_CERT"),
		KeyFile:  os.Getenv("VORA_API_REGISTRY_TLS_KEY"),
		CAFile:   os.Getenv("VORA_API_REGISTRY_TLS_CA"),
	}
	httpServeLn := net.Listener(httpLn)
	if httpTLS.Enabled() {
		cfg, err := httpTLS.Server()
		if err != nil {
			return fmt.Errorf("http tls: %w", err)
		}
		httpServeLn = tls.NewListener(httpLn, cfg)
	}

	var grpcOpts []grpc.ServerOption
	if registryTLS.Enabled() {
		cfg, err := registryTLS.Server()
		if err != nil {
			return fmt.Errorf("registry tls: %w", err)
		}
		grpcOpts = append(grpcOpts, grpc.Creds(credentials.NewTLS(cfg)))
	}
	grpcSrv := grpc.NewServer(grpcOpts...)
	v1.RegisterRunnerRegistryServer(grpcSrv, server)

	logger.Info("vora-api listening",
		"version", version.String(), "http", *httpFlag, "grpc", *grpcFlag,
		"join_token", os.Getenv("VORA_API_JOIN_TOKEN") != "",
		"bootstrap_key", os.Getenv("VORA_API_BOOTSTRAP_KEY") != "",
		"http_tls", httpTLS.Enabled(), "registry_tls", registryTLS.Enabled(),
		"runner_mtls", os.Getenv("VORA_RUNNER_TLS_CA") != "",
		"credential_brokering", secrets != nil,
	)

	errCh := make(chan error, 2)
	go func() {
		if err := httpSrv.Serve(httpServeLn); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("http serve: %w", err)
		}
	}()
	go func() {
		if err := grpcSrv.Serve(grpcLn); err != nil {
			errCh <- fmt.Errorf("grpc serve: %w", err)
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		logger.Info("shutdown signal received")
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(shutdownCtx)
	grpcSrv.GracefulStop()
	logger.Info("vora-api stopped")
	return nil
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
