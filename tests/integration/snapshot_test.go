//go:build integration

package integration

import (
	"context"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	v1 "github.com/vorash/vora/api/proto/v1"
	"github.com/vorash/vora/internal/cgroup"
	"github.com/vorash/vora/internal/config"
	"github.com/vorash/vora/internal/runner"
	"github.com/vorash/vora/internal/runtime"
	"github.com/vorash/vora/internal/runtime/bwrap"
	"github.com/vorash/vora/internal/sandbox"
	"github.com/vorash/vora/internal/ulid"
)

// TestSnapshotLifecycle covers the record side of snapshots without a real
// backend: capture from writable state, lookup, listing, and the loud errors.
func TestSnapshotLifecycle(t *testing.T) {
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "state")
	client, stop := startServer(t, stateDir, filepath.Join(dir, "runner.sock"))
	defer stop()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	created, err := client.CreateSandbox(ctx, &v1.CreateSandboxRequest{Spec: spec("")})
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	id := created.GetSandbox().GetId()

	// Writable state the way a workload leaves it.
	workspace := filepath.Join(stateDir, "sandboxes", id, "workspace")
	if err := os.WriteFile(filepath.Join(workspace, "hello.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}

	snap, err := client.SnapshotSandbox(ctx, &v1.SnapshotSandboxRequest{SandboxId: id})
	if err != nil {
		t.Fatalf("SnapshotSandbox: %v", err)
	}
	snapID := snap.GetSnapshot().GetId()
	if !ulid.Valid(snapID) || !strings.HasPrefix(snapID, "snap_") {
		t.Fatalf("snapshot id = %q, want a snap_ ULID", snapID)
	}
	if snap.GetSnapshot().GetSandboxId() != id {
		t.Fatalf("sandbox_id = %q, want %q", snap.GetSnapshot().GetSandboxId(), id)
	}
	if snap.GetSnapshot().GetBytes() <= 0 {
		t.Fatalf("bytes = %d, want > 0", snap.GetSnapshot().GetBytes())
	}

	got, err := client.GetSnapshot(ctx, &v1.GetSnapshotRequest{SnapshotId: snapID})
	if err != nil {
		t.Fatalf("GetSnapshot: %v", err)
	}
	if got.GetId() != snapID || got.GetRunnerId() == "" {
		t.Fatalf("GetSnapshot: %+v", got)
	}

	list, err := client.ListSnapshots(ctx, &v1.ListSnapshotsRequest{})
	if err != nil {
		t.Fatalf("ListSnapshots: %v", err)
	}
	if len(list.GetSnapshots()) != 1 || list.GetSnapshots()[0].GetId() != snapID {
		t.Fatalf("ListSnapshots: %v entries, want exactly %q", len(list.GetSnapshots()), snapID)
	}

	if _, err := client.SnapshotSandbox(ctx, &v1.SnapshotSandboxRequest{SandboxId: ulid.NewSandbox()}); status.Code(err) != codes.NotFound {
		t.Fatalf("SnapshotSandbox(unknown) code = %v, want NotFound", status.Code(err))
	}

	// A destroyed sandbox has no writable state left to capture.
	if _, err := client.DestroySandbox(ctx, &v1.DestroySandboxRequest{SandboxId: id}); err != nil {
		t.Fatalf("DestroySandbox: %v", err)
	}
	if _, err := client.SnapshotSandbox(ctx, &v1.SnapshotSandboxRequest{SandboxId: id}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("SnapshotSandbox(destroyed) code = %v, want FailedPrecondition", status.Code(err))
	}

	if _, err := client.DeleteSnapshot(ctx, &v1.DeleteSnapshotRequest{SnapshotId: snapID}); err != nil {
		t.Fatalf("DeleteSnapshot: %v", err)
	}
	if _, err := client.GetSnapshot(ctx, &v1.GetSnapshotRequest{SnapshotId: snapID}); status.Code(err) != codes.NotFound {
		t.Fatalf("GetSnapshot(deleted) code = %v, want NotFound", status.Code(err))
	}
}

// TestSnapshotFork drives the full flow on a real bwrap sandbox: write state,
// snapshot the RUNNING sandbox (the copy is live and crash-consistent), let the
// source die, and fork it back up. A fork reproduces file state as of the copy
// and is independent of its siblings from the first write.
func TestSnapshotFork(t *testing.T) {
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
	t.Cleanup(func() { grpcServer.Stop(); lis.Close() })

	conn, err := grpc.NewClient("unix://"+socket, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	client := v1.NewRunnerServiceClient(conn)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	created, err := client.CreateSandbox(ctx, &v1.CreateSandboxRequest{Spec: spec("")})
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	srcID := created.GetSandbox().GetId()
	if _, err := client.StartSandbox(ctx, &v1.StartSandboxRequest{SandboxId: srcID}); err != nil {
		t.Fatalf("StartSandbox: %v", err)
	}

	if out := execViaRunner(t, client, srcID, "sh", "-c", "echo hello fork > /workspace/hello.txt && cat /root/.nonexistent 2>/dev/null; true"); strings.TrimSpace(out) != "" {
		t.Fatalf("setup exec output = %q", out)
	}

	snap, err := client.SnapshotSandbox(ctx, &v1.SnapshotSandboxRequest{SandboxId: srcID})
	if err != nil {
		t.Fatalf("SnapshotSandbox(live): %v", err)
	}
	snapID := snap.GetSnapshot().GetId()

	// The snapshot outlives its source.
	if _, err := client.DestroySandbox(ctx, &v1.DestroySandboxRequest{SandboxId: srcID}); err != nil {
		t.Fatalf("DestroySandbox: %v", err)
	}

	fork1, err := client.ForkSnapshot(ctx, &v1.ForkSnapshotRequest{SnapshotId: snapID})
	if err != nil {
		t.Fatalf("ForkSnapshot: %v", err)
	}
	id1 := fork1.GetSandbox().GetId()
	if id1 == srcID || !ulid.Valid(id1) {
		t.Fatalf("fork id = %q, want a fresh ULID (source was %q)", id1, srcID)
	}
	if fork1.GetSandbox().GetState() != v1.SandboxState_RUNNING {
		t.Fatalf("fork state = %v, want RUNNING", fork1.GetSandbox().GetState())
	}
	if out := execViaRunner(t, client, id1, "cat", "/workspace/hello.txt"); strings.TrimSpace(out) != "hello fork" {
		t.Fatalf("fork sees %q, want %q", out, "hello fork")
	}

	// Siblings diverge from their first write.
	execViaRunner(t, client, id1, "sh", "-c", "echo only-in-fork1 > /workspace/marker.txt")

	fork2, err := client.ForkSnapshot(ctx, &v1.ForkSnapshotRequest{SnapshotId: snapID})
	if err != nil {
		t.Fatalf("ForkSnapshot(2): %v", err)
	}
	id2 := fork2.GetSandbox().GetId()
	if out := execViaRunner(t, client, id2, "cat", "/workspace/hello.txt"); strings.TrimSpace(out) != "hello fork" {
		t.Fatalf("fork2 sees %q, want %q", out, "hello fork")
	}
	if _, err := client.GetSandboxFile(ctx, &v1.GetSandboxFileRequest{SandboxId: id2, Path: "/workspace/marker.txt"}); status.Code(err) != codes.NotFound {
		t.Fatalf("fork2 sees fork1's writes: code = %v, want NotFound", status.Code(err))
	}

	// Deleting the snapshot leaves existing forks alone.
	if _, err := client.DeleteSnapshot(ctx, &v1.DeleteSnapshotRequest{SnapshotId: snapID}); err != nil {
		t.Fatalf("DeleteSnapshot: %v", err)
	}
	if out := execViaRunner(t, client, id2, "cat", "/workspace/hello.txt"); strings.TrimSpace(out) != "hello fork" {
		t.Fatalf("fork2 after snapshot delete sees %q", out)
	}

	for _, id := range []string{id1, id2} {
		if _, err := client.DestroySandbox(ctx, &v1.DestroySandboxRequest{SandboxId: id}); err != nil {
			t.Fatalf("DestroySandbox(%s): %v", id, err)
		}
	}
}
