package runner

import (
	"context"
	"errors"
	"os"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	v1 "github.com/vorash/vora/api/proto/v1"
	"github.com/vorash/vora/internal/runtime"
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
	s.snapshotMu.Lock()
	defer s.snapshotMu.Unlock()
	rec, err := s.store.Load(req.GetSandboxId())
	if err != nil {
		return nil, s.loadError(err, req.GetSandboxId())
	}
	if rec.Spec.PoolID != "" {
		return nil, status.Errorf(codes.FailedPrecondition, "sandbox %s belongs to pool %s; use the pool API", rec.ID, rec.Spec.PoolID)
	}
	existing, err := s.snapshots.List()
	if err != nil {
		return nil, status.Error(codes.Internal, "failed to check snapshot capacity")
	}
	maxTenant := s.cfg.MaxSnapshotsPerTenant
	if maxTenant <= 0 {
		maxTenant = 32
	}
	count := 0
	for _, snap := range existing {
		if snap.TenantID == rec.Spec.TenantID {
			count++
		}
	}
	if count >= int(maxTenant) {
		return nil, status.Error(codes.ResourceExhausted, "snapshot capacity reached")
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

	maxBytes := s.cfg.MaxSnapshotBytes
	if maxBytes <= 0 {
		maxBytes = 1 << 30
	}
	n, err := writeArchive(s.snapshots.DataPath(snap.ID), snapshot.Folders{
		snapshot.WorkspaceDir: ws.WorkspaceDir,
		snapshot.HomeDir:      ws.HomeDir,
	}, maxBytes)
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
	if err := s.enforceResourceLimits(&spec); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	// createMu covers the capacity check and the record that claims the slot.
	// The restore and the backend start below run outside it, so a fork does
	// not serialise every other create for the length of a start.
	spec.ID = ulid.NewSandbox()
	ws := s.workspaces.Paths(spec.ID)
	ws.RootfsDir = s.images.Rootfs(spec.Image)
	spec.Workspace = ws
	rec := &sandbox.Record{
		ID:       spec.ID,
		Spec:     spec,
		State:    sandbox.StateRequested,
		RunnerID: s.runnerID,
	}
	if err := s.forkRecord(rec); err != nil {
		return nil, err
	}
	if _, err := s.workspaces.Create(ws); err != nil {
		s.abortFork(ctx, rec.ID, req.GetActor().GetKeyId())
		s.logger.Error("fork workspace failed", "snapshot_id", snap.ID, "error", err)
		return nil, status.Error(codes.Internal, "failed to prepare workspace")
	}
	s.applyDiskQuota(ctx, rec)
	if err := restoreArchive(s.snapshots.DataPath(snap.ID), snapshot.Folders{
		snapshot.WorkspaceDir: ws.WorkspaceDir,
		snapshot.HomeDir:      ws.HomeDir,
	}, spec.Resources.DiskBytes); err != nil {
		s.abortFork(ctx, rec.ID, req.GetActor().GetKeyId())
		s.logger.Error("fork restore failed", "snapshot_id", snap.ID, "sandbox_id", spec.ID, "error", err)
		return nil, status.Errorf(codes.Internal, "failed to restore snapshot %s", snap.ID)
	}

	s.logger.Info("sandbox forked",
		"snapshot_id", snap.ID,
		"sandbox_id", rec.ID,
		"source_sandbox_id", snap.SandboxID,
		"runner_id", s.runnerID,
	)
	if len(creds) > 0 {
		s.stageBrokerCredentials(rec.ID, creds)
	}
	// The caller only learns the fork's ID from a successful response, so a fork
	// that fails to start could never be destroyed by them: release it here
	// rather than strand its restored state and capacity slot.
	started, err := s.StartSandbox(ctx, &v1.StartSandboxRequest{SandboxId: rec.ID, Actor: req.GetActor()})
	if err != nil {
		s.abortFork(ctx, rec.ID, req.GetActor().GetKeyId())
		return nil, err
	}
	return &v1.ForkSnapshotResponse{Sandbox: started}, nil
}

// forkRecord claims the fork's capacity slot: capacity check and record
// create, under createMu.
func (s *Server) forkRecord(rec *sandbox.Record) error {
	s.createMu.Lock()
	defer s.createMu.Unlock()
	if err := s.checkSandboxCapacity(rec.Spec.TenantID); err != nil {
		return err
	}
	if err := s.store.Create(rec); err != nil {
		if _, loadErr := s.store.Load(rec.ID); loadErr == nil {
			return status.Errorf(codes.AlreadyExists, "sandbox %s already exists", rec.ID)
		}
		s.logger.Error("fork persist failed", "sandbox_id", rec.ID, "error", err)
		return status.Error(codes.Internal, "failed to persist sandbox")
	}
	return nil
}

// abortFork marks a fork that could not start FAILED and destroys it, which
// releases its quota and removes its restored directories.
func (s *Server) abortFork(ctx context.Context, id, keyID string) {
	if rec, err := s.store.Load(id); err == nil && rec.State == sandbox.StateRequested {
		s.fail(rec, runtime.ReasonRuntimeFailure, keyID)
	}
	if err := s.destroy(ctx, id, keyID); err != nil {
		s.logger.Warn("abort failed fork", "sandbox_id", id, "error", err)
	}
}

func (s *Server) snapshotLoadError(err error, id string) error {
	if errors.Is(err, snapshot.ErrNotFound) {
		return status.Errorf(codes.NotFound, "snapshot %s not found", id)
	}
	if errors.Is(err, snapshot.ErrInvalidID) {
		return status.Error(codes.InvalidArgument, "invalid snapshot_id")
	}
	return status.Error(codes.Internal, "failed to load snapshot")
}
