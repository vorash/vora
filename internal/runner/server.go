// Package runner implements the RunnerService control API.
package runner

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	v1 "github.com/vorash/vora/api/proto/v1"
	"github.com/vorash/vora/internal/config"
	"github.com/vorash/vora/internal/images"
	"github.com/vorash/vora/internal/runtime"
	"github.com/vorash/vora/internal/sandbox"
	"github.com/vorash/vora/internal/ulid"
	"github.com/vorash/vora/internal/workspace"
)

// Server implements v1.RunnerServiceServer.
type Server struct {
	v1.UnimplementedRunnerServiceServer

	cfg        config.Config
	store      *sandbox.Store
	registry   *runtime.Registry
	images     *images.Cache
	workspaces *workspace.Manager
	runnerID   string
	logger     *slog.Logger

	handlesMu sync.Mutex
	handles   map[string]runtime.Handle
}

// NewServer constructs a runner server.
func NewServer(cfg config.Config, store *sandbox.Store, registry *runtime.Registry, runnerID string, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	return &Server{
		cfg:        cfg,
		store:      store,
		registry:   registry,
		images:     images.NewCache(cfg.ImagesDir()),
		workspaces: workspace.NewManager(cfg.SandboxesDir()),
		runnerID:   runnerID,
		logger:     logger,
		handles:    make(map[string]runtime.Handle),
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

	// Prepare the per-sandbox directories and record the rootfs path. The
	// image is not required to exist yet; the backend validates it at start.
	ws := s.workspaces.Paths(spec.ID)
	ws.RootfsDir = s.images.Rootfs(spec.Image)
	spec.Workspace = ws
	if _, err := s.workspaces.Create(ws); err != nil {
		s.logger.Error("create workspace failed", "sandbox_id", spec.ID, "error", err)
		return nil, status.Error(codes.Internal, "failed to prepare workspace")
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

// StartSandbox provisions and starts a sandbox using the default backend.
func (s *Server) StartSandbox(ctx context.Context, req *v1.StartSandboxRequest) (*v1.Sandbox, error) {
	if req.GetSandboxId() == "" {
		return nil, status.Error(codes.InvalidArgument, "sandbox_id is required")
	}
	rec, err := s.store.Load(req.GetSandboxId())
	if err != nil {
		return nil, s.loadError(err, req.GetSandboxId())
	}
	if rec.State != sandbox.StateRequested {
		return nil, status.Errorf(codes.FailedPrecondition, "sandbox %s is in state %s", rec.ID, rec.State)
	}
	backend, err := s.backend()
	if err != nil {
		return nil, err
	}
	if !backend.Capabilities().SupportsNetwork(rec.Spec.Network.Mode) {
		return nil, status.Errorf(codes.InvalidArgument, "backend %s does not support network mode %q", backend.Name(), rec.Spec.Network.Mode)
	}
	if _, err := s.images.Resolve(rec.Spec.Image); err != nil {
		s.fail(rec, runtime.ReasonRuntimeFailure)
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}

	if err := s.transition(rec, sandbox.StateScheduling, ""); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	if err := s.transition(rec, sandbox.StateCreating, ""); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	h, err := backend.Create(ctx, rec.Spec)
	if err != nil {
		s.fail(rec, runtime.ReasonRuntimeFailure)
		return nil, status.Errorf(codes.Internal, "create sandbox: %v", err)
	}
	if err := s.transition(rec, sandbox.StateStarting, ""); err != nil {
		_ = backend.Destroy(ctx, h)
		return nil, status.Error(codes.Internal, err.Error())
	}
	if err := backend.Start(ctx, h); err != nil {
		_ = backend.Destroy(ctx, h)
		s.fail(rec, runtime.ReasonRuntimeFailure)
		return nil, status.Errorf(codes.Internal, "start sandbox: %v", err)
	}
	s.putHandle(rec.ID, h)
	if err := s.transition(rec, sandbox.StateRunning, ""); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	s.logger.Info("sandbox running", "sandbox_id", rec.ID, "backend", backend.Name())
	return toProtoSandbox(rec), nil
}

// ExecSandbox runs a command in a running sandbox and streams its output.
func (s *Server) ExecSandbox(req *v1.ExecSandboxRequest, stream grpc.ServerStreamingServer[v1.ExecChunk]) error {
	backend, err := s.backend()
	if err != nil {
		return err
	}
	h, err := s.handle(req.GetSandboxId())
	if err != nil {
		return err
	}
	es, err := backend.Exec(stream.Context(), h, execRequestFromProto(req.GetExec()))
	if err != nil {
		return status.Errorf(codes.Internal, "exec: %v", err)
	}
	defer es.Close()
	for {
		chunk, err := es.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return status.Errorf(codes.Internal, "exec stream: %v", err)
		}
		if err := stream.Send(execChunkToProto(chunk)); err != nil {
			return err
		}
	}
}

// SignalSandbox forwards a signal to the sandbox.
func (s *Server) SignalSandbox(ctx context.Context, req *v1.SignalSandboxRequest) (*v1.SignalSandboxResponse, error) {
	backend, err := s.backend()
	if err != nil {
		return nil, err
	}
	h, err := s.handle(req.GetSandboxId())
	if err != nil {
		return nil, err
	}
	if err := backend.Signal(ctx, h, req.GetSignal()); err != nil {
		return nil, status.Errorf(codes.Internal, "signal: %v", err)
	}
	return &v1.SignalSandboxResponse{}, nil
}

// StatsSandbox returns resource usage for a running sandbox.
func (s *Server) StatsSandbox(ctx context.Context, req *v1.StatsSandboxRequest) (*v1.Stats, error) {
	backend, err := s.backend()
	if err != nil {
		return nil, err
	}
	h, err := s.handle(req.GetSandboxId())
	if err != nil {
		return nil, err
	}
	st, err := backend.Stats(ctx, h)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "stats: %v", err)
	}
	return &v1.Stats{
		MemoryBytes:     st.MemoryBytes,
		MemoryPeakBytes: st.MemoryPeak,
		CpuMillis:       st.CPUMillis,
		Pids:            st.Pids,
		OomKills:        st.OOMKills,
		PidsLimitHits:   st.PidsLimitHits,
	}, nil
}

// DestroySandbox stops a sandbox, releases its resources and records the
// tombstone. It is idempotent.
func (s *Server) DestroySandbox(ctx context.Context, req *v1.DestroySandboxRequest) (*v1.DestroySandboxResponse, error) {
	rec, err := s.store.Load(req.GetSandboxId())
	if err != nil {
		return nil, s.loadError(err, req.GetSandboxId())
	}
	if h, herr := s.handle(rec.ID); herr == nil {
		if backend, berr := s.backend(); berr == nil {
			if err := backend.Destroy(ctx, h); err != nil {
				s.logger.Warn("destroy backend failed", "sandbox_id", rec.ID, "error", err)
			}
		}
	}
	s.delHandle(rec.ID)
	if err := s.workspaces.Clean(rec.ID); err != nil {
		s.logger.Warn("clean workspace failed", "sandbox_id", rec.ID, "error", err)
	}
	if rec.State != sandbox.StateDestroyed {
		if err := s.transition(rec, sandbox.StateDestroyed, rec.Reason); err != nil {
			s.logger.Warn("state transition on destroy failed", "sandbox_id", rec.ID, "state", rec.State, "error", err)
		}
	}
	return &v1.DestroySandboxResponse{}, nil
}

func (s *Server) backend() (runtime.Backend, error) {
	b, ok := s.registry.Default()
	if !ok {
		return nil, status.Error(codes.Unavailable, "no runtime backend is registered")
	}
	return b, nil
}

func (s *Server) transition(rec *sandbox.Record, to sandbox.State, reason runtime.ExitReason) error {
	if rec.State == to {
		return nil
	}
	if err := sandbox.Transition(rec, to, reason); err != nil {
		return err
	}
	return s.store.Save(rec)
}

func (s *Server) fail(rec *sandbox.Record, reason runtime.ExitReason) {
	if err := s.transition(rec, sandbox.StateFailed, reason); err != nil {
		s.logger.Warn("failed to record FAILED state", "sandbox_id", rec.ID, "state", rec.State, "error", err)
	}
}

func (s *Server) loadError(err error, id string) error {
	if errors.Is(err, sandbox.ErrNotFound) {
		return status.Errorf(codes.NotFound, "sandbox %s not found", id)
	}
	return status.Error(codes.Internal, "failed to load sandbox")
}

func (s *Server) putHandle(id string, h runtime.Handle) {
	s.handlesMu.Lock()
	defer s.handlesMu.Unlock()
	s.handles[id] = h
}

func (s *Server) handle(id string) (runtime.Handle, error) {
	if id == "" {
		return nil, status.Error(codes.InvalidArgument, "sandbox_id is required")
	}
	s.handlesMu.Lock()
	defer s.handlesMu.Unlock()
	h, ok := s.handles[id]
	if !ok {
		return nil, status.Errorf(codes.FailedPrecondition, "sandbox %s is not running", id)
	}
	return h, nil
}

func (s *Server) delHandle(id string) {
	s.handlesMu.Lock()
	defer s.handlesMu.Unlock()
	delete(s.handles, id)
}
