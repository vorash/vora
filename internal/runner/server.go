// Package runner implements the RunnerService control API.
package runner

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	v1 "github.com/vorash/vora/api/proto/v1"
	"github.com/vorash/vora/internal/audit"
	"github.com/vorash/vora/internal/audit/jsonl"
	"github.com/vorash/vora/internal/cgroup"
	"github.com/vorash/vora/internal/config"
	"github.com/vorash/vora/internal/egress"
	"github.com/vorash/vora/internal/identity"
	"github.com/vorash/vora/internal/images"
	"github.com/vorash/vora/internal/quota"
	"github.com/vorash/vora/internal/runtime"
	"github.com/vorash/vora/internal/sandbox"
	"github.com/vorash/vora/internal/snapshot"
	"github.com/vorash/vora/internal/telemetry"
	"github.com/vorash/vora/internal/ulid"
	"github.com/vorash/vora/internal/workspace"
)

// Server implements v1.RunnerServiceServer.
type Server struct {
	v1.UnimplementedRunnerServiceServer

	cfg        config.Config
	store      *sandbox.Store
	registry   *runtime.Registry
	images     *images.Resolver
	workspaces *workspace.Manager
	snapshots  *snapshot.Store
	quota      *quota.Manager
	audit      audit.Store
	runnerID   string
	logger     *slog.Logger
	metrics    *telemetry.Metrics
	cgroups    cgroup.Manager

	handlesMu sync.Mutex
	handles   map[string]sandboxHandle

	// brokerMu guards credentials resolved by the control plane for a sandbox
	// that has been created but not yet started. They are transient by design:
	// never persisted with the record, never logged, dropped once the sandbox
	// starts (or is destroyed).
	brokerMu    sync.Mutex
	brokerCreds map[string][]egress.Credential
}

// sandboxHandle binds a sandbox to the backend that manages it, so later RPCs
// (exec/stats/signal/destroy) use the right backend.
type sandboxHandle struct {
	backend runtime.Backend
	handle  runtime.Handle
}

// NewServer constructs a runner server.
func NewServer(cfg config.Config, store *sandbox.Store, registry *runtime.Registry, runnerID string, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	workspaces := workspace.NewManager(cfg.SandboxesDir())
	s := &Server{
		cfg:         cfg,
		store:       store,
		registry:    registry,
		images:      images.NewResolver(cfg, logger),
		workspaces:  workspaces,
		snapshots:   snapshot.NewStore(cfg.SnapshotsDir()),
		quota:       quota.New(cfg.SandboxesDir()),
		audit:       jsonl.New(workspaces.LogsDir),
		runnerID:    runnerID,
		logger:      logger,
		metrics:     telemetry.New(),
		handles:     make(map[string]sandboxHandle),
		brokerCreds: make(map[string][]egress.Credential),
	}
	s.setQuotaModeMetric()
	return s
}

// SetAudit replaces the audit store (e.g. with the PostgreSQL/Supabase store).
func (s *Server) SetAudit(a audit.Store) {
	if a != nil {
		s.audit = a
	}
}

// SetMetrics replaces the default metric set (used by the daemon).
func (s *Server) SetMetrics(m *telemetry.Metrics) {
	if m != nil {
		s.metrics = m
		s.setQuotaModeMetric()
	}
}

// setQuotaModeMetric records whether this host enforces disk limits.
func (s *Server) setQuotaModeMetric() {
	if s.metrics == nil || s.quota == nil {
		return
	}
	if s.quota.Mode() == quota.ModeProject {
		s.metrics.DiskQuotaEnforced.Set(1)
		return
	}
	s.metrics.DiskQuotaEnforced.Set(0)
}

// RecordEgress persists and meters one restricted-network policy decision.
// Returning an error makes allowed traffic fail closed in the proxy.
func (s *Server) RecordEgress(event egress.Event) error {
	decision, reason := "allowed", "allowed"
	if !event.Allowed {
		decision, reason = "denied", event.Reason
		s.metrics.NetworkDeniedTotal.Inc()
		s.metrics.PolicyDeniedTotal.Inc()
	}
	s.metrics.NetworkDecisionsTotal.WithLabelValues(decision, reason).Inc()
	record := sandbox.EgressRecord{
		SandboxID: event.SandboxID,
		Actor:     s.recordActor(event.SandboxID, ""),
		Host:      event.Host,
		Remote:    event.Remote,
		Alias:     event.Alias,
		Allowed:   event.Allowed,
		Reason:    event.Reason,
		Timestamp: event.Time.UTC(),
	}
	if err := s.audit.AppendEgress(context.Background(), event.SandboxID, record); err != nil {
		s.metrics.AuditWriteFailedTotal.Inc()
		s.logger.Error("write egress audit failed", "sandbox_id", event.SandboxID, "host", event.Host, "alias", event.Alias, "allowed", event.Allowed, "error", err)
		return err
	}
	if !event.Allowed {
		s.logger.Info("egress denied", "sandbox_id", event.SandboxID, "host", event.Host, "alias", event.Alias, "reason", event.Reason)
	}
	return nil
}

// SetCgroups provides the cgroup manager, enabling reconciliation of orphaned
// systemd units on startup. It is optional.
func (s *Server) SetCgroups(m cgroup.Manager) { s.cgroups = m }

// RunningCount returns the number of sandboxes this runner currently tracks.
func (s *Server) RunningCount() int {
	s.handlesMu.Lock()
	defer s.handlesMu.Unlock()
	return len(s.handles)
}

// CreateSandbox validates and persists a new sandbox in the REQUESTED state.
func (s *Server) CreateSandbox(ctx context.Context, req *v1.CreateSandboxRequest) (resp *v1.CreateSandboxResponse, err error) {
	metricStatus := "requested"
	defer func() {
		if err != nil {
			metricStatus = "failed"
		}
		s.metrics.SandboxCreateTotal.WithLabelValues(metricStatus).Inc()
	}()
	spec, err := specFromProto(req.GetSpec(), s.cfg.DefaultTTL)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if spec.ID != "" {
		return nil, status.Error(codes.InvalidArgument, "sandbox_id is assigned by the runner")
	}
	spec.ID = ulid.NewSandbox()
	if err := spec.Validate(); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	creds, err := brokerCredentialsFromProto(req.GetBrokerSecrets())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if err := validateBrokerGrants(spec, creds); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	// Prepare the per-sandbox directories and record the rootfs path. The
	// image is not required to exist yet; the backend validates it at start.
	ws := s.workspaces.Paths(spec.ID)
	ws.RootfsDir = s.images.Rootfs(spec.Image)
	spec.Workspace = ws
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
	if _, err := s.workspaces.Create(ws); err != nil {
		_ = s.workspaces.Destroy(spec.ID)
		s.logger.Error("create workspace failed", "sandbox_id", spec.ID, "error", err)
		return nil, status.Error(codes.Internal, "failed to prepare workspace")
	}
	s.applyDiskQuota(ctx, rec)

	s.logger.Info("sandbox requested",
		"sandbox_id", rec.ID,
		"image", spec.Image,
		"runner_id", s.runnerID,
		"memory_bytes", spec.Resources.MemoryBytes,
		"cpu_millis", spec.Resources.CPUMillis,
		"pids", spec.Resources.Pids,
		"network", string(spec.Network.Mode),
		"broker_aliases", spec.Network.BrokerAliases,
	)
	if len(creds) > 0 {
		s.stageBrokerCredentials(rec.ID, creds)
	}
	return &v1.CreateSandboxResponse{Sandbox: toProtoSandbox(rec)}, nil
}

// GetSandbox returns a single sandbox.
func (s *Server) GetSandbox(ctx context.Context, req *v1.GetSandboxRequest) (*v1.Sandbox, error) {
	if req.GetSandboxId() == "" {
		return nil, status.Error(codes.InvalidArgument, "sandbox_id is required")
	}
	rec, err := s.store.Load(req.GetSandboxId())
	if err != nil {
		return nil, s.loadError(err, req.GetSandboxId())
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
	start := time.Now()
	defer func() { s.metrics.SandboxCreateDuration.Observe(time.Since(start).Seconds()) }()
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
	keyID := req.GetActor().GetKeyId()
	backend, err := s.selectBackend(rec.Spec)
	if err != nil {
		return nil, err
	}
	// A sandbox that grants broker aliases must have credentials; losing them
	// (for example across a runner restart) fails closed rather than starting
	// a sandbox whose granted aliases cannot work.
	creds := s.takeBrokerCredentials(rec.ID)
	defer zeroCredentials(creds)
	if len(rec.Spec.Network.BrokerAliases) > 0 && len(creds) == 0 {
		return nil, status.Error(codes.FailedPrecondition, "broker credentials are unavailable; recreate the sandbox")
	}
	// Record the chosen backend so future RPCs and `inspect` are explicit.
	if rec.Spec.Backend == "" {
		rec.Spec.Backend = backend.Name()
	}
	if !backend.Capabilities().SupportsNetwork(rec.Spec.Network.Mode) {
		return nil, status.Errorf(codes.InvalidArgument, "backend %s does not support network mode %q", backend.Name(), rec.Spec.Network.Mode)
	}
	rootfs, err := s.images.Resolve(ctx, rec.Spec.Image)
	if err != nil {
		s.fail(rec, runtime.ReasonRuntimeFailure, keyID)
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}
	if rootfs != rec.Spec.Workspace.RootfsDir {
		rec.Spec.Workspace.RootfsDir = rootfs
		if err := s.store.Save(rec); err != nil {
			s.fail(rec, runtime.ReasonRuntimeFailure, keyID)
			return nil, status.Error(codes.Internal, "failed to persist resolved rootfs")
		}
	}

	if err := s.transition(rec, sandbox.StateScheduling, "", keyID); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	if err := s.transition(rec, sandbox.StateCreating, "", keyID); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	ctx, span := telemetry.Start(ctx, "runtime.start",
		attribute.String("sandbox.id", rec.ID),
		attribute.String("vora.image", rec.Spec.Image),
		attribute.String("vora.backend", backend.Name()))
	defer span.End()
	h, err := backend.Create(ctx, rec.Spec)
	if err != nil {
		s.fail(rec, runtime.ReasonRuntimeFailure, keyID)
		return nil, status.Errorf(codes.Internal, "create sandbox: %v", err)
	}
	if err := s.transition(rec, sandbox.StateStarting, "", keyID); err != nil {
		_ = backend.Destroy(ctx, h)
		return nil, status.Error(codes.Internal, err.Error())
	}
	var startErr error
	if len(creds) > 0 {
		ba, ok := backend.(runtime.BrokerAware)
		if !ok {
			_ = backend.Destroy(ctx, h)
			s.fail(rec, runtime.ReasonRuntimeFailure, keyID)
			return nil, status.Errorf(codes.InvalidArgument, "backend %s does not support credential brokering", backend.Name())
		}
		startErr = ba.StartWithCredentials(ctx, h, creds)
	} else {
		startErr = backend.Start(ctx, h)
	}
	if startErr != nil {
		_ = backend.Destroy(ctx, h)
		s.fail(rec, runtime.ReasonRuntimeFailure, keyID)
		return nil, status.Errorf(codes.Internal, "start sandbox: %v", startErr)
	}
	s.putHandle(rec.ID, backend, h)
	if err := s.transition(rec, sandbox.StateRunning, "", keyID); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	s.metrics.SandboxRunning.Inc()
	s.metrics.RunnerSandboxes.Inc()
	s.logger.Info("sandbox running", "sandbox_id", rec.ID, "backend", backend.Name())
	return toProtoSandbox(rec), nil
}

// ExecSandbox runs a command in a running sandbox and streams its output.
func (s *Server) ExecSandbox(req *v1.ExecSandboxRequest, stream grpc.ServerStreamingServer[v1.ExecChunk]) error {
	start := time.Now()
	defer func() { s.metrics.ExecDuration.Observe(time.Since(start).Seconds()) }()
	backend, h, err := s.handle(req.GetSandboxId())
	if err != nil {
		return err
	}
	execReq := execRequestFromProto(req.GetExec())
	if execReq.ExecutionID == "" {
		execReq.ExecutionID = ulid.NewExec()
	}

	started := time.Now().UTC()
	execCtx, span := telemetry.Start(stream.Context(), "runtime.exec",
		attribute.String("sandbox.id", req.GetSandboxId()),
		attribute.String("vora.exec.command", execReq.Command))
	defer span.End()
	baseline := s.usageBaseline(execCtx, backend, h)
	es, err := backend.Exec(execCtx, h, execReq)
	if err != nil {
		s.recordExec(req.GetSandboxId(), execReq, started, 0, 0, 0, runtime.ReasonRuntimeFailure, 0, 0, req.GetActor().GetKeyId())
		s.metrics.ExecFailedTotal.WithLabelValues(string(runtime.ReasonRuntimeFailure)).Inc()
		return status.Errorf(codes.Internal, "exec: %v", err)
	}
	defer es.Close()

	var stdoutBytes, stderrBytes int64
	var recorded bool
	for {
		chunk, err := es.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			if !recorded {
				cpu, peak := s.usageDelta(stream.Context(), backend, h, baseline)
				s.recordExec(req.GetSandboxId(), execReq, started, stdoutBytes, stderrBytes, 0, runtime.ReasonRuntimeFailure, cpu, peak, req.GetActor().GetKeyId())
				s.metrics.ExecFailedTotal.WithLabelValues(string(runtime.ReasonRuntimeFailure)).Inc()
			}
			return status.Errorf(codes.Internal, "exec stream: %v", err)
		}
		stdoutBytes += int64(len(chunk.Stdout))
		stderrBytes += int64(len(chunk.Stderr))
		if chunk.Exit != nil && !recorded {
			// Persist before observing: the client may destroy the sandbox the
			// moment it sees the exit chunk (the API closes its response right
			// after forwarding it), so the record must be on disk first. The exit
			// chunk is final, so the byte totals are complete here.
			cpu, peak := s.usageDelta(stream.Context(), backend, h, baseline)
			s.recordExec(req.GetSandboxId(), execReq, started, stdoutBytes, stderrBytes, chunk.Exit.ExitCode, chunk.Exit.Reason, cpu, peak, req.GetActor().GetKeyId())
			s.metrics.ExecTotal.WithLabelValues("ok").Inc()
			recorded = true
		}
		if err := stream.Send(execChunkToProto(chunk)); err != nil {
			return err
		}
	}

	if recorded {
		return nil
	}
	// EOF with no exit record is a killed run (watchdog, timeout, agent
	// death) — never a silent success.
	cpu, peak := s.usageDelta(stream.Context(), backend, h, baseline)
	s.recordExec(req.GetSandboxId(), execReq, started, stdoutBytes, stderrBytes, 0, runtime.ReasonRuntimeFailure, cpu, peak, req.GetActor().GetKeyId())
	s.metrics.ExecFailedTotal.WithLabelValues(string(runtime.ReasonRuntimeFailure)).Inc()
	return status.Error(codes.Internal, "exec stream ended without an exit record (the run was killed)")
}

// recordExec persists an execution audit record. Failures are logged, not
// fatal: losing an audit write must not fail the caller's command.
func (s *Server) recordExec(sandboxID string, req runtime.ExecRequest, started time.Time, stdoutBytes, stderrBytes int64, code int, reason runtime.ExitReason, cpuMillis, memPeak int64, keyID string) {
	finished := time.Now().UTC()
	allowed, denied := s.egressInWindow(sandboxID, started, finished)
	rec := sandbox.ExecRecord{
		ExecutionID:     req.ExecutionID,
		SandboxID:       sandboxID,
		Actor:           s.recordActor(sandboxID, keyID),
		Command:         req.Command,
		Args:            req.Args,
		Cwd:             req.Cwd,
		StartedAt:       started,
		FinishedAt:      finished,
		ExitCode:        code,
		Reason:          reason,
		StdoutBytes:     stdoutBytes,
		StderrBytes:     stderrBytes,
		CPUMillis:       cpuMillis,
		MemoryPeakBytes: memPeak,
		EgressAllowed:   allowed,
		EgressDenied:    denied,
	}
	if err := s.audit.AppendExec(context.Background(), sandboxID, rec); err != nil {
		s.logger.Warn("write exec record failed", "sandbox_id", sandboxID, "execution_id", req.ExecutionID, "error", err)
	}
}

// usageBaseline captures the sandbox cgroup's CPU counter before an execution,
// so the execution's own consumption can be derived as a delta.
type usageBaseline struct {
	cpuMillis int64
	ok        bool
}

func (s *Server) usageBaseline(ctx context.Context, backend runtime.Backend, h runtime.Handle) usageBaseline {
	st, err := backend.Stats(ctx, h)
	if err != nil || st == nil {
		return usageBaseline{}
	}
	return usageBaseline{cpuMillis: st.CPUMillis, ok: true}
}

// usageDelta returns the CPU consumed since the baseline and the cgroup's
// memory.peak at the end of the execution. memory.peak is a high-water mark, so
// this is the sandbox's peak, not a per-execution sample; current memory is
// used when the kernel does not expose memory.peak.
func (s *Server) usageDelta(ctx context.Context, backend runtime.Backend, h runtime.Handle, base usageBaseline) (int64, int64) {
	if !base.ok {
		return 0, 0
	}
	st, err := backend.Stats(ctx, h)
	if err != nil || st == nil {
		return 0, 0
	}
	cpu := st.CPUMillis - base.cpuMillis
	if cpu < 0 {
		cpu = 0
	}
	peak := st.MemoryPeak
	if peak <= 0 {
		peak = st.MemoryBytes
	}
	return cpu, peak
}

// egressInWindow counts restricted-network decisions inside [start, end], so an
// execution's outbound requests and policy violations are recorded with it.
func (s *Server) egressInWindow(sandboxID string, start, end time.Time) (allowed, denied int64) {
	records, err := s.audit.Egress(context.Background(), sandboxID)
	if err != nil {
		return 0, 0
	}
	for _, r := range records {
		ts := r.Timestamp.UTC()
		if ts.Before(start) || ts.After(end) {
			continue
		}
		if r.Allowed {
			allowed++
		} else {
			denied++
		}
	}
	return allowed, denied
}

// SignalSandbox forwards a signal to the sandbox.
func (s *Server) SignalSandbox(ctx context.Context, req *v1.SignalSandboxRequest) (*v1.SignalSandboxResponse, error) {
	backend, h, err := s.handle(req.GetSandboxId())
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
	backend, h, err := s.handle(req.GetSandboxId())
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
	if err := s.destroy(ctx, req.GetSandboxId(), req.GetActor().GetKeyId()); err != nil {
		return nil, err
	}
	return &v1.DestroySandboxResponse{}, nil
}

// destroy releases a sandbox's runtime resources and marks it DESTROYED.
func (s *Server) destroy(ctx context.Context, id, keyID string) error {
	rec, err := s.store.Load(id)
	if err != nil {
		return s.loadError(err, id)
	}
	if backend, h, herr := s.handle(rec.ID); herr == nil {
		if err := backend.Destroy(ctx, h); err != nil {
			s.logger.Warn("destroy backend failed", "sandbox_id", rec.ID, "error", err)
		}
	}
	s.delHandle(rec.ID)
	s.dropBrokerCredentials(rec.ID)
	if rec.State == sandbox.StateRunning {
		s.metrics.SandboxRunning.Dec()
		s.metrics.RunnerSandboxes.Dec()
	}
	if rec.State != sandbox.StateDestroyed {
		if err := s.transition(rec, sandbox.StateDestroyed, rec.Reason, keyID); err != nil {
			s.logger.Warn("state transition on destroy failed", "sandbox_id", rec.ID, "state", rec.State, "error", err)
		}
	}
	s.releaseDiskQuota(ctx, rec)
	if err := s.workspaces.Clean(rec.ID); err != nil {
		s.logger.Warn("clean workspace failed", "sandbox_id", rec.ID, "error", err)
	}
	return nil
}

// quotaPaths returns the writable directories a disk limit covers.
func quotaPaths(rec *sandbox.Record) []string {
	return []string{rec.Spec.Workspace.WorkspaceDir, rec.Spec.Workspace.HomeDir}
}

// applyDiskQuota assigns the sandbox's hard disk limit when the host supports
// project quotas. A failure is logged and metered, not fatal: the periodic
// enforcer still stops an over-quota sandbox, but the host loses the
// kernel-level guarantee. The caller must hold a persisted record.
func (s *Server) applyDiskQuota(ctx context.Context, rec *sandbox.Record) {
	limit := rec.Spec.Resources.DiskBytes
	if limit <= 0 {
		return
	}
	if err := s.quota.Enforce(ctx, rec.ID, quotaPaths(rec), limit); err != nil {
		s.metrics.DiskQuotaUnenforced.Inc()
		s.logger.Error("disk quota not enforced",
			"sandbox_id", rec.ID, "limit_bytes", limit, "quota_mode", string(s.quota.Mode()), "error", err)
	}
}

// releaseDiskQuota clears a sandbox's project quota, if any.
func (s *Server) releaseDiskQuota(ctx context.Context, rec *sandbox.Record) {
	if rec.Spec.Resources.DiskBytes <= 0 {
		return
	}
	if err := s.quota.Release(ctx, rec.ID, quotaPaths(rec)); err != nil {
		s.logger.Warn("release disk quota failed", "sandbox_id", rec.ID, "error", err)
	}
}

// EnforceDiskQuota stops running sandboxes whose writable bytes exceed their
// DiskBytes limit and records a semantic reason. It is the enforcement layer
// on hosts without project quotas, and the reason attribution on hosts with
// them (the kernel already refuses the writes there).
func (s *Server) EnforceDiskQuota(ctx context.Context) error {
	recs, err := s.store.List()
	if err != nil {
		return err
	}
	for _, rec := range recs {
		limit := rec.Spec.Resources.DiskBytes
		if rec.State != sandbox.StateRunning || limit <= 0 {
			continue
		}
		used, err := s.quota.Usage(quotaPaths(rec))
		if err != nil {
			s.logger.Warn("disk usage scan failed", "sandbox_id", rec.ID, "error", err)
			continue
		}
		if used <= limit {
			continue
		}
		s.logger.Warn("sandbox exceeded disk quota",
			"sandbox_id", rec.ID, "used_bytes", used, "limit_bytes", limit)
		s.fail(rec, runtime.ReasonDiskQuota, "")
		if err := s.destroy(ctx, rec.ID, ""); err != nil {
			s.logger.Warn("destroy over-quota sandbox failed", "sandbox_id", rec.ID, "error", err)
		}
	}
	return nil
}

// ReapExpired destroys running sandboxes whose TTL has elapsed.
func (s *Server) ReapExpired(ctx context.Context) error {
	recs, err := s.store.List()
	if err != nil {
		return err
	}
	now := time.Now()
	for _, rec := range recs {
		if rec.State != sandbox.StateRunning || rec.Spec.TTL <= 0 {
			continue
		}
		if now.Before(rec.CreatedAt.Add(rec.Spec.TTL)) {
			continue
		}
		s.logger.Info("sandbox TTL expired", "sandbox_id", rec.ID, "ttl", rec.Spec.TTL.String())
		if err := s.transition(rec, sandbox.StateExpired, runtime.ReasonTimeout, ""); err != nil {
			s.logger.Warn("expire transition failed", "sandbox_id", rec.ID, "error", err)
			continue
		}
		if err := s.destroy(ctx, rec.ID, ""); err != nil {
			s.logger.Warn("destroy expired sandbox failed", "sandbox_id", rec.ID, "error", err)
		}
	}
	return nil
}

// Reconcile fail-closes runner state at startup: active store records are
// reaped and marked FAILED, then leftover backend containers and systemd
// units are swept. Live sandboxes are not reattached.
func (s *Server) Reconcile(ctx context.Context) error {
	recs, err := s.store.List()
	if err != nil {
		return err
	}
	inStore := make(map[string]*sandbox.Record, len(recs))
	for _, rec := range recs {
		inStore[rec.ID] = rec
	}

	for _, rec := range recs {
		switch rec.State {
		case sandbox.StateRunning, sandbox.StateStarting, sandbox.StateCreating, sandbox.StateStopping:
		default:
			continue
		}
		s.logger.Warn("reconcile: reaping active sandbox", "sandbox_id", rec.ID, "state", rec.State)
		s.reapRuntime(ctx, rec)
		s.delHandle(rec.ID)
		if rec.State == sandbox.StateRunning {
			s.metrics.SandboxRunning.Dec()
			s.metrics.RunnerSandboxes.Dec()
		}
		if err := s.transition(rec, sandbox.StateFailed, runtime.ReasonRuntimeFailure, ""); err != nil {
			s.logger.Warn("reconcile: transition failed", "sandbox_id", rec.ID, "error", err)
		}
		s.releaseDiskQuota(ctx, rec)
		if err := s.workspaces.Clean(rec.ID); err != nil {
			s.logger.Warn("reconcile: clean failed", "sandbox_id", rec.ID, "error", err)
		}
	}

	for _, name := range s.registry.Names() {
		b, ok := s.registry.Get(name)
		if !ok {
			continue
		}
		sw, ok := b.(runtime.Sweeper)
		if !ok {
			continue
		}
		ids, err := sw.List(ctx)
		if err != nil {
			s.logger.Warn("reconcile: backend list failed", "backend", name, "error", err)
			continue
		}
		for _, id := range ids {
			if rec, ok := inStore[id]; ok && rec.State.Active() {
				continue
			}
			s.logger.Info("reconcile: reaping leftover sandbox", "backend", name, "sandbox_id", id)
			_ = sw.Reap(ctx, id)
		}
	}

	if s.cgroups != nil {
		units, err := s.cgroups.List(ctx)
		if err != nil {
			s.logger.Warn("reconcile: list units failed", "error", err)
		} else {
			for _, name := range units {
				if !cgroup.IsSandboxUnit(name) {
					continue
				}
				s.logger.Info("reconcile: removing leftover unit", "unit", name)
				if u, aerr := s.cgroups.Attach(ctx, name); aerr == nil {
					_ = u.Stop(ctx)
					_ = u.Remove(ctx)
				}
			}
		}
	}
	return nil
}

func (s *Server) reapRuntime(ctx context.Context, rec *sandbox.Record) {
	if rec.Spec.Backend != "" {
		if b, ok := s.registry.Get(rec.Spec.Backend); ok {
			if sw, ok := b.(runtime.Sweeper); ok {
				if err := sw.Reap(ctx, rec.ID); err != nil {
					s.logger.Warn("reconcile: reap failed", "backend", rec.Spec.Backend, "sandbox_id", rec.ID, "error", err)
				}
				return
			}
		}
	}
	for _, name := range s.registry.Names() {
		b, ok := s.registry.Get(name)
		if !ok {
			continue
		}
		sw, ok := b.(runtime.Sweeper)
		if !ok {
			continue
		}
		_ = sw.Reap(ctx, rec.ID)
	}
	if s.cgroups != nil {
		if u, err := s.cgroups.Attach(ctx, cgroup.UnitName(rec.ID)); err == nil {
			_ = u.Stop(ctx)
			_ = u.Remove(ctx)
		}
	}
}

// GetSandboxEvents returns the lifecycle event history for a sandbox.
func (s *Server) GetSandboxEvents(ctx context.Context, req *v1.GetSandboxEventsRequest) (*v1.GetSandboxEventsResponse, error) {
	if req.GetSandboxId() == "" {
		return nil, status.Error(codes.InvalidArgument, "sandbox_id is required")
	}
	if _, err := s.store.Load(req.GetSandboxId()); err != nil {
		return nil, s.loadError(err, req.GetSandboxId())
	}
	events, err := s.audit.Events(ctx, req.GetSandboxId())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "read events: %v", err)
	}
	return &v1.GetSandboxEventsResponse{Events: eventsToProto(events)}, nil
}

// GetSandboxExecs returns the execution audit records for a sandbox.
func (s *Server) GetSandboxExecs(ctx context.Context, req *v1.GetSandboxExecsRequest) (*v1.GetSandboxExecsResponse, error) {
	if req.GetSandboxId() == "" {
		return nil, status.Error(codes.InvalidArgument, "sandbox_id is required")
	}
	if _, err := s.store.Load(req.GetSandboxId()); err != nil {
		return nil, s.loadError(err, req.GetSandboxId())
	}
	execs, err := s.audit.Execs(ctx, req.GetSandboxId())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "read execs: %v", err)
	}
	return &v1.GetSandboxExecsResponse{Execs: execsToProto(execs)}, nil
}

// GetSandboxEgress returns restricted-network policy decisions for a sandbox.
func (s *Server) GetSandboxEgress(ctx context.Context, req *v1.GetSandboxEgressRequest) (*v1.GetSandboxEgressResponse, error) {
	if req.GetSandboxId() == "" {
		return nil, status.Error(codes.InvalidArgument, "sandbox_id is required")
	}
	if _, err := s.store.Load(req.GetSandboxId()); err != nil {
		return nil, s.loadError(err, req.GetSandboxId())
	}
	records, err := s.audit.Egress(ctx, req.GetSandboxId())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "read egress: %v", err)
	}
	return &v1.GetSandboxEgressResponse{Records: egressToProto(records)}, nil
}

// GetSandboxFile reads one file from a running sandbox's writable roots.
func (s *Server) GetSandboxFile(ctx context.Context, req *v1.GetSandboxFileRequest) (*v1.GetSandboxFileResponse, error) {
	spec, err := s.runningSpec(req.GetSandboxId())
	if err != nil {
		return nil, err
	}
	content, err := sandbox.ReadFile(spec, req.GetPath())
	if err != nil {
		return nil, fileError(err)
	}
	return &v1.GetSandboxFileResponse{Path: req.GetPath(), Content: content}, nil
}

// PutSandboxFile writes one file into a running sandbox's writable roots.
func (s *Server) PutSandboxFile(ctx context.Context, req *v1.PutSandboxFileRequest) (*v1.PutSandboxFileResponse, error) {
	spec, err := s.runningSpec(req.GetSandboxId())
	if err != nil {
		return nil, err
	}
	n, err := sandbox.WriteFile(spec, req.GetPath(), req.GetContent(), req.GetCreateParents())
	if err != nil {
		return nil, fileError(err)
	}
	return &v1.PutSandboxFileResponse{Path: req.GetPath(), BytesWritten: n}, nil
}

// runningSpec loads a sandbox and returns its workspace spec, requiring the
// RUNNING state so a file transfer never races creation or teardown.
func (s *Server) runningSpec(id string) (runtime.WorkspaceSpec, error) {
	if id == "" {
		return runtime.WorkspaceSpec{}, status.Error(codes.InvalidArgument, "sandbox_id is required")
	}
	rec, err := s.store.Load(id)
	if err != nil {
		return runtime.WorkspaceSpec{}, s.loadError(err, id)
	}
	if rec.State != sandbox.StateRunning {
		return runtime.WorkspaceSpec{}, status.Errorf(codes.FailedPrecondition, "sandbox %s is in state %s", id, rec.State)
	}
	return rec.Spec.Workspace, nil
}

// fileError maps filesystem failures to gRPC status codes.
func fileError(err error) error {
	switch {
	case errors.Is(err, sandbox.ErrPathOutsideWritableRoots), errors.Is(err, sandbox.ErrFileTooLarge):
		return status.Error(codes.InvalidArgument, err.Error())
	case errors.Is(err, os.ErrNotExist):
		return status.Error(codes.NotFound, err.Error())
	default:
		return status.Errorf(codes.Internal, "file transfer: %v", err)
	}
}

// selectBackend resolves the backend for a spec: an explicit spec.Backend wins,
// otherwise auto-select the strongest usable backend.
//
// gVisor is only preferred when the runner is UID 0 (root or a rootlesskit
// user namespace) AND the backend reports EnforcesResourceLimits. An
// unprivileged runner, or a gVisor without a writable cgroup subtree, falls
// back to bwrap.
func (s *Server) selectBackend(spec runtime.SandboxSpec) (runtime.Backend, error) {
	if spec.Backend != "" {
		if b, ok := s.registry.Get(spec.Backend); ok {
			return b, nil
		}
		return nil, status.Errorf(codes.InvalidArgument, "unknown backend %q", spec.Backend)
	}
	preferred := []string{}
	supports := func(name string) bool {
		b, ok := s.registry.Get(name)
		return ok && b.Capabilities().SupportsNetwork(spec.Network.Mode)
	}
	if os.Getuid() == 0 {
		if b, ok := s.registry.Get("gvisor"); ok && b.Capabilities().EnforcesResourceLimits && supports("gvisor") {
			preferred = append(preferred, "gvisor")
		}
	}
	if supports("bwrap") {
		preferred = append(preferred, "bwrap")
	}
	if b, ok := s.registry.Preferred(preferred...); ok {
		return b, nil
	}
	return nil, status.Error(codes.Unavailable, "no runtime backend is registered")
}

// actorFor attributes an audit record to the sandbox's owner. Tenant and agent
// identity are derived from the persisted spec, which is authoritative after
// create; keyID marks request-driven actions and is empty for machinery
// transitions.
func actorFor(spec runtime.SandboxSpec, keyID string) sandbox.Actor {
	actor := sandbox.Actor{TenantID: spec.TenantID, KeyID: keyID}
	if spec.AgentID != "" {
		actor.AgentIdentity = identity.Agent{Tenant: spec.TenantID, Name: spec.AgentID}.URI()
	}
	return actor
}

// recordActor attributes a record for a sandbox whose record may not load;
// losing attribution must never lose the record itself.
func (s *Server) recordActor(sandboxID, keyID string) sandbox.Actor {
	rec, err := s.store.Load(sandboxID)
	if err != nil {
		return sandbox.Actor{KeyID: keyID}
	}
	return actorFor(rec.Spec, keyID)
}

func (s *Server) transition(rec *sandbox.Record, to sandbox.State, reason runtime.ExitReason, keyID string) error {
	if rec.State == to {
		return nil
	}
	from := rec.State
	if err := sandbox.Transition(rec, to, reason); err != nil {
		return err
	}
	if err := s.store.Save(rec); err != nil {
		return err
	}
	event := sandbox.Event{
		SandboxID: rec.ID,
		Actor:     actorFor(rec.Spec, keyID),
		From:      from,
		To:        to,
		Reason:    string(reason),
		RunnerID:  s.runnerID,
	}
	if err := s.audit.AppendEvent(context.Background(), rec.ID, event); err != nil {
		s.logger.Debug("append event failed", "sandbox_id", rec.ID, "error", err)
	}
	return nil
}

func (s *Server) fail(rec *sandbox.Record, reason runtime.ExitReason, keyID string) {
	s.metrics.SandboxFailedTotal.WithLabelValues(string(reason)).Inc()
	if err := s.transition(rec, sandbox.StateFailed, reason, keyID); err != nil {
		s.logger.Warn("failed to record FAILED state", "sandbox_id", rec.ID, "state", rec.State, "error", err)
	}
}

func (s *Server) loadError(err error, id string) error {
	if errors.Is(err, sandbox.ErrNotFound) {
		return status.Errorf(codes.NotFound, "sandbox %s not found", id)
	}
	if errors.Is(err, sandbox.ErrInvalidID) {
		return status.Error(codes.InvalidArgument, "invalid sandbox_id")
	}
	return status.Error(codes.Internal, "failed to load sandbox")
}

func (s *Server) putHandle(id string, backend runtime.Backend, h runtime.Handle) {
	s.handlesMu.Lock()
	defer s.handlesMu.Unlock()
	s.handles[id] = sandboxHandle{backend: backend, handle: h}
}

func (s *Server) handle(id string) (runtime.Backend, runtime.Handle, error) {
	if id == "" {
		return nil, nil, status.Error(codes.InvalidArgument, "sandbox_id is required")
	}
	s.handlesMu.Lock()
	defer s.handlesMu.Unlock()
	sh, ok := s.handles[id]
	if !ok {
		return nil, nil, status.Errorf(codes.FailedPrecondition, "sandbox %s is not running", id)
	}
	return sh.backend, sh.handle, nil
}

func (s *Server) delHandle(id string) {
	s.handlesMu.Lock()
	defer s.handlesMu.Unlock()
	delete(s.handles, id)
}
