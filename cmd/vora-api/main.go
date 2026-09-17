// Command vora-api is the Vora control plane: an HTTP/JSON API for external
// clients plus a gRPC runner registry that runners dial out to.
package main

import (
	"context"
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

	v1 "github.com/vorash/vora/api/proto/v1"
	"github.com/vorash/vora/internal/api"
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
	if dsn := os.Getenv("VORA_API_DSN"); dsn != "" {
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

	server := api.New(api.Options{
		Store:        store,
		Logger:       logger,
		JoinToken:    os.Getenv("VORA_API_JOIN_TOKEN"),
		BootstrapKey: os.Getenv("VORA_API_BOOTSTRAP_KEY"),
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
	grpcSrv := grpc.NewServer()
	v1.RegisterRunnerRegistryServer(grpcSrv, server)

	logger.Info("vora-api listening",
		"version", version.String(), "http", *httpFlag, "grpc", *grpcFlag,
		"join_token", os.Getenv("VORA_API_JOIN_TOKEN") != "",
		"bootstrap_key", os.Getenv("VORA_API_BOOTSTRAP_KEY") != "",
	)

	errCh := make(chan error, 2)
	go func() {
		if err := httpSrv.Serve(httpLn); err != nil && !errors.Is(err, http.ErrServerClosed) {
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
