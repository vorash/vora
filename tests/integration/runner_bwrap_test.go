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
	"strings"
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

// TestRunnerSandboxLifecycle drives the full runner path: CreateSandbox,
// StartSandbox, repeated ExecSandbox, StatsSandbox and DestroySandbox.
func TestRunnerSandboxLifecycle(t *testing.T) {
	initBin := buildBinary(t, "vora-init")
	t.Setenv("VORA_INIT_BINARY", initBin)

	cgroups, err := cgroup.New(config.Config{SystemdMode: config.SystemdUser})
	if err != nil {
		t.Skipf("systemd user manager unavailable: %v", err)
	}
	rootfs := imageRootfs(t, "base")
	realImages := filepath.Dir(filepath.Dir(filepath.Dir(rootfs))) // .../images

	stateDir := stateTempDir(t)
	if err := os.Symlink(realImages, filepath.Join(stateDir, "images")); err != nil {
		t.Fatalf("symlink images: %v", err)
	}
	cfg := config.Config{
		StateDir:    stateDir,
		SystemdMode: config.SystemdUser,
		DefaultTTL:  time.Minute,
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	backend, err := bwrap.New(cfg, cgroups, logger)
	if err != nil {
		t.Fatalf("bwrap backend: %v", err)
	}
	registry := runtime.NewRegistry()
	if err := registry.Register(backend); err != nil {
		t.Fatalf("register: %v", err)
	}
	store := sandbox.NewStore(cfg.SandboxesDir())
	srv := runner.NewServer(cfg, store, registry, ulid.NewRunner(), logger)

	socket := filepath.Join(t.TempDir(), "runner.sock")
	lis, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	grpcServer := grpc.NewServer()
	v1.RegisterRunnerServiceServer(grpcServer, srv)
	go func() { _ = grpcServer.Serve(lis) }()
	t.Cleanup(func() {
		grpcServer.Stop()
		lis.Close()
	})

	conn, err := grpc.NewClient("unix://"+socket, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	client := v1.NewRunnerServiceClient(conn)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	created, err := client.CreateSandbox(ctx, &v1.CreateSandboxRequest{Spec: &v1.SandboxSpec{
		Image:      "vora/base",
		Resources:  &v1.ResourceLimits{CpuMillis: 1000, MemoryBytes: 256 << 20, Pids: 64},
		Network:    &v1.NetworkPolicy{Mode: v1.NetworkMode_NETWORK_NONE},
		TtlSeconds: 60,
	}})
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	id := created.GetSandbox().GetId()

	started, err := client.StartSandbox(ctx, &v1.StartSandboxRequest{SandboxId: id})
	if err != nil {
		t.Fatalf("StartSandbox: %v", err)
	}
	if started.GetState() != v1.SandboxState_RUNNING {
		t.Fatalf("state = %v, want RUNNING", started.GetState())
	}
	t.Cleanup(func() {
		_, _ = client.DestroySandbox(context.Background(), &v1.DestroySandboxRequest{SandboxId: id})
	})

	// First exec writes a file; second exec must see it.
	execViaRunner(t, client, id, "/bin/sh", "-c", "echo persisted > /workspace/state.txt")
	out := execViaRunner(t, client, id, "/bin/sh", "-c", "cat /workspace/state.txt")
	if strings.TrimSpace(out) != "persisted" {
		t.Fatalf("second exec output = %q, want persisted", out)
	}

	st, err := client.StatsSandbox(ctx, &v1.StatsSandboxRequest{SandboxId: id})
	if err != nil {
		t.Fatalf("StatsSandbox: %v", err)
	}
	if st.GetMemoryBytes() <= 0 {
		t.Fatalf("expected positive memory, got %+v", st)
	}

	// Execution audit records are queryable through the API.
	execs, err := client.GetSandboxExecs(ctx, &v1.GetSandboxExecsRequest{SandboxId: id})
	if err != nil {
		t.Fatalf("GetSandboxExecs: %v", err)
	}
	if len(execs.GetExecs()) < 2 {
		t.Fatalf("expected at least 2 exec records, got %d", len(execs.GetExecs()))
	}

	if _, err := client.DestroySandbox(ctx, &v1.DestroySandboxRequest{SandboxId: id}); err != nil {
		t.Fatalf("DestroySandbox: %v", err)
	}
	got, err := client.GetSandbox(ctx, &v1.GetSandboxRequest{SandboxId: id})
	if err != nil {
		t.Fatalf("GetSandbox after destroy: %v", err)
	}
	if got.GetState() != v1.SandboxState_DESTROYED {
		t.Fatalf("state after destroy = %v, want DESTROYED", got.GetState())
	}
}

// TestRunnerEnforcesDiskQuota starts a sandbox with a small disk limit,
// writes past it, and asserts the enforcer stops the sandbox with a semantic
// reason and removes the host-side workspace.
func TestRunnerEnforcesDiskQuota(t *testing.T) {
	initBin := buildBinary(t, "vora-init")
	t.Setenv("VORA_INIT_BINARY", initBin)

	cgroups, err := cgroup.New(config.Config{SystemdMode: config.SystemdUser})
	if err != nil {
		t.Skipf("systemd user manager unavailable: %v", err)
	}
	rootfs := imageRootfs(t, "base")
	realImages := filepath.Dir(filepath.Dir(filepath.Dir(rootfs)))

	stateDir := stateTempDir(t)
	if err := os.Symlink(realImages, filepath.Join(stateDir, "images")); err != nil {
		t.Fatalf("symlink images: %v", err)
	}
	cfg := config.Config{StateDir: stateDir, SystemdMode: config.SystemdUser, DefaultTTL: time.Minute}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	backend, err := bwrap.New(cfg, cgroups, logger)
	if err != nil {
		t.Fatalf("bwrap backend: %v", err)
	}
	registry := runtime.NewRegistry()
	if err := registry.Register(backend); err != nil {
		t.Fatalf("register: %v", err)
	}
	store := sandbox.NewStore(cfg.SandboxesDir())
	srv := runner.NewServer(cfg, store, registry, ulid.NewRunner(), logger)

	socket := filepath.Join(t.TempDir(), "runner.sock")
	lis, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	grpcServer := grpc.NewServer()
	v1.RegisterRunnerServiceServer(grpcServer, srv)
	go func() { _ = grpcServer.Serve(lis) }()
	t.Cleanup(func() {
		grpcServer.Stop()
		lis.Close()
	})

	conn, err := grpc.NewClient("unix://"+socket, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	client := v1.NewRunnerServiceClient(conn)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	const limit = int64(1 << 20)
	created, err := client.CreateSandbox(ctx, &v1.CreateSandboxRequest{Spec: &v1.SandboxSpec{
		Image:      "vora/base",
		Resources:  &v1.ResourceLimits{CpuMillis: 1000, MemoryBytes: 256 << 20, Pids: 64, DiskBytes: limit},
		Network:    &v1.NetworkPolicy{Mode: v1.NetworkMode_NETWORK_NONE},
		TtlSeconds: 60,
	}})
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	id := created.GetSandbox().GetId()
	if _, err := client.StartSandbox(ctx, &v1.StartSandboxRequest{SandboxId: id}); err != nil {
		t.Fatalf("StartSandbox: %v", err)
	}
	t.Cleanup(func() {
		_, _ = client.DestroySandbox(context.Background(), &v1.DestroySandboxRequest{SandboxId: id})
	})

	// Write past the limit; the command may see a write error or not, but the
	// enforcer must notice the usage and stop the sandbox.
	execViaRunner(t, client, id, "/bin/sh", "-c", "dd if=/dev/zero of=/workspace/fill bs=1M count=8 2>/dev/null; true")

	if err := srv.EnforceDiskQuota(ctx); err != nil {
		t.Fatalf("EnforceDiskQuota: %v", err)
	}

	rec, err := store.Load(id)
	if err != nil {
		t.Fatalf("load record: %v", err)
	}
	if rec.State != sandbox.StateDestroyed {
		t.Fatalf("state = %s, want DESTROYED", rec.State)
	}
	if rec.Reason != runtime.ReasonDiskQuota {
		t.Fatalf("reason = %s, want %s", rec.Reason, runtime.ReasonDiskQuota)
	}
	if _, err := os.Stat(rec.Spec.Workspace.WorkspaceDir); !os.IsNotExist(err) {
		t.Fatalf("workspace not removed after quota stop: %v", err)
	}
}

func execViaRunner(t *testing.T, client v1.RunnerServiceClient, id, command string, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	stream, err := client.ExecSandbox(ctx, &v1.ExecSandboxRequest{
		SandboxId: id,
		Exec:      &v1.ExecRequest{Command: command, Args: args, Cwd: "/workspace"},
	})
	if err != nil {
		t.Fatalf("ExecSandbox: %v", err)
	}
	var out strings.Builder
	for {
		chunk, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return out.String()
		}
		if err != nil {
			t.Fatalf("exec recv: %v", err)
		}
		out.Write(chunk.GetStderr())
		out.Write(chunk.GetStdout())
	}
}
