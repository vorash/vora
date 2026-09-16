//go:build integration

// Package integration exercises the runner over a real Unix socket.
package integration

import (
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	v1 "github.com/vorash/vora/api/proto/v1"
	"github.com/vorash/vora/internal/config"
	"github.com/vorash/vora/internal/runner"
	"github.com/vorash/vora/internal/runtime"
	"github.com/vorash/vora/internal/sandbox"
	"github.com/vorash/vora/internal/ulid"
)

// startServer starts an in-process runner on a temp socket and returns a
// connected client plus a stop function.
func startServer(t *testing.T, stateDir, socket string) (v1.RunnerServiceClient, func()) {
	t.Helper()

	cfg := config.Config{
		StateDir:    stateDir,
		SocketPath:  socket,
		SystemdMode: config.SystemdUser,
		DefaultTTL:  time.Minute,
	}
	store := sandbox.NewStore(cfg.SandboxesDir())
	registry := runtime.NewRegistry()
	srv := runner.NewServer(cfg, store, registry, ulid.NewRunner(), nil)

	lis, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	grpcServer := grpc.NewServer()
	v1.RegisterRunnerServiceServer(grpcServer, srv)
	go func() { _ = grpcServer.Serve(lis) }()

	conn, err := grpc.NewClient("unix://"+socket,
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}

	stop := func() {
		conn.Close()
		grpcServer.Stop()
		lis.Close()
	}
	return v1.NewRunnerServiceClient(conn), stop
}

func spec(id string) *v1.SandboxSpec {
	return &v1.SandboxSpec{
		SandboxId:  id,
		Image:      "vora/base",
		Resources:  &v1.ResourceLimits{CpuMillis: 1000, MemoryBytes: 512 << 20, Pids: 128},
		Network:    &v1.NetworkPolicy{Mode: v1.NetworkMode_NETWORK_NONE},
		TtlSeconds: 60,
	}
}

func TestCreateGetList(t *testing.T) {
	dir := t.TempDir()
	client, stop := startServer(t, filepath.Join(dir, "state"), filepath.Join(dir, "runner.sock"))
	defer stop()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	created, err := client.CreateSandbox(ctx, &v1.CreateSandboxRequest{Spec: spec("")})
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	id := created.GetSandbox().GetId()
	if id == "" || !ulid.Valid(id) {
		t.Fatalf("expected generated ULID sandbox id, got %q", id)
	}
	if created.GetSandbox().GetState() != v1.SandboxState_REQUESTED {
		t.Fatalf("state = %v, want REQUESTED", created.GetSandbox().GetState())
	}

	got, err := client.GetSandbox(ctx, &v1.GetSandboxRequest{SandboxId: id})
	if err != nil {
		t.Fatalf("GetSandbox: %v", err)
	}
	if got.GetId() != id || got.GetSpec().GetImage() != "vora/base" {
		t.Fatalf("unexpected sandbox: %+v", got)
	}

	list, err := client.ListSandboxes(ctx, &v1.ListSandboxesRequest{})
	if err != nil {
		t.Fatalf("ListSandboxes: %v", err)
	}
	if len(list.GetSandboxes()) != 1 {
		t.Fatalf("list = %d sandboxes, want 1", len(list.GetSandboxes()))
	}

	if _, err := client.GetSandbox(ctx, &v1.GetSandboxRequest{SandboxId: "sbx_missing"}); err == nil {
		t.Fatal("GetSandbox(unknown) should fail")
	}
}

func TestRestartPreservesState(t *testing.T) {
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "state")
	socket := filepath.Join(dir, "runner.sock")

	client, stop := startServer(t, stateDir, socket)
	ctx := context.Background()
	created, err := client.CreateSandbox(ctx, &v1.CreateSandboxRequest{Spec: spec("")})
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	id := created.GetSandbox().GetId()
	stop()
	os.Remove(socket)

	client2, stop2 := startServer(t, stateDir, socket)
	defer stop2()
	got, err := client2.GetSandbox(ctx, &v1.GetSandboxRequest{SandboxId: id})
	if err != nil {
		t.Fatalf("GetSandbox after restart: %v", err)
	}
	if got.GetId() != id || got.GetState() != v1.SandboxState_REQUESTED {
		t.Fatalf("state not preserved: %+v", got)
	}
}

func TestBinaryShutsDownOnSIGTERM(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "vora-runner")

	build := exec.Command("go", "build", "-o", bin, "./cmd/vora-runner")
	build.Dir = filepath.Join("..", "..")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build runner: %v\n%s", err, out)
	}

	stateDir := filepath.Join(dir, "state")
	socket := filepath.Join(dir, "runner.sock")
	cmd := exec.Command(bin)
	cmd.Env = append(os.Environ(),
		"VORA_STATE_DIR="+stateDir,
		"VORA_SOCKET="+socket,
	)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start runner: %v", err)
	}

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(socket); err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if _, err := os.Stat(socket); err != nil {
		cmd.Process.Kill()
		t.Fatalf("runner socket did not appear: %v", err)
	}

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("signal: %v", err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("runner exited non-zero: %v", err)
	}
	if _, err := os.Stat(socket); !os.IsNotExist(err) {
		t.Fatalf("socket not removed after shutdown: %v", err)
	}
}
