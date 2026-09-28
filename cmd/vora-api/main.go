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

	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	v1 "github.com/vorash/vora/api/proto/v1"
	"github.com/vorash/vora/internal/api"
	"github.com/vorash/vora/internal/secret"
	"github.com/vorash/vora/internal/telemetry"
	"github.com/vorash/vora/internal/tlsutil"
	"github.com/vorash/vora/internal/tunnel"
	"github.com/vorash/vora/internal/version"
)

func requireRegistryTLS(addr string, cfg tlsutil.Config) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return nil
	}
	if cfg.CAFile == "" || cfg.CertFile == "" || cfg.KeyFile == "" {
		return fmt.Errorf("public runner registry requires VORA_API_REGISTRY_TLS_{CERT,KEY,CA}")
	}
	return nil
}

func requireTunnelTLS(addr string, cfg tlsutil.Config, insecure bool) error {
	if cfg.CAFile != "" && cfg.CertFile != "" && cfg.KeyFile != "" {
		return nil
	}
	if !insecure || cfg.Enabled() {
		return fmt.Errorf("public tunnel requires VORA_API_TUNNEL_TLS_{CERT,KEY,CA}")
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("plaintext tunnel must bind to a loopback IP")
	}
	return nil
}

func requireJoinToken(addr, token string) error {
	if token != "" {
		return nil
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("runner registry address: %w", err)
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("a public runner registry or tunnel requires VORA_API_JOIN_TOKEN")
	}
	return nil
}

// requireHTTPTLS refuses a plaintext HTTP API on a non-loopback address unless
// the operator opts in (TLS may be terminated by a proxy in front).
func requireHTTPTLS(addr string, cfg tlsutil.Config, insecure bool) error {
	if cfg.Enabled() || insecure {
		return nil
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("http address: %w", err)
	}
	if host == "localhost" {
		return nil
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return nil
	}
	return fmt.Errorf("plaintext HTTP API on %s would send API keys and sandbox I/O in clear: set VORA_API_HTTP_TLS_{CERT,KEY}, bind to loopback behind a TLS proxy, or set VORA_API_HTTP_INSECURE=1", addr)
}

// minBootstrapKeyLen matches VORA_MASTER_KEY: the bootstrap key is a global
// admin credential and must not be guessable.
const minBootstrapKeyLen = 32

func requireBootstrapKey(key string) error {
	if key != "" && len(key) < minBootstrapKeyLen {
		return fmt.Errorf("VORA_API_BOOTSTRAP_KEY must be at least %d characters", minBootstrapKeyLen)
	}
	return nil
}

// joinTokenWarning explains the exposure of a registry that authenticates
// runners with neither a join token nor client certificates. Empty when fine.
func joinTokenWarning(joinToken string, registryTLS tlsutil.Config) string {
	if joinToken != "" || registryTLS.CAFile != "" {
		return ""
	}
	return "runner registry has no VORA_API_JOIN_TOKEN and no client CA: any local process can register as a runner and will receive new sandboxes"
}

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

	logger := slog.New(telemetry.ContextHandler{Handler: slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})})
	slog.SetDefault(logger)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	shutdownTracing, err := telemetry.SetupTracing(ctx, telemetry.TracingConfig{
		Endpoint:    os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"),
		ServiceName: "vora-api",
		Insecure:    os.Getenv("OTEL_EXPORTER_OTLP_INSECURE") == "true",
	})
	if err != nil {
		return fmt.Errorf("tracing: %w", err)
	}
	defer func() { _ = shutdownTracing(context.Background()) }()

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

	runnerTLS := tlsutil.Config{
		CertFile: os.Getenv("VORA_RUNNER_TLS_CERT"),
		KeyFile:  os.Getenv("VORA_RUNNER_TLS_KEY"),
		CAFile:   os.Getenv("VORA_RUNNER_TLS_CA"),
	}
	allowInsecureBroker := os.Getenv("VORA_ALLOW_INSECURE_BROKER") == "1"
	if secrets != nil && !runnerTLS.Enabled() && !allowInsecureBroker {
		logger.Warn("credential brokering is enabled but the API-to-runner channel has no TLS; brokered creates will be refused",
			"hint", "set VORA_RUNNER_TLS_{CERT,KEY,CA} or VORA_ALLOW_INSECURE_BROKER=1")
	}

	joinToken := os.Getenv("VORA_API_JOIN_TOKEN")
	registryTLS := tlsutil.Config{CertFile: os.Getenv("VORA_API_REGISTRY_TLS_CERT"), KeyFile: os.Getenv("VORA_API_REGISTRY_TLS_KEY"), CAFile: os.Getenv("VORA_API_REGISTRY_TLS_CA")}
	if err := requireRegistryTLS(*grpcFlag, registryTLS); err != nil {
		return err
	}
	if err := requireJoinToken(*grpcFlag, joinToken); err != nil {
		return err
	}
	if warning := joinTokenWarning(joinToken, registryTLS); warning != "" {
		logger.Warn(warning, "hint", "set VORA_API_JOIN_TOKEN or VORA_API_REGISTRY_TLS_{CERT,KEY,CA}")
	}
	if tunnelAddr := os.Getenv("VORA_API_TUNNEL_ADDR"); tunnelAddr != "" {
		if err := requireJoinToken(tunnelAddr, joinToken); err != nil {
			return err
		}
	}

	if err := requireBootstrapKey(os.Getenv("VORA_API_BOOTSTRAP_KEY")); err != nil {
		return err
	}

	// Optional reverse tunnel: lets runners the API cannot dial (NAT) still be
	// driven. The runner connects out; the API multiplexes RunnerService over it.
	var tunnelSrv *tunnel.Server
	tunnelTLS := tlsutil.Config{CertFile: os.Getenv("VORA_API_TUNNEL_TLS_CERT"), KeyFile: os.Getenv("VORA_API_TUNNEL_TLS_KEY"), CAFile: os.Getenv("VORA_API_TUNNEL_TLS_CA")}
	if addr := os.Getenv("VORA_API_TUNNEL_ADDR"); addr != "" {
		if err := requireTunnelTLS(addr, tunnelTLS, os.Getenv("VORA_TUNNEL_INSECURE") == "1"); err != nil {
			return err
		}
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			return fmt.Errorf("listen tunnel %s: %w", addr, err)
		}
		defer ln.Close()
		tunnelSrv = tunnel.NewServer(joinToken, logger)
		serveLn := net.Listener(ln)
		if tunnelTLS.Enabled() {
			cfg, err := tunnelTLS.Server()
			if err != nil {
				return fmt.Errorf("tunnel tls: %w", err)
			}
			serveLn = tls.NewListener(ln, cfg)
			tunnelSrv.RequireTLS()
		}
		go func() {
			if err := tunnelSrv.Serve(ctx, serveLn); err != nil {
				logger.Warn("tunnel server stopped", "error", err)
			}
		}()
		logger.Info("tunnel listening", "addr", addr)
	}

	server := api.New(api.Options{
		Store:                 store,
		Logger:                logger,
		JoinToken:             joinToken,
		BootstrapKey:          os.Getenv("VORA_API_BOOTSTRAP_KEY"),
		Secrets:               secrets,
		AllowInsecureBroker:   allowInsecureBroker,
		RunnerTLS:             runnerTLS,
		RequireRunnerIdentity: registryTLS.CAFile != "",
		Tunnel:                tunnelSrv,
		TunnelTLS:             tunnelTLS.Enabled(),
	})
	defer server.Close()

	httpTLS := tlsutil.Config{
		CertFile: os.Getenv("VORA_API_HTTP_TLS_CERT"),
		KeyFile:  os.Getenv("VORA_API_HTTP_TLS_KEY"),
		CAFile:   os.Getenv("VORA_API_HTTP_TLS_CA"),
	}
	if err := requireHTTPTLS(*httpFlag, httpTLS, os.Getenv("VORA_API_HTTP_INSECURE") == "1"); err != nil {
		return err
	}

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

	httpSrv := &http.Server{
		Handler: server.Handler(), ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second,
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
	grpcOpts = append(grpcOpts, grpc.StatsHandler(otelgrpc.NewServerHandler()))
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
