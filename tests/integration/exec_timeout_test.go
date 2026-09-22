//go:build integration

package integration

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	v1 "github.com/vorash/vora/api/proto/v1"
	"github.com/vorash/vora/internal/cgroup"
	"github.com/vorash/vora/internal/config"
	"github.com/vorash/vora/internal/runner"
	"github.com/vorash/vora/internal/runtime"
	"github.com/vorash/vora/internal/runtime/bwrap"
	"github.com/vorash/vora/internal/sandbox"
	"github.com/vorash/vora/internal/ulid"
)

// newExecClient starts a real bwrap-backed runner on a temp socket.
func newExecClient(t *testing.T) v1.RunnerServiceClient {
	t.Helper()
	initBin := buildBinary(t, "vora-init")
	t.Setenv("VORA_INIT_BINARY", initBin)

	cgroups, err := cgroup.New(config.Config{SystemdMode: config.SystemdUser})
	if err != nil {
		t.Skipf("systemd user manager unavailable: %v", err)
	}
	rootfs := imageRootfs(t, "base")
	realImages := filepath.Dir(filepath.Dir(filepath.Dir(rootfs))) // .../images

	stateDir := t.TempDir()
	if err := os.Symlink(realImages, filepath.Join(stateDir, "images")); err != nil {
		t.Fatalf("symlink images: %v", err)
	}
	cfg := config.Config{StateDir: stateDir, SystemdMode: config.SystemdUser, DefaultTTL: 5 * time.Minute}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	backend, err := bwrap.New(cfg, cgroups, logger)
	if err != nil {
		t.Fatalf("bwrap backend: %v", err)
	}
	registry := runtime.NewRegistry()
	if err := registry.Register(backend); err != nil {
		t.Fatalf("register: %v", err)
	}
	srv := runner.NewServer(cfg, sandbox.NewStore(cfg.SandboxesDir()), registry, ulid.NewRunner(), logger)

	socket := filepath.Join(t.TempDir(), "runner.sock")
	lis, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	grpcServer := grpc.NewServer()
	v1.RegisterRunnerServiceServer(grpcServer, srv)
	go func() { _ = grpcServer.Serve(lis) }()
	t.Cleanup(func() { grpcServer.Stop(); lis.Close() })

	conn, err := grpc.NewClient("unix://"+socket, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return v1.NewRunnerServiceClient(conn)
}

// runExecOnce starts a sandbox, runs one command and returns its exit record,
// or nil when the stream ends without one.
func runExecOnce(t *testing.T, client v1.RunnerServiceClient, exec *v1.ExecRequest) (*v1.ExecExit, time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	created, err := client.CreateSandbox(ctx, &v1.CreateSandboxRequest{Spec: spec("")})
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	id := created.GetSandbox().GetId()
	if _, err := client.StartSandbox(ctx, &v1.StartSandboxRequest{SandboxId: id}); err != nil {
		t.Fatalf("StartSandbox: %v", err)
	}
	t.Cleanup(func() {
		destroyCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_, _ = client.DestroySandbox(destroyCtx, &v1.DestroySandboxRequest{SandboxId: id})
	})

	start := time.Now()
	stream, err := client.ExecSandbox(ctx, &v1.ExecSandboxRequest{SandboxId: id, Exec: exec})
	if err != nil {
		t.Fatalf("ExecSandbox: %v", err)
	}
	var exit *v1.ExecExit
	for {
		chunk, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return exit, time.Since(start)
		}
		if err != nil {
			t.Fatalf("recv: %v", err)
		}
		if e := chunk.GetExit(); e != nil {
			exit = e
		}
	}
}

// TestExecTimeoutKillsAndReports proves the timeout contract: ExecRequest
// timeout_seconds must actually cut a runaway command — and say so with
// TIMEOUT_EXCEEDED on the exit record, promptly. Before the fix it was
// silently ignored and the run died much later as an unexplained
// RUNTIME_FAILURE.
func TestExecTimeoutKillsAndReports(t *testing.T) {
	client := newExecClient(t)
	exit, elapsed := runExecOnce(t, client, &v1.ExecRequest{
		Command: "sh", Args: []string{"-c", "sleep 30"},
		TimeoutSeconds: 2,
	})
	if exit == nil {
		t.Fatal("no exit record: the stream must end with one even for a killed run")
	}
	if exit.GetReason() != v1.ExitReason_TIMEOUT_EXCEEDED {
		t.Fatalf("reason = %v, want TIMEOUT_EXCEEDED", exit.GetReason())
	}
	if elapsed > 15*time.Second {
		t.Fatalf("timeout took %s, want ~2s", elapsed)
	}
}

// TestExecOutlivesWatchdogGrace pins the watchdog fix: a silent run that
// outlives the agent's runner-detached grace must complete normally. Open exec
// streams are proof of life; before the fix the watchdog tore the sandbox down
// mid-run and the command died as RUNTIME_FAILURE after ~26s.
func TestExecOutlivesWatchdogGrace(t *testing.T) {
	client := newExecClient(t)
	start := time.Now()
	exit, _ := runExecOnce(t, client, &v1.ExecRequest{
		Command: "sh", Args: []string{"-c", "sleep 25"},
	})
	if exit == nil || exit.GetExitCode() != 0 {
		t.Fatalf("exit = %v, want a clean exit after sleeping past the 20s grace", exit)
	}
	if exit.GetReason() != v1.ExitReason_EXITED {
		t.Fatalf("reason = %v, want EXITED", exit.GetReason())
	}
	if time.Since(start) < 24*time.Second {
		t.Fatal("test finished suspiciously fast: the sleep did not run to completion")
	}
}
