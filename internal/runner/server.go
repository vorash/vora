// Package runner implements the RunnerService control API.
package runner

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
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
	"github.com/vorash/vora/internal/pool"
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
	denyMu      sync.Mutex
	denyWindows map[string]denyWindow
	// allowedWindow is the aggregation window for allowed egress decisions.
	allowedWindow    time.Duration
	allowMu          sync.Mutex
	allowedWindows   map[allowedKey]*allowedCount
	allowedSandboxes map[string]*allowedCount
	createMu         sync.Mutex
	snapshotMu       sync.Mutex
	// sandboxLocks serialise destroy and audit pruning per sandbox, striped by
	// id so the set stays bounded however many tombstones accumulate.
	sandboxLocks [64]sync.Mutex
	// localAudit is true while the audit trail is the runner's own JSONL
	// store, the only one retention may prune. It is configuration, not a type
	// check, so wrapping the store cannot silently disable retention.
	localAudit bool

	pools  poolRegistry
	events poolEvents
	// auditTimeout bounds an audit write on the path of a sandbox start, exec or
	// destroy: a stalled store loses the record, logged and counted, rather than
	// holding the caller, and with it a pool member.
	auditTimeout time.Duration
	// execGrace is how long past its own timeout a pool run may stay open
	// before the runner ends it with a synthesized TIMEOUT_EXCEEDED.
	execGrace time.Duration
	// poolCloseTimeout bounds waiting for the pools to stop at shutdown.
	poolCloseTimeout time.Duration
	// poolOptions tune every pool's timeouts; zero means pool's defaults.
	poolOptions pool.Options
}

// defaultExecGrace is the design's hard deadline margin: timeout + 5 s.
const defaultExecGrace = 5 * time.Second

const (
	defaultAuditTimeout     = 5 * time.Second
	defaultPoolCloseTimeout = 10 * time.Second
)

// sandboxHandle binds a sandbox to the backend that manages it, so later RPCs
// (exec/stats/signal/destroy) use the right backend.
type denyWindow struct {
	start      time.Time
	written    int
	suppressed int
}

// Allowed decisions are audited individually up to allowedBudget per key per
// window; the rest are counted and written as one summary row. Every allowed
// decision stays accounted for, but a sandbox cannot turn an allowlisted name
// into an unbounded stream of synchronous audit writes.
//
// allowedSandboxBudget caps a sandbox's individual rows per window across all
// keys: allowlisting a domain allows every subdomain, so a sandbox that queries
// fresh names under a wildcard-DNS domain would otherwise get a new per-host
// budget, and a new map entry, for each one. Past the cap, names without an
// open window are counted under the sandbox's overflow host instead.
const (
	allowedBudget        = 10
	allowedSandboxBudget = 100
	allowedOverflowHost  = "*"
	defaultAllowedWindow = 10 * time.Second
)

type allowedKey struct{ sandboxID, host, alias, reason string }

type allowedCount struct {
	start      time.Time
	written    int
	suppressed int64
}

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
		denyWindows: make(map[string]denyWindow),
		localAudit:  true,

		allowedWindow:    defaultAllowedWindow,
		allowedWindows:   make(map[allowedKey]*allowedCount),
		allowedSandboxes: make(map[string]*allowedCount),
		execGrace:        defaultExecGrace,
		auditTimeout:     defaultAuditTimeout,
		poolCloseTimeout: defaultPoolCloseTimeout,
	}
	s.pools.byID = make(map[pool.ID]*poolEntry)
	s.pools.notLoaded = make(map[pool.ID]*notLoadedPool)
	s.events.init(s)
	s.setQuotaModeMetric()
	s.metrics.RegisterPoolSource(s.poolSamples)
	return s
}

// SetAudit replaces the audit store (e.g. with the PostgreSQL/Supabase store).
func (s *Server) SetAudit(a audit.Store) {
	if a != nil {
		s.audit = a
		s.localAudit = false
	}
}

// lockSandbox takes the lock that serialises destroy and pruning for id.
func (s *Server) lockSandbox(id string) func() {
	h := fnv.New32a()
	_, _ = h.Write([]byte(id))
	mu := &s.sandboxLocks[h.Sum32()%uint32(len(s.sandboxLocks))]
	mu.Lock()
	return mu.Unlock
}

// SetMetrics replaces the default metric set (used by the daemon).
func (s *Server) SetMetrics(m *telemetry.Metrics) {
	if m != nil {
		s.metrics = m
		s.setQuotaModeMetric()
		m.RegisterPoolSource(s.poolSamples)
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
	if !event.Allowed {
		s.denyMu.Lock()
		defer s.denyMu.Unlock()
		window := s.denyWindows[event.SandboxID]
		now := time.Now()
		if now.Sub(window.start) >= time.Second {
			if window.suppressed > 0 {
				summary := record
				summary.Host, summary.Remote, summary.Alias = "", "", ""
				summary.Reason = fmt.Sprintf("%d additional denials suppressed", window.suppressed)
				if err := s.audit.AppendEgress(context.Background(), event.SandboxID, summary); err != nil {
					return err
				}
			}
			window = denyWindow{start: now}
		}
		if window.written >= 20 {
			window.suppressed++
			s.denyWindows[event.SandboxID] = window
			return nil
		}
		window.written++
		s.denyWindows[event.SandboxID] = window
	}
	if event.Allowed {
		summary, key, suppressed, write := s.admitAllowed(record)
		if summary != nil {
			if err := s.audit.AppendEgress(context.Background(), event.SandboxID, *summary); err != nil {
				s.restoreSuppressed(key, suppressed)
				s.metrics.AuditWriteFailedTotal.Inc()
				s.logger.Error("write egress summary failed", "sandbox_id", event.SandboxID, "host", event.Host, "error", err)
				return err
			}
		}
		if !write {
			return nil
		}
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

func keyOf(r sandbox.EgressRecord) allowedKey {
	return allowedKey{r.SandboxID, r.Host, r.Alias, r.Reason}
}

func allowedSummary(k allowedKey, n int64, at time.Time, actor sandbox.Actor) sandbox.EgressRecord {
	return sandbox.EgressRecord{
		SandboxID: k.sandboxID, Actor: actor, Host: k.host, Alias: k.alias, Allowed: true,
		Reason:    fmt.Sprintf("%d additional allowed decisions aggregated (%s)", n, k.reason),
		Timestamp: at.UTC(),
	}
}

// admitAllowed decides whether an allowed decision is written individually,
// and returns the key it was counted under. It returns a summary of that key's
// previous window's suppressed decisions when the window has ended. It never
// holds its lock across an audit write, so one sandbox's flood cannot serialise
// every other tenant's allowed egress.
func (s *Server) admitAllowed(record sandbox.EgressRecord) (summary *sandbox.EgressRecord, k allowedKey, suppressed int64, write bool) {
	k = keyOf(record)
	now := time.Now()
	s.allowMu.Lock()
	defer s.allowMu.Unlock()
	sb := s.allowedSandboxes[k.sandboxID]
	if sb == nil || now.Sub(sb.start) >= s.allowedWindow {
		sb = &allowedCount{start: now}
		s.allowedSandboxes[k.sandboxID] = sb
	}
	overflow := s.allowedWindows[k] == nil && sb.written >= allowedSandboxBudget
	if overflow {
		k.host = allowedOverflowHost
	}
	c := s.allowedWindows[k]
	if c == nil || now.Sub(c.start) >= s.allowedWindow {
		if c != nil && c.suppressed > 0 {
			sum := allowedSummary(k, c.suppressed, now, record.Actor)
			summary, suppressed = &sum, c.suppressed
		}
		c = &allowedCount{start: now}
		s.allowedWindows[k] = c
	}
	if overflow || c.written >= allowedBudget || sb.written >= allowedSandboxBudget {
		c.suppressed++
		return summary, k, suppressed, false
	}
	c.written++
	sb.written++
	return summary, k, suppressed, true
}

// restoreSuppressed puts back counts whose summary could not be persisted, so
// a failed write does not lose them.
func (s *Server) restoreSuppressed(k allowedKey, n int64) {
	s.allowMu.Lock()
	defer s.allowMu.Unlock()
	c := s.allowedWindows[k]
	if c == nil {
		c = &allowedCount{start: time.Now()}
		s.allowedWindows[k] = c
	}
	c.suppressed += n
}

// FlushEgress writes out the suppressed-decision counts of aggregation windows
// that have ended, so they are recorded even if the sandbox goes quiet.
func (s *Server) FlushEgress(ctx context.Context) error {
	return s.flushAllowed(ctx, "", false)
}

// flushAllowed writes summaries for ended windows (or, with all set, every
// window of sandboxID) and forgets them.
func (s *Server) flushAllowed(ctx context.Context, sandboxID string, all bool) error {
	now := time.Now()
	type pending struct {
		key allowedKey
		n   int64
	}
	var due []pending
	s.allowMu.Lock()
	for k, c := range s.allowedWindows {
		if all && k.sandboxID != sandboxID || !all && now.Sub(c.start) < s.allowedWindow {
			continue
		}
		if c.suppressed > 0 {
			due = append(due, pending{k, c.suppressed})
		}
		delete(s.allowedWindows, k)
	}
	for id, sb := range s.allowedSandboxes {
		if all && id == sandboxID || !all && now.Sub(sb.start) >= s.allowedWindow {
			delete(s.allowedSandboxes, id)
		}
	}
	s.allowMu.Unlock()

	var firstErr error
	for _, p := range due {
		summary := allowedSummary(p.key, p.n, now, s.recordActor(p.key.sandboxID, ""))
		if err := s.audit.AppendEgress(ctx, p.key.sandboxID, summary); err != nil {
			s.restoreSuppressed(p.key, p.n)
			s.metrics.AuditWriteFailedTotal.Inc()
			s.logger.Error("write egress summary failed", "sandbox_id", p.key.sandboxID, "host", p.key.host, "error", err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
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
	if err := s.enforceResourceLimits(&spec); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
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

	s.createMu.Lock()
	defer s.createMu.Unlock()
	if err := s.checkSandboxCapacity(spec.TenantID); err != nil {
		return nil, err
	}
	rec, err := s.createRecord(ctx, spec)
	if err != nil {
		return nil, err
	}

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

// createRecord persists a validated spec as a REQUESTED record and prepares its
// workspace and disk quota. The caller has settled capacity: under createMu after
// checkSandboxCapacity, or through a pool's reservation.
func (s *Server) createRecord(ctx context.Context, spec runtime.SandboxSpec) (*sandbox.Record, error) {
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
	return rec, nil
}

// checkSandboxCapacity admits one more plain sandbox for tenant. The caller
// holds createMu.
func (s *Server) checkSandboxCapacity(tenant string) error {
	if !s.pools.isLoaded() {
		return status.Error(codes.Unavailable, "the runner is still starting")
	}
	global, forTenant, err := s.committed(tenant)
	if err != nil {
		return err
	}
	maxGlobal, maxTenant := s.capacityCaps()
	if global >= maxGlobal || forTenant >= maxTenant {
		return status.Error(codes.ResourceExhausted, "sandbox capacity reached")
	}
	return nil
}

// committed counts what is spoken for on this runner and for tenant: every
// active record that no loaded pool owns, plus each loaded pool's max_size.
// A pool's members are covered by its reservation and never counted again,
// so a pool that replaces members thousands of times never moves the books.
// A pooled record whose pool is not loaded (deleted, or not yet loaded) still
// counts, which keeps the books conservative.
func (s *Server) committed(tenant string) (global, forTenant int, err error) {
	records, err := s.store.List()
	if err != nil {
		return 0, 0, status.Error(codes.Internal, "failed to check sandbox capacity")
	}
	reservations := s.pools.reservations()
	for _, rec := range records {
		if rec.State == sandbox.StateDestroyed || rec.State == sandbox.StateFailed {
			continue
		}
		if _, pooled := reservations[pool.ID(rec.Spec.PoolID)]; pooled {
			continue
		}
		global++
		if rec.Spec.TenantID == tenant {
			forTenant++
		}
	}
	for _, r := range reservations {
		global += r.maxSize
		if r.tenant == tenant {
			forTenant += r.maxSize
		}
	}
	return global, forTenant, nil
}

// capacityCaps are VORA_MAX_SANDBOXES and VORA_MAX_SANDBOXES_PER_TENANT with
// their built-in defaults.
func (s *Server) capacityCaps() (maxGlobal, maxTenant int) {
	maxGlobal, maxTenant = int(s.cfg.MaxSandboxes), int(s.cfg.MaxSandboxesPerTenant)
	if maxGlobal <= 0 {
		maxGlobal = 64
	}
	if maxTenant <= 0 {
		maxTenant = 16
	}
	return maxGlobal, maxTenant
}

// rejectPooled refuses a sandbox RPC that would act on a pool member. A
// member is lent and reset by its pool alone; anything else acting on it
// would break the next run's proof of cleanliness. A missing record is left
// for the handler's own NotFound.
func (s *Server) rejectPooled(id string) error {
	if id == "" {
		return nil
	}
	rec, err := s.store.Load(id)
	if err != nil {
		return nil
	}
	if rec.Spec.PoolID != "" {
		return status.Errorf(codes.FailedPrecondition, "sandbox %s belongs to pool %s; use the pool API", id, rec.Spec.PoolID)
	}
	return nil
}

func (s *Server) enforceResourceLimits(spec *runtime.SandboxSpec) error {
	if spec.Resources.DiskBytes == 0 {
		spec.Resources.DiskBytes = s.cfg.DefaultDiskBytes
		if spec.Resources.DiskBytes <= 0 {
			spec.Resources.DiskBytes = 1 << 30
		}
	}
	maxMemory, maxCPU, maxPids, maxTTL, maxDisk := s.cfg.MaxMemoryBytes, s.cfg.MaxCPUMillis, s.cfg.MaxPids, s.cfg.MaxTTL, s.cfg.MaxDiskBytes
	if maxMemory <= 0 {
		maxMemory = 8 << 30
	}
	if maxCPU <= 0 {
		maxCPU = 8000
	}
	if maxPids <= 0 {
		maxPids = 512
	}
	if maxTTL <= 0 {
		maxTTL = 24 * time.Hour
	}
	if maxDisk <= 0 {
		maxDisk = 8 << 30
	}
	if spec.Resources.MemoryBytes > maxMemory || spec.Resources.CPUMillis > maxCPU || spec.Resources.Pids > maxPids || spec.Resources.DiskBytes > maxDisk || spec.TTL > maxTTL {
		return fmt.Errorf("sandbox resources or TTL exceed runner limits")
	}
	return nil
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
	if err := s.rejectPooled(req.GetSandboxId()); err != nil {
		return nil, err
	}
	rec, err := s.store.Load(req.GetSandboxId())
	if err != nil {
		return nil, s.loadError(err, req.GetSandboxId())
	}
	if rec.State != sandbox.StateRequested {
		return nil, status.Errorf(codes.FailedPrecondition, "sandbox %s is in state %s", rec.ID, rec.State)
	}
	if err := s.startRecord(ctx, rec, req.GetActor().GetKeyId()); err != nil {
		return nil, err
	}
	return toProtoSandbox(rec), nil
}

// startRecord takes a REQUESTED record to RUNNING: it picks the backend,
// resolves the image, creates and starts the sandbox with any staged broker
// credentials, and registers the handle.
func (s *Server) startRecord(ctx context.Context, rec *sandbox.Record, keyID string) error {
	backend, err := s.selectBackend(rec.Spec)
	if err != nil {
		return err
	}
	// A sandbox that grants broker aliases must have credentials; losing them
	// (for example across a runner restart) fails closed rather than starting
	// a sandbox whose granted aliases cannot work.
	creds := s.takeBrokerCredentials(rec.ID)
	defer zeroCredentials(creds)
	if len(rec.Spec.Network.BrokerAliases) > 0 && len(creds) == 0 {
		s.fail(rec, runtime.ReasonRuntimeFailure, keyID)
		return status.Error(codes.FailedPrecondition, "broker credentials are unavailable; recreate the sandbox")
	}
	// Record the chosen backend so future RPCs and `inspect` are explicit.
	if rec.Spec.Backend == "" {
		rec.Spec.Backend = backend.Name()
	}
	if !backend.Capabilities().SupportsNetwork(rec.Spec.Network.Mode) {
		return status.Errorf(codes.InvalidArgument, "backend %s does not support network mode %q", backend.Name(), rec.Spec.Network.Mode)
	}
	rootfs, err := s.images.Resolve(ctx, rec.Spec.Image)
	if err != nil {
		s.fail(rec, runtime.ReasonRuntimeFailure, keyID)
		return status.Error(codes.FailedPrecondition, err.Error())
	}
	if rootfs != rec.Spec.Workspace.RootfsDir {
		rec.Spec.Workspace.RootfsDir = rootfs
		if err := s.store.Save(rec); err != nil {
			s.fail(rec, runtime.ReasonRuntimeFailure, keyID)
			return status.Error(codes.Internal, "failed to persist resolved rootfs")
		}
	}

	if err := s.transition(rec, sandbox.StateScheduling, "", keyID); err != nil {
		return status.Error(codes.Internal, err.Error())
	}
	if err := s.transition(rec, sandbox.StateCreating, "", keyID); err != nil {
		return status.Error(codes.Internal, err.Error())
	}

	ctx, span := telemetry.Start(ctx, "runtime.start",
		attribute.String("sandbox.id", rec.ID),
		attribute.String("vora.image", rec.Spec.Image),
		attribute.String("vora.backend", backend.Name()))
	defer span.End()
	// A start that fails because the caller went away (a client disconnect
	// mid cold start) still has to be undone, so cleanup ignores ctx's cancel.
	cleanup := context.WithoutCancel(ctx)
	h, err := backend.Create(ctx, rec.Spec)
	if err != nil {
		s.fail(rec, runtime.ReasonRuntimeFailure, keyID)
		return status.Errorf(codes.Internal, "create sandbox: %v", err)
	}
	if err := s.transition(rec, sandbox.StateStarting, "", keyID); err != nil {
		_ = backend.Destroy(cleanup, h)
		return status.Error(codes.Internal, err.Error())
	}
	var startErr error
	if len(creds) > 0 {
		ba, ok := backend.(runtime.BrokerAware)
		if !ok {
			_ = backend.Destroy(cleanup, h)
			s.fail(rec, runtime.ReasonRuntimeFailure, keyID)
			return status.Errorf(codes.InvalidArgument, "backend %s does not support credential brokering", backend.Name())
		}
		startErr = ba.StartWithCredentials(ctx, h, creds)
	} else {
		startErr = backend.Start(ctx, h)
	}
	if startErr != nil {
		_ = backend.Destroy(cleanup, h)
		s.fail(rec, runtime.ReasonRuntimeFailure, keyID)
		return status.Errorf(codes.Internal, "start sandbox: %v", startErr)
	}
	s.putHandle(rec.ID, backend, h)
	if err := s.transition(rec, sandbox.StateRunning, "", keyID); err != nil {
		return status.Error(codes.Internal, err.Error())
	}
	s.metrics.SandboxRunning.Inc()
	s.metrics.RunnerSandboxes.Inc()
	s.logger.Info("sandbox running", "sandbox_id", rec.ID, "backend", backend.Name())
	return nil
}

// ExecSandbox runs a command in a running sandbox and streams its output.
func (s *Server) ExecSandbox(req *v1.ExecSandboxRequest, stream grpc.ServerStreamingServer[v1.ExecChunk]) error {
	start := time.Now()
	defer func() { s.metrics.ExecDuration.Observe(time.Since(start).Seconds()) }()
	if err := s.rejectPooled(req.GetSandboxId()); err != nil {
		return err
	}
	backend, h, err := s.handle(req.GetSandboxId())
	if err != nil {
		return err
	}
	if req.GetExec().GetExecutionId() != "" {
		return status.Error(codes.InvalidArgument, "execution_id is assigned by the runner")
	}
	_, err = s.runExec(stream.Context(), req.GetSandboxId(), backend, h, execRequestFromProto(req.GetExec()), req.GetActor().GetKeyId(), execOptions{}, stream.Send)
	return err
}

// execOptions are the ways a pool run differs from a plain exec. The zero
// value is a plain exec.
type execOptions struct {
	// poolID and sandboxRun go into the exec record, so the audit trail marks
	// run boundaries inside a reused sandbox.
	poolID     string
	sandboxRun int
	// hard, when set, closes at the run's hard deadline, on top of the backend's
	// own timeout. An exec still open past it is closed with a synthesized
	// TIMEOUT_EXCEEDED exit, whatever the backend is doing: a process holding the
	// exec's stdout cannot hold a pool member. A send that hard interrupted
	// returns errClientStalled, and ends the run the same way.
	hard <-chan struct{}
	// beforeFirst runs once, after the backend accepted the command and before
	// anything is forwarded, with the minted execution id. The pool sends its
	// run frame here. If runExec fails and beforeFirst never ran, the exec did
	// not start.
	beforeFirst func(executionID string) error
}

// timedOutExitCode is the exit code of a run the runner's hard deadline
// ended, as gVisor reports a killed exec.
const timedOutExitCode = -1

// runExec runs one command in a started sandbox. It mints the execution ID,
// meters the run, forwards the backend's chunks through send, and writes
// exactly one exec record, persisted before the exit chunk is sent. A run that
// ends without an exit chunk is recorded as a RuntimeFailure and returns an
// error. The returned reason is the one recorded.
func (s *Server) runExec(ctx context.Context, sandboxID string, backend runtime.Backend, h runtime.Handle, execReq runtime.ExecRequest, keyID string, opts execOptions, send func(*v1.ExecChunk) error) (runtime.ExitReason, error) {
	execReq.ExecutionID = ulid.NewExec()

	started := time.Now().UTC()
	execCtx, span := telemetry.Start(ctx, "runtime.exec",
		attribute.String("sandbox.id", sandboxID),
		attribute.String("vora.exec.command", execReq.Command))
	defer span.End()
	stopMeter := s.meterExec(execCtx, sandboxID, execReq.ExecutionID, backend, h)
	// Closes the window on paths that never read it, such as a failed send.
	defer stopMeter()
	var stdoutBytes, stderrBytes int64
	failed := func() runtime.ExitReason {
		s.recordExec(sandboxID, execReq, started, stdoutBytes, stderrBytes, 0, runtime.ReasonRuntimeFailure, stopMeter(), keyID, opts)
		s.metrics.ExecFailedTotal.WithLabelValues(string(runtime.ReasonRuntimeFailure)).Inc()
		return runtime.ReasonRuntimeFailure
	}
	// timedOut ends a run past opts.hard. The member's reset kills whatever still
	// holds the exec; here the run just ends.
	timedOut := func() (runtime.ExitReason, error) {
		usage := stopMeter()
		s.recordExec(sandboxID, execReq, started, stdoutBytes, stderrBytes, timedOutExitCode, runtime.ReasonTimeout, usage, keyID, opts)
		s.metrics.ExecTotal.WithLabelValues("ok").Inc()
		return runtime.ReasonTimeout, send(execExitToProto(timedOutExitCode, runtime.ReasonTimeout, usage, execReq.ExecutionID))
	}
	es, err := backend.Exec(execCtx, h, execReq)
	if err != nil {
		return failed(), status.Errorf(codes.Internal, "exec: %v", err)
	}
	defer func() { _ = es.Close() }()
	if opts.beforeFirst != nil {
		if err := opts.beforeFirst(execReq.ExecutionID); err != nil {
			return failed(), err
		}
	}

	recv, stop := recvAsync(es)
	defer close(stop)
	for {
		var r received
		select {
		case r = <-recv:
		case <-opts.hard:
			return timedOut()
		}
		chunk, err := r.chunk, r.err
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			failed()
			return runtime.ReasonRuntimeFailure, status.Errorf(codes.Internal, "exec stream: %v", err)
		}
		stdoutBytes += int64(len(chunk.Stdout))
		stderrBytes += int64(len(chunk.Stderr))
		if chunk.Exit != nil {
			// Persist before observing: the client may destroy the sandbox the
			// moment it sees the exit chunk (the API closes its response right
			// after forwarding it), so the record must be on disk first. The exit
			// chunk is final, so the byte totals are complete here.
			usage := stopMeter()
			reason := usage.Reason(chunk.Exit.Reason, chunk.Exit.ExitCode)
			s.recordExec(sandboxID, execReq, started, stdoutBytes, stderrBytes, chunk.Exit.ExitCode, reason, usage, keyID, opts)
			s.metrics.ExecTotal.WithLabelValues("ok").Inc()
			return reason, send(execExitToProto(chunk.Exit.ExitCode, reason, usage, execReq.ExecutionID))
		}
		if err := send(execChunkToProto(chunk)); err != nil {
			if errors.Is(err, errClientStalled) {
				return timedOut()
			}
			return failed(), err
		}
	}

	// EOF with no exit record is a killed run (watchdog, timeout, agent
	// death) — never a silent success.
	return failed(), status.Error(codes.Internal, "exec stream ended without an exit record (the run was killed)")
}

type received struct {
	chunk runtime.ExecChunk
	err   error
}

// recvAsync reads es on its own goroutine, so the caller can stop waiting at
// its deadline even if Recv never returns. The goroutine ends on the first
// error from Recv, or when stop is closed; es.Close after the deadline is
// what makes a blocked Recv return.
func recvAsync(es runtime.ExecStream) (<-chan received, chan<- struct{}) {
	out := make(chan received)
	stop := make(chan struct{})
	go func() {
		for {
			chunk, err := es.Recv()
			select {
			case out <- received{chunk, err}:
			case <-stop:
				return
			}
			if err != nil {
				return
			}
		}
	}()
	return out, stop
}

// meterExec opens the exec's usage window and returns the function that
// closes it. Only the first call stops the meter; later calls return the same
// usage, so every exit path can ask for it. A backend that cannot meter must
// not block the command: the exec then runs unmetered and reports zero usage
// with no peak scope.
func (s *Server) meterExec(ctx context.Context, sandboxID, executionID string, backend runtime.Backend, h runtime.Handle) func() runtime.Usage {
	m, err := backend.Meter(ctx, h)
	if err != nil {
		s.logger.Warn("exec runs unmetered", "sandbox_id", sandboxID, "execution_id", executionID, "error", err)
		return func() runtime.Usage { return runtime.Usage{} }
	}
	var (
		stopped bool
		usage   runtime.Usage
	)
	return func() runtime.Usage {
		if stopped {
			return usage
		}
		stopped = true
		// The record is still written after a client disconnect, so its usage
		// must not depend on the caller's context.
		u, err := m.Stop(context.WithoutCancel(ctx))
		if err != nil {
			s.logger.Warn("exec usage unavailable", "sandbox_id", sandboxID, "execution_id", executionID, "error", err)
			return usage
		}
		u.Measured = true
		usage = u
		return usage
	}
}

// recordExec persists an execution audit record. Failures are logged, not
// fatal: losing an audit write must not fail the caller's command.
func (s *Server) recordExec(sandboxID string, req runtime.ExecRequest, started time.Time, stdoutBytes, stderrBytes int64, code int, reason runtime.ExitReason, usage runtime.Usage, keyID string, opts execOptions) {
	finished := time.Now().UTC()
	rec := sandbox.ExecRecord{
		PoolID:      opts.poolID,
		SandboxRun:  opts.sandboxRun,
		ExecutionID: req.ExecutionID,
		SandboxID:   sandboxID,
		Actor:       s.recordActor(sandboxID, keyID),
		Command:     req.Command,
		Args:        req.Args,
		Cwd:         req.Cwd,
		StartedAt:   started,
		FinishedAt:  finished,
		ExitCode:    code,
		Reason:      reason,
		StdoutBytes: stdoutBytes,
		StderrBytes: stderrBytes,
	}
	if usage.Measured {
		rec.CPUMillis, rec.OOMKills, rec.PidsLimitHits = &usage.CPUMillis, &usage.OOMKills, &usage.PidsLimitHits
	}
	// A window can read its counters with no memory.peak to read.
	if usage.PeakScope != runtime.PeakUnspecified {
		rec.MemoryPeakBytes, rec.MemoryPeakScope = &usage.MemoryPeak, usage.PeakScope
	}
	rec.EgressAllowed, rec.EgressDenied = s.egressInWindow(sandboxID, started, finished)
	ctx, cancel := context.WithTimeout(context.Background(), s.auditTimeout)
	defer cancel()
	if err := s.audit.AppendExec(ctx, sandboxID, rec); err != nil {
		s.metrics.AuditWriteFailedTotal.Inc()
		s.logger.Warn("write exec record failed", "sandbox_id", sandboxID, "execution_id", req.ExecutionID, "error", err)
	}
}

// egressInWindow counts restricted-network decisions inside [start, end], so an
// execution's outbound requests and policy violations are recorded with it. Both
// are nil if the store could not count them.
func (s *Server) egressInWindow(sandboxID string, start, end time.Time) (allowed, denied *int64) {
	ctx, cancel := context.WithTimeout(context.Background(), s.auditTimeout)
	defer cancel()
	a, d, err := s.audit.EgressCounts(ctx, sandboxID, start, end)
	if err != nil {
		s.logger.Warn("count exec egress failed", "sandbox_id", sandboxID, "error", err)
		return nil, nil
	}
	return &a, &d
}

// SignalSandbox forwards a signal to the sandbox.
func (s *Server) SignalSandbox(ctx context.Context, req *v1.SignalSandboxRequest) (*v1.SignalSandboxResponse, error) {
	if err := s.rejectPooled(req.GetSandboxId()); err != nil {
		return nil, err
	}
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
	if err := s.rejectPooled(req.GetSandboxId()); err != nil {
		return nil, err
	}
	if err := s.destroy(ctx, req.GetSandboxId(), req.GetActor().GetKeyId()); err != nil {
		return nil, err
	}
	return &v1.DestroySandboxResponse{}, nil
}

// destroy releases a sandbox's runtime resources and marks it DESTROYED.
func (s *Server) destroy(ctx context.Context, id, keyID string) error {
	defer s.lockSandbox(id)()
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
	_ = s.flushAllowed(ctx, rec.ID, true) // failures are logged and metered inside
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
		// A pool member belongs to its pool, which retires it: now if idle, at
		// check-in if a run holds it. Only a record no loaded pool owns is
		// destroyed here.
		if entry := s.pools.get(pool.ID(rec.Spec.PoolID)); entry != nil && entry.pool.RetireExpired(rec.ID) {
			s.logger.Info("pool member TTL expired, handed to its pool", "sandbox_id", rec.ID, "pool_id", rec.Spec.PoolID)
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

// PruneAudit removes the tombstone, including the local audit trail, of
// sandboxes that finished longer ago than the configured retention. Retention
// is off by default: destroy never deletes audit, only this reaper does. It
// only applies to the local JSONL trail: with an external audit store the
// tombstone is what makes that store's rows reachable through the API.
func (s *Server) PruneAudit(ctx context.Context) error {
	if !s.localAudit || s.cfg.AuditRetention <= 0 {
		return nil
	}
	recs, err := s.store.List()
	if err != nil {
		return err
	}
	cutoff := time.Now().Add(-s.cfg.AuditRetention)
	for _, rec := range recs {
		if !rec.State.Terminal() || !rec.UpdatedAt.Before(cutoff) {
			continue
		}
		s.pruneOne(ctx, rec.ID, cutoff)
	}
	return nil
}

// pruneOne removes one expired tombstone. It re-reads the record under the
// sandbox lock: a destroy that raced the listing may have just rewritten it,
// and deleting underneath that destroy would resurrect a partial tombstone.
func (s *Server) pruneOne(ctx context.Context, id string, cutoff time.Time) {
	defer s.lockSandbox(id)()
	rec, err := s.store.Load(id)
	if err != nil || !rec.State.Terminal() || !rec.UpdatedAt.Before(cutoff) {
		return
	}
	// A sandbox that failed without being destroyed may still hold its
	// project quota; release it so the limit does not outlive the directory.
	if rec.State != sandbox.StateDestroyed {
		s.releaseDiskQuota(ctx, rec)
	}
	if err := s.store.Delete(rec.ID); err != nil {
		s.logger.Warn("prune audit failed", "sandbox_id", rec.ID, "error", err)
		return
	}
	s.logger.Info("audit trail pruned", "sandbox_id", rec.ID, "retention", s.cfg.AuditRetention.String())
}

// Reconcile fail-closes runner state at startup: active store records are
// reaped and marked FAILED, then leftover backend containers and systemd
// units are swept. Live sandboxes are not reattached. A plain REQUESTED record
// waits for its StartSandbox, but a pool member is started by its pool alone, so
// one stranded before CREATING has nobody left to start or remove it.
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
		case sandbox.StateRequested, sandbox.StateScheduling:
			if rec.Spec.PoolID == "" {
				continue
			}
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
	rec, err := s.store.Load(req.GetSandboxId())
	if err != nil {
		return nil, s.loadError(err, req.GetSandboxId())
	}
	if req.GetPoolId() != "" && rec.Spec.PoolID != req.GetPoolId() {
		return nil, status.Errorf(codes.NotFound, "sandbox %s not found in pool %s", rec.ID, req.GetPoolId())
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
	if err := s.rejectPooled(req.GetSandboxId()); err != nil {
		return nil, err
	}
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
	if err := s.rejectPooled(req.GetSandboxId()); err != nil {
		return nil, err
	}
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
	ctx, cancel := context.WithTimeout(context.Background(), s.auditTimeout)
	defer cancel()
	if err := s.audit.AppendEvent(ctx, rec.ID, event); err != nil {
		s.metrics.AuditWriteFailedTotal.Inc()
		s.logger.Warn("append event failed", "sandbox_id", rec.ID, "to", to, "error", err)
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
