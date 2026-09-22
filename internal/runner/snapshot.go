package runner

import (
	"context"
	"errors"
	"os"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	v1 "github.com/vorash/vora/api/proto/v1"
	"github.com/vorash/vora/internal/sandbox"
	"github.com/vorash/vora/internal/snapshot"
	"github.com/vorash/vora/internal/ulid"
)

// SnapshotSandbox captures a sandbox's writable state (workspace and home) as
// a crash-consistent archive. The sandbox keeps running while the copy happens
// (ADR 023).
func (s *Server) SnapshotSandbox(_ context.Context, req *v1.SnapshotSandboxRequest) (*v1.SnapshotSandboxResponse, error) {
	if req.GetSandboxId() == "" {
		return nil, status.Error(codes.InvalidArgument, "sandbox_id is required")
	}
	rec, err := s.store.Load(req.GetSandboxId())
	if err != nil {
		return nil, s.loadError(err, req.GetSandboxId())
	}
	ws := rec.Spec.Workspace
	if _, err := os.Stat(ws.WorkspaceDir); err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "sandbox %s has no writable state to snapshot", rec.ID)
	}

	snap := &snapshot.Record{
		ID:        ulid.NewSnapshot(),
		SandboxID: rec.ID,
		TenantID:  rec.Spec.TenantID,
		RunnerID:  s.runnerID,
		Spec:      rec.Spec,
	}
	dir := s.snapshots.Dir(snap.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		s.logger.Error("snapshot mkdir failed", "snapshot_id", snap.ID, "error", err)
		return nil, status.Error(codes.Internal, "failed to prepare snapshot directory")
	}
	keep := false
	defer func() {
		if !keep {
			_ = s.snapshots.Delete(snap.ID)
		}
	}()

	n, err := writeArchive(s.snapshots.DataPath(snap.ID), snapshot.Folders{
		snapshot.WorkspaceDir: ws.WorkspaceDir,
		snapshot.HomeDir:      ws.HomeDir,
	})
	if err != nil {
		s.logger.Error("snapshot archive failed", "snapshot_id", snap.ID, "error", err)
		return nil, status.Error(codes.Internal, "failed to archive sandbox state")
	}
	snap.Bytes = n
	if err := s.snapshots.Create(snap); err != nil {
		s.logger.Error("snapshot persist failed", "snapshot_id", snap.ID, "error", err)
		return nil, status.Error(codes.Internal, "failed to persist snapshot")
	}
	keep = true
	s.logger.Info("snapshot created",
		"snapshot_id", snap.ID,
		"sandbox_id", rec.ID,
		"runner_id", s.runnerID,
		"bytes", snap.Bytes,
	)
	return &v1.SnapshotSandboxResponse{Snapshot: toProtoSnapshot(snap)}, nil
}

// GetSnapshot returns a single snapshot.
func (s *Server) GetSnapshot(_ context.Context, req *v1.GetSnapshotRequest) (*v1.Snapshot, error) {
	if req.GetSnapshotId() == "" {
		return nil, status.Error(codes.InvalidArgument, "snapshot_id is required")
	}
	snap, err := s.snapshots.Load(req.GetSnapshotId())
	if err != nil {
		return nil, s.snapshotLoadError(err, req.GetSnapshotId())
	}
	return toProtoSnapshot(snap), nil
}

// ListSnapshots returns all snapshots held by this runner.
func (s *Server) ListSnapshots(_ context.Context, _ *v1.ListSnapshotsRequest) (*v1.ListSnapshotsResponse, error) {
	snaps, err := s.snapshots.List()
	if err != nil {
		return nil, status.Error(codes.Internal, "failed to list snapshots")
	}
	out := make([]*v1.Snapshot, 0, len(snaps))
	for _, snap := range snaps {
		out = append(out, toProtoSnapshot(snap))
	}
	return &v1.ListSnapshotsResponse{Snapshots: out}, nil
}

// DeleteSnapshot removes a snapshot and its archive. Sandboxes forked from it
// are unaffected.
func (s *Server) DeleteSnapshot(_ context.Context, req *v1.DeleteSnapshotRequest) (*v1.DeleteSnapshotResponse, error) {
	if req.GetSnapshotId() == "" {
		return nil, status.Error(codes.InvalidArgument, "snapshot_id is required")
	}
	if _, err := s.snapshots.Load(req.GetSnapshotId()); err != nil {
		return nil, s.snapshotLoadError(err, req.GetSnapshotId())
	}
	if err := s.snapshots.Delete(req.GetSnapshotId()); err != nil {
		return nil, status.Error(codes.Internal, "failed to delete snapshot")
	}
	s.logger.Info("snapshot deleted", "snapshot_id", req.GetSnapshotId(), "runner_id", s.runnerID)
	return &v1.DeleteSnapshotResponse{}, nil
}

// ForkSnapshot creates and starts a new sandbox from a snapshot. The fork
// inherits the source specification verbatim (ADR 023); its writable state is
// restored from an archive the workload authored, so Restore enforces the
// extraction rules and a failure cleans the fork's directories up.
func (s *Server) ForkSnapshot(ctx context.Context, req *v1.ForkSnapshotRequest) (*v1.ForkSnapshotResponse, error) {
	if req.GetSnapshotId() == "" {
		return nil, status.Error(codes.InvalidArgument, "snapshot_id is required")
	}
	snap, err := s.snapshots.Load(req.GetSnapshotId())
	if err != nil {
		return nil, s.snapshotLoadError(err, req.GetSnapshotId())
	}
	creds, err := brokerCredentialsFromProto(req.GetBrokerSecrets())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	spec := snap.Spec
	spec.ID = ulid.NewSandbox()
	ws := s.workspaces.Paths(spec.ID)
	ws.RootfsDir = s.images.Rootfs(spec.Image)
	spec.Workspace = ws
	if _, err := s.workspaces.Create(ws); err != nil {
		s.logger.Error("fork workspace failed", "snapshot_id", snap.ID, "error", err)
		return nil, status.Error(codes.Internal, "failed to prepare workspace")
	}

	if err := restoreArchive(s.snapshots.DataPath(snap.ID), snapshot.Folders{
		snapshot.WorkspaceDir: ws.WorkspaceDir,
		snapshot.HomeDir:      ws.HomeDir,
	}); err != nil {
		_ = s.workspaces.Destroy(spec.ID)
		s.logger.Error("fork restore failed", "snapshot_id", snap.ID, "sandbox_id", spec.ID, "error", err)
		return nil, status.Errorf(codes.Internal, "failed to restore snapshot %s", snap.ID)
	}

	rec := &sandbox.Record{
		ID:       spec.ID,
		Spec:     spec,
		State:    sandbox.StateRequested,
		RunnerID: s.runnerID,
	}
	if err := s.store.Create(rec); err != nil {
		_ = s.workspaces.Destroy(spec.ID)
		if _, loadErr := s.store.Load(spec.ID); loadErr == nil {
			return nil, status.Errorf(codes.AlreadyExists, "sandbox %s already exists", spec.ID)
		}
		s.logger.Error("fork persist failed", "snapshot_id", snap.ID, "error", err)
		return nil, status.Error(codes.Internal, "failed to persist sandbox")
	}
	s.applyDiskQuota(ctx, rec)

	s.logger.Info("sandbox forked",
		"snapshot_id", snap.ID,
		"sandbox_id", rec.ID,
		"source_sandbox_id", snap.SandboxID,
		"runner_id", s.runnerID,
	)
	if len(creds) > 0 {
		s.stageBrokerCredentials(rec.ID, creds)
	}
	// A fork that fails to start is left as a FAILED record, exactly like a
	// create whose start fails: inspectable and destroyable, never hidden.
	started, err := s.StartSandbox(ctx, &v1.StartSandboxRequest{SandboxId: rec.ID})
	if err != nil {
		return nil, err
	}
	return &v1.ForkSnapshotResponse{Sandbox: started}, nil
}

func (s *Server) snapshotLoadError(err error, id string) error {
	if errors.Is(err, snapshot.ErrNotFound) {
		return status.Errorf(codes.NotFound, "snapshot %s not found", id)
	}
	return status.Error(codes.Internal, "failed to load snapshot")
}
