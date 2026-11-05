package runner

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	v1 "github.com/vorash/vora/api/proto/v1"
	"github.com/vorash/vora/internal/pool"
	"github.com/vorash/vora/internal/runtime"
	"github.com/vorash/vora/internal/sandbox"
	"github.com/vorash/vora/internal/telemetry"
	"github.com/vorash/vora/internal/ulid"
)

// poolEntry is one declared pool: the definition the runner accepted, the
// live pool, and its record's metadata. draining is set by DeletePool and
// persisted, so a restart mid-delete removes the pool instead of refilling it.
type poolEntry struct {
	spec      pool.Spec
	pool      *pool.Pool
	createdAt time.Time
	draining  atomic.Bool
}

// label is the pool's metric label, stable across a delete and recreate.
func (e *poolEntry) label() string { return e.spec.TenantID() + "/" + e.spec.Name() }

// poolRegistry is the runner's table of loaded pools. createMu, not this
// mutex, serialises a pool's reservation against sandbox creates: CreatePool
// and DeletePool take createMu around the add and remove, and
// checkSandboxCapacity runs under it, so the capacity books never see a half
// reserved pool.
type poolRegistry struct {
	mu sync.Mutex
	// loaded is set by LoadPools. Pool RPCs are Unavailable before it: the
	// gRPC servers in main serve before Reconcile and LoadPools run.
	loaded bool
	byID   map[pool.ID]*poolEntry
}

type reservation struct {
	tenant  string
	maxSize int
}

func (r *poolRegistry) get(id pool.ID) *poolEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.byID[id]
}

func (r *poolRegistry) byName(tenant, name string) *poolEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, e := range r.byID {
		if e.spec.TenantID() == tenant && e.spec.Name() == name {
			return e
		}
	}
	return nil
}

func (r *poolRegistry) add(e *poolEntry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.byID[e.spec.ID()] = e
}

func (r *poolRegistry) remove(id pool.ID) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.byID, id)
}

func (r *poolRegistry) all() []*poolEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*poolEntry, 0, len(r.byID))
	for _, e := range r.byID {
		out = append(out, e)
	}
	return out
}

// reservations is what every loaded pool holds against capacity: its
// max_size, whether its members are busy, idle or not started yet.
func (r *poolRegistry) reservations() map[pool.ID]reservation {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[pool.ID]reservation, len(r.byID))
	for id, e := range r.byID {
		out[id] = reservation{tenant: e.spec.TenantID(), maxSize: e.spec.MaxSize()}
	}
	return out
}

func (r *poolRegistry) isLoaded() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.loaded
}

// poolLimits are the runner-wide rules a pool spec is checked against when it
// is declared, so a pool that could only fail is refused up front. MaxSize is
// the smaller of the global and the per-tenant sandbox cap: a pool reserves
// max_size members of its tenant's cap, so a larger one could never fit.
func (s *Server) poolLimits() pool.Limits {
	maxGlobal, maxTenant := s.capacityCaps()
	return pool.Limits{
		MaxTTL:           s.cfg.MaxTTL,
		MaxMemoryBytes:   s.cfg.MaxMemoryBytes,
		MaxCPUMillis:     s.cfg.MaxCPUMillis,
		MaxPids:          s.cfg.MaxPids,
		MaxDiskBytes:     s.cfg.MaxDiskBytes,
		MaxSize:          min(maxGlobal, maxTenant),
		AllowUnsafeReuse: s.cfg.PoolUnsafeReuse,
		Reusable: func(backend string) bool {
			b, ok := s.registry.Get(backend)
			if !ok {
				return false
			}
			_, ok = b.(runtime.Reusable)
			return ok
		},
		HasBackend: func(backend string) bool {
			_, ok := s.registry.Get(backend)
			return ok
		},
	}
}

// startPool builds the live pool for an accepted spec. Members appear as
// its refill runs.
func (s *Server) startPool(spec pool.Spec, createdAt time.Time) *poolEntry {
	e := &poolEntry{spec: spec, createdAt: createdAt}
	adapter := poolSandboxes{s: s, actor: actorFor(spec.MemberSpec(), "")}
	e.pool = pool.New(spec, adapter, s.poolOptions)
	return e
}

// LoadPools restarts every persisted pool after Reconcile has failed their old
// members. A draining record is removed. A record the current limits refuse
// (an unsafe grant that is gone, a backend no longer registered) is removed
// and logged, so the control plane sees NotFound and drops its route rather
// than a pool that runs under rules it was not declared with.
func (s *Server) LoadPools() error {
	if s.pools.isLoaded() {
		return nil
	}
	recs, err := s.loadPoolRecords()
	if err != nil {
		return err
	}
	limits := s.poolLimits()
	for _, rec := range recs {
		if rec.Draining {
			s.logger.Info("pool was draining at shutdown; removed", "pool_id", rec.ID)
			_ = s.deletePoolRecord(rec.ID)
			continue
		}
		spec, err := pool.Parse(rec.Spec, limits)
		if err != nil {
			s.logger.Warn("refusing persisted pool", "pool_id", rec.ID, "name", rec.Spec.Name, "tenant_id", rec.Spec.TenantID, "error", err)
			_ = s.deletePoolRecord(rec.ID)
			continue
		}
		s.pools.add(s.startPool(spec, rec.CreatedAt))
		s.logger.Info("pool loaded", "pool_id", rec.ID, "name", rec.Spec.Name, "tenant_id", rec.Spec.TenantID, "size", spec.Size(), "max_size", spec.MaxSize())
	}
	s.pools.mu.Lock()
	s.pools.loaded = true
	s.pools.mu.Unlock()
	return nil
}

// SweepPools runs every pool's periodic work: idle expiry, max_age, retries
// of failed destroys, refill. Draining pools are swept too, since between
// DeletePool calls only this retries their destroys.
func (s *Server) SweepPools() {
	for _, e := range s.pools.all() {
		e.pool.Sweep()
	}
}

// ClosePools stops every pool's background work and flushes pool events. It
// destroys nothing: the next start's Reconcile owns what is left.
func (s *Server) ClosePools() {
	for _, e := range s.pools.all() {
		e.pool.Close()
	}
	s.events.flush()
}

// poolSamples is the scrape-time source of the pool state metrics.
func (s *Server) poolSamples() []telemetry.PoolSample {
	entries := s.pools.all()
	out := make([]telemetry.PoolSample, 0, len(entries))
	for _, e := range entries {
		st := e.pool.Stats()
		retired := make(map[string]uint64, len(st.Retired))
		for cause, n := range st.Retired {
			retired[string(cause)] = n
		}
		out = append(out, telemetry.PoolSample{
			Pool: e.label(),
			States: map[string]int{
				"starting": st.Starting, "idle": st.Idle, "busy": st.Busy,
				"resetting": st.Resetting, "retiring": st.Retiring,
			},
			Retired:     retired,
			Exhausted:   st.ExhaustedTotal,
			StartFailed: st.StartFailedTotal,
		})
	}
	return out
}

// lookupPool finds a pool for a tenant. A pool another tenant owns is
// NotFound, as if it did not exist. tenant_id is required, so a caller that
// forgets it never reaches a pool by id alone.
func (s *Server) lookupPool(id, tenant string) (*poolEntry, error) {
	if !s.pools.isLoaded() {
		return nil, status.Error(codes.Unavailable, "pools are not loaded yet")
	}
	if !validPoolID(id) {
		return nil, status.Error(codes.InvalidArgument, "invalid pool_id")
	}
	if tenant == "" {
		return nil, status.Error(codes.InvalidArgument, "tenant_id is required")
	}
	e := s.pools.get(pool.ID(id))
	if e == nil || e.spec.TenantID() != tenant {
		return nil, status.Errorf(codes.NotFound, "pool %s not found", id)
	}
	return e, nil
}

// CreatePool declares a pool. It is idempotent by (tenant, name): the same
// spec again returns the existing pool, a different one is AlreadyExists.
// The pool's max_size is reserved against the runner's sandbox caps here,
// once, and its members are never counted again.
func (s *Server) CreatePool(ctx context.Context, req *v1.CreatePoolRequest) (*v1.Pool, error) {
	if !s.pools.isLoaded() {
		return nil, status.Error(codes.Unavailable, "pools are not loaded yet")
	}
	raw, err := poolSpecFromProto(req.GetSpec())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	raw.ID = pool.ID(ulid.NewPool())
	if raw.Template.Resources.DiskBytes == 0 {
		raw.Template.Resources.DiskBytes = s.cfg.DefaultDiskBytes
		if raw.Template.Resources.DiskBytes <= 0 {
			raw.Template.Resources.DiskBytes = 1 << 30
		}
	}
	spec, err := pool.Parse(raw, s.poolLimits())
	if err != nil {
		return nil, poolStatus(err)
	}

	s.createMu.Lock()
	defer s.createMu.Unlock()
	if existing := s.pools.byName(spec.TenantID(), spec.Name()); existing != nil {
		switch {
		case existing.draining.Load():
			return nil, status.Errorf(codes.FailedPrecondition, "pool %s is draining; retry once it is gone", spec.Name())
		case proto.Equal(poolSpecToProto(existing.spec), poolSpecToProto(spec)):
			return s.poolToProto(existing), nil
		default:
			return nil, status.Errorf(codes.AlreadyExists, "pool %s exists with a different spec", spec.Name())
		}
	}
	global, forTenant, err := s.committed(spec.TenantID())
	if err != nil {
		return nil, err
	}
	maxGlobal, maxTenant := s.capacityCaps()
	if global+spec.MaxSize() > maxGlobal || forTenant+spec.MaxSize() > maxTenant {
		return nil, status.Errorf(codes.ResourceExhausted, "sandbox capacity reached: pool needs %d of %d free", spec.MaxSize(), maxTenant-forTenant)
	}
	rec := poolRecord{ID: spec.ID(), Spec: spec.Raw(), CreatedAt: time.Now().UTC()}
	if err := s.savePool(rec); err != nil {
		s.logger.Error("persist pool failed", "pool_id", spec.ID(), "error", err)
		return nil, status.Error(codes.Internal, "failed to persist pool")
	}
	e := s.startPool(spec, rec.CreatedAt)
	s.pools.add(e)
	s.logger.Info("pool declared", "pool_id", spec.ID(), "name", spec.Name(), "tenant_id", spec.TenantID(),
		"backend", raw.Template.Backend, "size", spec.Size(), "max_size", spec.MaxSize(), "reuse", spec.Reuse().String())
	return s.poolToProto(e), nil
}

// GetPool returns a pool's spec, state and live stats.
func (s *Server) GetPool(ctx context.Context, req *v1.GetPoolRequest) (*v1.Pool, error) {
	e, err := s.lookupPool(req.GetPoolId(), req.GetTenantId())
	if err != nil {
		return nil, err
	}
	return s.poolToProto(e), nil
}

// DeletePool drains the pool under the request's deadline and reports
// whether every member is gone. While busy members finish, the pool stays
// registered as draining and the caller repeats the call.
func (s *Server) DeletePool(ctx context.Context, req *v1.DeletePoolRequest) (*v1.DeletePoolResponse, error) {
	e, err := s.lookupPool(req.GetPoolId(), req.GetTenantId())
	if err != nil {
		return nil, err
	}
	if !e.draining.Swap(true) {
		rec := poolRecord{ID: e.spec.ID(), Spec: e.spec.Raw(), CreatedAt: e.createdAt, Draining: true}
		if err := s.savePool(rec); err != nil {
			s.logger.Warn("persist pool draining failed", "pool_id", e.spec.ID(), "error", err)
		}
		s.logger.Info("pool draining", "pool_id", e.spec.ID(), "name", e.spec.Name())
	}
	if !e.pool.Drain(ctx) {
		return &v1.DeletePoolResponse{Drained: false}, nil
	}
	e.pool.Close()
	s.createMu.Lock()
	s.pools.remove(e.spec.ID())
	s.createMu.Unlock()
	if err := s.deletePoolRecord(e.spec.ID()); err != nil {
		s.logger.Warn("delete pool record failed", "pool_id", e.spec.ID(), "error", err)
	}
	s.logger.Info("pool deleted", "pool_id", e.spec.ID(), "name", e.spec.Name())
	return &v1.DeletePoolResponse{Drained: true}, nil
}

// RunInPool runs one command on a pool member and streams run, output, exit
// and run_done. The run frame goes out once the member accepted the command,
// so a member whose exec did not start is retired and the run retried on
// another before the client sees anything. The exec itself is the same
// runExec as a plain exec, with a hard deadline of timeout plus execGrace.
func (s *Server) RunInPool(req *v1.RunInPoolRequest, stream grpc.ServerStreamingServer[v1.RunChunk]) error {
	e, err := s.lookupPool(req.GetPoolId(), req.GetTenantId())
	if err != nil {
		return err
	}
	if req.GetExec().GetExecutionId() != "" {
		return status.Error(codes.InvalidArgument, "execution_id is assigned by the runner")
	}
	execReq := execRequestFromProto(req.GetExec())
	switch {
	case execReq.Timeout <= 0:
		return status.Error(codes.InvalidArgument, "timeout_seconds is required")
	case execReq.Timeout > e.spec.MaxRun():
		return status.Errorf(codes.InvalidArgument, "timeout_seconds exceeds the pool's max_run of %s", e.spec.MaxRun())
	}
	keyID := req.GetActor().GetKeyId()

	var (
		placement pool.Placement
		reason    runtime.ExitReason
		execStart time.Time
	)
	done, err := e.pool.Run(stream.Context(), func(ctx context.Context, pl pool.Placement) error {
		placement = pl
		backend, h, err := s.handle(pl.SandboxID)
		if err != nil {
			return fmt.Errorf("%w: %w", pool.ErrNotStarted, err)
		}
		started := false
		execStart = time.Now()
		reason, err = s.runExec(ctx, pl.SandboxID, backend, h, execReq, keyID, execOptions{
			poolID:       string(e.spec.ID()),
			sandboxRun:   pl.SandboxRun,
			hardDeadline: execReq.Timeout + s.execGrace,
			beforeFirst: func() error {
				started = true
				return stream.Send(runFrameToProto(e.spec.ID(), pl, ""))
			},
		}, func(c *v1.ExecChunk) error { return stream.Send(execToRunChunk(c)) })
		if err != nil && !started {
			return fmt.Errorf("%w: %w", pool.ErrNotStarted, err)
		}
		return err
	})
	if placement.SandboxID != "" && !errors.Is(err, pool.ErrNotStarted) {
		s.observeRun(e, placement, reason, time.Since(execStart), done)
	}
	if err != nil {
		return poolStatus(err)
	}
	return stream.Send(runDoneToProto(done))
}

// observeRun records one run that reached a member.
func (s *Server) observeRun(e *poolEntry, pl pool.Placement, reason runtime.ExitReason, took time.Duration, done pool.Done) {
	start := "cold"
	if pl.Warm {
		start = "warm"
	}
	label := e.label()
	s.metrics.PoolRunsTotal.WithLabelValues(label, start, string(reason)).Inc()
	s.metrics.PoolRunDuration.WithLabelValues(label, start).Observe(took.Seconds())
	s.metrics.PoolStartup.WithLabelValues(label, start).Observe(pl.Startup.Seconds())
	switch done.Reset {
	case pool.ResetOK:
		s.metrics.PoolResetDuration.WithLabelValues(label, "ok").Observe(done.Duration.Seconds())
	case pool.ResetFailed:
		s.metrics.PoolResetDuration.WithLabelValues(label, "failed").Observe(done.Duration.Seconds())
	}
}

// poolSandboxes is the runner side of pool.Sandboxes: one member is one
// ordinary sandbox record with a PoolID, provisioned through the same
// createRecord and startRecord as a plain sandbox. Capacity is not checked
// per member because the pool reserved it, and createMu is held only for the
// record, so a member start never serialises other creates.
type poolSandboxes struct {
	s     *Server
	actor sandbox.Actor
}

func (a poolSandboxes) Start(ctx context.Context, spec runtime.SandboxSpec, prepare bool) (pool.Started, error) {
	s := a.s
	spec.ID = ulid.NewSandbox()
	if err := s.enforceResourceLimits(&spec); err != nil {
		return pool.Started{}, status.Error(codes.InvalidArgument, err.Error())
	}
	s.createMu.Lock()
	rec, err := s.createRecord(ctx, spec)
	s.createMu.Unlock()
	if err != nil {
		a.undo(ctx, spec.ID)
		return pool.Started{}, err
	}
	if err := s.startRecord(ctx, rec, ""); err != nil {
		a.undo(ctx, rec.ID)
		return pool.Started{}, err
	}
	started := pool.Started{SandboxID: rec.ID}
	if !prepare {
		return started, nil
	}
	backend, h, err := s.handle(rec.ID)
	if err != nil {
		a.undo(ctx, rec.ID)
		return pool.Started{}, err
	}
	reusable, ok := backend.(runtime.Reusable)
	if !ok {
		a.undo(ctx, rec.ID)
		return pool.Started{}, status.Errorf(codes.FailedPrecondition, "backend %s cannot reset sandboxes", backend.Name())
	}
	reset, err := reusable.Prepare(ctx, h)
	if err != nil {
		s.logger.Warn("pool member failed admission", "sandbox_id", rec.ID, "pool_id", spec.PoolID, "error", err)
		a.undo(ctx, rec.ID)
		return pool.Started{}, fmt.Errorf("prepare: %w", err)
	}
	started.Reset = reset
	return started, nil
}

// undo removes whatever a failed Start left behind. It runs on a context
// detached from ctx: a cold start runs on the client's context, and a client
// that disconnected mid-start must not leak a half-started sandbox.
func (a poolSandboxes) undo(ctx context.Context, id string) {
	err := a.s.destroy(context.WithoutCancel(ctx), id, "")
	if err != nil && status.Code(err) != codes.NotFound {
		a.s.logger.Warn("undo pool member start failed", "sandbox_id", id, "error", err)
	}
}

func (a poolSandboxes) Destroy(ctx context.Context, sandboxID string) error {
	return a.s.destroy(ctx, sandboxID, "")
}

func (a poolSandboxes) Event(sandboxID, kind, detail string) {
	a.s.events.append(sandbox.Event{
		SandboxID: sandboxID,
		Actor:     a.actor,
		From:      sandbox.StateRunning,
		To:        sandbox.StateRunning,
		Kind:      kind,
		Detail:    detail,
		RunnerID:  a.s.runnerID,
	})
}
