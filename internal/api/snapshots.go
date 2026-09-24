package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	v1 "github.com/vorash/vora/api/proto/v1"
)

// errSnapshotAccess mirrors errTenantAccess for snapshot routes, so a
// cross-tenant probe reads as "not found" for both resource kinds.
var errSnapshotAccess = errors.New("api: snapshot not found")

// authorizedSnapshotRoute returns the caller's own snapshot route; a foreign
// tenant's snapshot is indistinguishable from one that does not exist.
func (s *Server) authorizedSnapshotRoute(ctx context.Context, snapshotID string) (SnapshotRoute, error) {
	route, err := s.store.SnapshotRoute(ctx, snapshotID)
	key := principalFrom(ctx).key
	if err != nil || !canAccessTenant(key, route.TenantID) || !canAccessAgent(key, route.AgentID) {
		return SnapshotRoute{}, errSnapshotAccess
	}
	return route, nil
}

// snapshotSandbox captures a running sandbox's writable state (ADR 023).
func (s *Server) snapshotSandbox(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	route, err := s.authorizedRoute(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	rc, err := s.runnerByID(r.Context(), route.RunnerID)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	defer rc.Close()

	snap, err := rc.SnapshotSandbox(r.Context(), id)
	if err != nil {
		writeGRPCError(w, err)
		return
	}
	sroute := SnapshotRoute{
		SnapshotID: snap.GetId(),
		RunnerID:   snap.GetRunnerId(),
		TenantID:   route.TenantID,
		AgentID:    route.AgentID,
		SandboxID:  snap.GetSandboxId(),
	}
	if err := s.store.SetSnapshotRoute(r.Context(), sroute); err != nil {
		// Without its route the snapshot is unreachable: remove it rather than
		// leaving state no caller can ever see.
		s.cleanupRunnerSnapshot(rc, snap.GetId(), "route persistence failed")
		writeError(w, http.StatusInternalServerError, "failed to persist snapshot route")
		return
	}
	s.logger.Info("snapshot placed", "snapshot_id", snap.GetId(), "sandbox_id", id, "runner_id", route.RunnerID)
	writeProto(w, http.StatusCreated, snap)
}

// getSnapshot returns a caller-owned snapshot.
func (s *Server) getSnapshot(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	route, err := s.authorizedSnapshotRoute(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	rc, err := s.runnerByID(r.Context(), route.RunnerID)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	defer rc.Close()

	snap, err := rc.GetSnapshot(r.Context(), id)
	if err != nil {
		writeGRPCError(w, err)
		return
	}
	writeProto(w, http.StatusOK, snap)
}

// listSnapshots aggregates snapshots across runners, tenant-scoped like
// listSandboxes.
func (s *Server) listSnapshots(w http.ResponseWriter, r *http.Request) {
	all := &v1.ListSnapshotsResponse{}
	for _, runner := range s.runners(r.Context()) {
		rc, err := s.dialRunner(runner)
		if err != nil {
			continue
		}
		items, err := rc.ListSnapshots(r.Context())
		rc.Close()
		if err != nil {
			continue
		}
		key := principalFrom(r.Context()).key
		for _, snap := range items {
			if canAccessTenant(key, snap.GetTenantId()) && canAccessAgent(key, snap.GetSpec().GetAgentId()) {
				all.Snapshots = append(all.Snapshots, snap)
			}
		}
	}
	writeProto(w, http.StatusOK, all)
}

// deleteSnapshot removes a snapshot; sandboxes forked from it are unaffected.
func (s *Server) deleteSnapshot(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	route, err := s.authorizedSnapshotRoute(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	rc, err := s.runnerByID(r.Context(), route.RunnerID)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	defer rc.Close()

	if err := rc.DeleteSnapshot(r.Context(), id); err != nil {
		writeGRPCError(w, err)
		return
	}
	if err := s.store.DeleteSnapshotRoute(r.Context(), id); err != nil {
		// The data is gone; say so loudly rather than pretending full success.
		writeError(w, http.StatusInternalServerError, "snapshot deleted but route cleanup failed")
		return
	}
	s.logger.Info("snapshot deleted", "snapshot_id", id, "runner_id", route.RunnerID)
	writeProto(w, http.StatusOK, &v1.DeleteSnapshotResponse{})
}

// forkSnapshot creates and starts a sandbox from a caller-owned snapshot on the
// runner that holds the data (ADR 023).
func (s *Server) forkSnapshot(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	route, err := s.authorizedSnapshotRoute(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	rc, err := s.runnerByID(r.Context(), route.RunnerID)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	defer rc.Close()

	snap, err := rc.GetSnapshot(r.Context(), id)
	if err != nil {
		writeGRPCError(w, err)
		return
	}
	// The fork inherits the snapshot's spec, so its broker aliases need their
	// credentials resolved exactly like a create.
	runner, err := s.store.Runner(r.Context(), route.RunnerID)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "snapshot runner unavailable")
		return
	}
	creds, err := s.resolveBrokerSecrets(r.Context(), snap.GetSpec(), runner)
	if err != nil {
		if errors.Is(err, errSecretsNotConfigured) {
			writeError(w, http.StatusServiceUnavailable, err.Error())
			return
		}
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	defer zeroBrokerSecrets(creds)

	sb, err := rc.ForkSnapshotWithCredentials(r.Context(), id, creds, actorFromKey(principalFrom(r.Context()).key))
	if err != nil {
		writeGRPCError(w, err)
		return
	}
	froute := SandboxRoute{
		SandboxID:   sb.GetId(),
		RunnerID:    route.RunnerID,
		TenantID:    route.TenantID,
		AgentID:     route.AgentID,
		CPUMillis:   sb.GetSpec().GetResources().GetCpuMillis(),
		MemoryBytes: sb.GetSpec().GetResources().GetMemoryBytes(),
	}
	if err := s.store.SetRoute(r.Context(), froute); err != nil {
		s.cleanupRunnerSandbox(rc, sb.GetId(), "route persistence failed", actorFromKey(principalFrom(r.Context()).key))
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		if deleteErr := s.store.DeleteRoute(cleanupCtx, sb.GetId()); deleteErr != nil {
			s.logger.Error("remove partial route failed", "sandbox_id", sb.GetId(), "error", deleteErr)
		}
		cancel()
		writeError(w, http.StatusInternalServerError, "failed to persist sandbox route")
		return
	}
	s.logger.Info("sandbox forked", "snapshot_id", id, "sandbox_id", sb.GetId(), "runner_id", route.RunnerID)
	writeProto(w, http.StatusCreated, sb)
}

func (s *Server) cleanupRunnerSnapshot(rc RunnerClient, snapshotID, cause string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := rc.DeleteSnapshot(ctx, snapshotID); err != nil {
		s.logger.Error("compensating snapshot cleanup failed", "snapshot_id", snapshotID, "cause", cause, "error", err)
	}
}
