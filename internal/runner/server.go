// Package runner implements the RunnerService control API.
package runner

import (
	"context"
	"errors"
	"log/slog"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	v1 "github.com/vorash/vora/api/proto/v1"
	"github.com/vorash/vora/internal/config"
	"github.com/vorash/vora/internal/runtime"
	"github.com/vorash/vora/internal/sandbox"
	"github.com/vorash/vora/internal/ulid"
)

// Server implements v1.RunnerServiceServer.
type Server struct {
	v1.UnimplementedRunnerServiceServer

	cfg      config.Config
	store    *sandbox.Store
	registry *runtime.Registry
	runnerID string
	logger   *slog.Logger
}

// NewServer constructs a runner server.
func NewServer(cfg config.Config, store *sandbox.Store, registry *runtime.Registry, runnerID string, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	return &Server{
		cfg:      cfg,
		store:    store,
		registry: registry,
		runnerID: runnerID,
		logger:   logger,
	}
}

// CreateSandbox validates and persists a new sandbox in the REQUESTED state.
func (s *Server) CreateSandbox(ctx context.Context, req *v1.CreateSandboxRequest) (*v1.CreateSandboxResponse, error) {
	spec, err := specFromProto(req.GetSpec(), s.cfg.DefaultTTL)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if spec.ID == "" {
		spec.ID = ulid.NewSandbox()
	}
	if err := spec.Validate(); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	rec := &sandbox.Record{
		ID:       spec.ID,
		Spec:     spec,
		State:    sandbox.StateRequested,
		RunnerID: s.runnerID,
	}
	if err := s.store.Create(rec); err != nil {
		if _, loadErr := s.store.Load(spec.ID); loadErr == nil {
			return nil, status.Errorf(codes.AlreadyExists, "sandbox %s already exists", spec.ID)
		}
		s.logger.Error("create sandbox failed", "sandbox_id", spec.ID, "error", err)
		return nil, status.Error(codes.Internal, "failed to persist sandbox")
	}

	s.logger.Info("sandbox requested",
		"sandbox_id", rec.ID,
		"image", spec.Image,
		"runner_id", s.runnerID,
		"memory_bytes", spec.Resources.MemoryBytes,
		"cpu_millis", spec.Resources.CPUMillis,
		"pids", spec.Resources.Pids,
		"network", string(spec.Network.Mode),
	)
	return &v1.CreateSandboxResponse{Sandbox: toProtoSandbox(rec)}, nil
}

// GetSandbox returns a single sandbox.
func (s *Server) GetSandbox(ctx context.Context, req *v1.GetSandboxRequest) (*v1.Sandbox, error) {
	if req.GetSandboxId() == "" {
		return nil, status.Error(codes.InvalidArgument, "sandbox_id is required")
	}
	rec, err := s.store.Load(req.GetSandboxId())
	if err != nil {
		if errors.Is(err, sandbox.ErrNotFound) {
			return nil, status.Errorf(codes.NotFound, "sandbox %s not found", req.GetSandboxId())
		}
		return nil, status.Error(codes.Internal, "failed to load sandbox")
	}
	return toProtoSandbox(rec), nil
}

// ListSandboxes returns sandboxes, optionally filtered by state.
func (s *Server) ListSandboxes(ctx context.Context, req *v1.ListSandboxesRequest) (*v1.ListSandboxesResponse, error) {
	recs, err := s.store.List()
	if err != nil {
		return nil, status.Error(codes.Internal, "failed to list sandboxes")
	}
	filter := stateFromProto(req.GetState())
	out := make([]*v1.Sandbox, 0, len(recs))
	for _, rec := range recs {
		if rec.State == sandbox.StateDestroyed && !req.GetIncludeDestroyed() {
			continue
		}
		if filter != "" && rec.State != filter {
			continue
		}
		out = append(out, toProtoSandbox(rec))
	}
	return &v1.ListSandboxesResponse{Sandboxes: out}, nil
}
