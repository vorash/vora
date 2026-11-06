package runner

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
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

// notLoadedPool is a persisted pool this boot refused. Its record stays on disk,
// so a later boot that can honour it loads it again. It holds no reservation and
// has no members, but it keeps its (tenant, name).
type notLoadedPool struct {
	rec    poolRecord
	reason string
}

// Why a persisted pool was not loaded. These reach the tenant through GetPool, so
// each is a fixed word: the detail, which can name host paths, goes to the log.
const (
	notLoadedBackendMissing = "backend_missing"
	notLoadedNotReusable    = "backend_not_reusable"
	notLoadedUnsafeRefused  = "unsafe_reuse_not_granted"
	notLoadedSpecRefused    = "spec_refused"
	notLoadedCapacity       = "capacity"
	notLoadedDuplicate      = "duplicate_name"
)

// errPoolNotLoaded is the answer for a pool that is known but not loaded. It is
// a gRPC status as it stands, and GetPool and DeletePool look for it.
type errPoolNotLoaded struct{ pool *notLoadedPool }

func (e errPoolNotLoaded) Error() string {
	return fmt.Sprintf("pool %s is not loaded on this runner: %s", e.pool.rec.ID, e.pool.reason)
}

func (e errPoolNotLoaded) GRPCStatus() *status.Status {
	return status.New(codes.FailedPrecondition, e.Error())
}

// poolRegistry is the runner's table of pools. createMu, not this mutex,
// serialises a pool's reservation against sandbox creates: CreatePool, DeletePool
// and LoadPools take createMu around the add and remove, and checkSandboxCapacity
// runs under it, so the capacity books never see a half reserved pool.
type poolRegistry struct {
	mu sync.Mutex
	// loaded is set by LoadPools. Pool RPCs and sandbox creates are Unavailable
	// before it, since the gRPC servers in main serve before Reconcile and
	// LoadPools run and the capacity books mean nothing until then.
	loaded bool
	// closed is set by ClosePools, which stops CreatePool.
	closed    bool
	byID      map[pool.ID]*poolEntry
	notLoaded map[pool.ID]*notLoadedPool
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

func (r *poolRegistry) notLoadedPool(id pool.ID) *notLoadedPool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.notLoaded[id]
}

func (r *poolRegistry) notLoadedByName(tenant, name string) *notLoadedPool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, n := range r.notLoaded {
		if n.rec.Spec.TenantID == tenant && n.rec.Spec.Name == name {
			return n
		}
	}
	return nil
}

func (r *poolRegistry) refuse(n *notLoadedPool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.notLoaded[n.rec.ID] = n
}

func (r *poolRegistry) forget(id pool.ID) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.notLoaded, id)
}

func (r *poolRegistry) refused() []*notLoadedPool {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*notLoadedPool, 0, len(r.notLoaded))
	for _, n := range r.notLoaded {
		out = append(out, n)
	}
	return out
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

func (r *poolRegistry) isClosed() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.closed
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
// members, oldest first, so which pool fits a lowered cap does not depend on the
// order of a directory listing. A draining record is removed. A record this boot
// cannot honour stays on disk, is logged and is reported through GetPool and
// vora_pool_not_loaded, and starts no members: losing a definition because a
// backend failed to register once would be permanent, and a pool whose unsafe
// grant is gone must not run at all, not even with a reset. A later boot that can
// honour it loads it again.
func (s *Server) LoadPools() error {
	s.createMu.Lock()
	defer s.createMu.Unlock()
	if s.pools.isLoaded() {
		return nil
	}
	recs, err := s.loadPoolRecords()
	if err != nil {
		return err
	}
	slices.SortFunc(recs, func(a, b poolRecord) int {
		return cmp.Or(a.CreatedAt.Compare(b.CreatedAt), cmp.Compare(a.ID, b.ID))
	})
	limits := s.poolLimits()
	for _, rec := range recs {
		if rec.Draining {
			s.logger.Info("pool was draining at shutdown; removed", "pool_id", rec.ID)
			if err := s.deletePoolRecord(rec.ID); err != nil {
				s.logger.Warn("delete pool record failed", "pool_id", rec.ID, "error", err)
			}
			continue
		}
		spec, reason, err := s.admitPersisted(rec, limits)
		if err != nil {
			s.logger.Error("pool NOT loaded: its record is kept, and it runs nothing until a boot can honour it",
				"pool_id", rec.ID, "name", rec.Spec.Name, "tenant_id", rec.Spec.TenantID, "reason", reason, "error", err)
			s.pools.refuse(&notLoadedPool{rec: rec, reason: reason})
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

// admitPersisted decides whether a record can load on this boot. The caller holds
// createMu. On refusal it returns the tenant-safe reason and the detail for the log.
func (s *Server) admitPersisted(rec poolRecord, limits pool.Limits) (pool.Spec, string, error) {
	if !limits.HasBackend(rec.Spec.Template.Backend) {
		return pool.Spec{}, notLoadedBackendMissing, fmt.Errorf("no backend %q", rec.Spec.Template.Backend)
	}
	spec, err := pool.Parse(rec.Spec, limits)
	switch {
	case errors.Is(err, pool.ErrUnsafeRefused):
		return pool.Spec{}, notLoadedUnsafeRefused, err
	case errors.Is(err, pool.ErrNotReusable):
		return pool.Spec{}, notLoadedNotReusable, err
	case err != nil:
		return pool.Spec{}, notLoadedSpecRefused, err
	}
	if other := s.pools.byName(spec.TenantID(), spec.Name()); other != nil {
		return pool.Spec{}, notLoadedDuplicate, fmt.Errorf("pool %s holds this name", other.spec.ID())
	}
	if err := s.checkPoolCapacity(spec); err != nil {
		return pool.Spec{}, notLoadedCapacity, err
	}
	return spec, "", nil
}

// checkPoolCapacity admits a pool's whole reservation. The caller holds createMu.
func (s *Server) checkPoolCapacity(spec pool.Spec) error {
	global, forTenant, err := s.committed(spec.TenantID())
	if err != nil {
		return err
	}
	maxGlobal, maxTenant := s.capacityCaps()
	if global+spec.MaxSize() > maxGlobal || forTenant+spec.MaxSize() > maxTenant {
		return status.Errorf(codes.ResourceExhausted, "sandbox capacity reached: pool needs %d of %d free", spec.MaxSize(), min(maxGlobal-global, maxTenant-forTenant))
	}
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

// ClosePools stops CreatePool and every pool's background work, then flushes
// pool events. It destroys nothing: the next start's Reconcile owns what is left.
// It runs before the backends close, and it is bounded: a backend call that
// ignores its deadline holds a pool's Close, not the shutdown.
func (s *Server) ClosePools() {
	s.createMu.Lock()
	s.pools.mu.Lock()
	s.pools.closed = true
	s.pools.mu.Unlock()
	s.createMu.Unlock()

	var closing sync.WaitGroup
	for _, e := range s.pools.all() {
		closing.Go(e.pool.Close)
	}
	closed := make(chan struct{})
	go func() {
		closing.Wait()
		close(closed)
	}()
	timer := time.NewTimer(s.poolCloseTimeout)
	defer timer.Stop()
	select {
	case <-closed:
	case <-timer.C:
		s.logger.Warn("pools did not stop in time; leaving what is left to the next start's reconcile")
	}
	s.events.flush()
}

// poolSamples is the scrape-time source of the pool state metrics.
func (s *Server) poolSamples() []telemetry.PoolSample {
	entries := s.pools.all()
	refused := s.pools.refused()
	out := make([]telemetry.PoolSample, 0, len(entries)+len(refused))
	for _, n := range refused {
		out = append(out, telemetry.PoolSample{Pool: n.rec.Spec.TenantID + "/" + n.rec.Spec.Name, NotLoaded: n.reason})
	}
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
	if n := s.pools.notLoadedPool(pool.ID(id)); n != nil && n.rec.Spec.TenantID == tenant {
		return nil, errPoolNotLoaded{n}
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
	// Before Parse, which would refuse it for the same reason the load did and
	// hide that the pool exists. A loaded pool holds the name over a duplicate
	// record, and the set only shrinks once pools are loaded.
	if s.pools.byName(raw.TenantID, raw.Name) == nil {
		if n := s.pools.notLoadedByName(raw.TenantID, raw.Name); n != nil {
			return nil, errPoolNotLoaded{n}
		}
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
	if s.pools.isClosed() {
		return nil, status.Error(codes.Unavailable, "the runner is shutting down")
	}
	if existing := s.pools.byName(spec.TenantID(), spec.Name()); existing != nil {
		switch {
		case existing.draining.Load():
			return nil, status.Errorf(codes.FailedPrecondition, "pool %s is draining; retry once it is gone", spec.Name())
		case sameDefinition(existing.spec, spec):
			return s.poolToProto(existing), nil
		default:
			return nil, status.Errorf(codes.AlreadyExists, "pool %s exists with a different spec", spec.Name())
		}
	}
	if err := s.checkPoolCapacity(spec); err != nil {
		return nil, err
	}
	rec := poolRecord{ID: spec.ID(), Spec: spec.Raw(), CreatedAt: time.Now().UTC()}
	if err := s.savePool(rec); err != nil {
		s.logger.Error("persist pool failed", "pool_id", spec.ID(), "error", err)
		return nil, status.Error(codes.Internal, "failed to persist pool")
	}
	e := s.startPool(spec, rec.CreatedAt)
	s.pools.add(e)
	s.auditPool(spec.Raw(), req.GetActor().GetKeyId(), "pool_create", spec.Name())
	s.logger.Info("pool declared", "pool_id", spec.ID(), "name", spec.Name(), "tenant_id", spec.TenantID(), "key_id", req.GetActor().GetKeyId(),
		"backend", raw.Template.Backend, "size", spec.Size(), "max_size", spec.MaxSize(), "reuse", spec.Reuse().String())
	return s.poolToProto(e), nil
}

// sameDefinition reports whether two declares mean the same pool. poolSpecToProto
// fills in defaults, so a spelled-out default compares equal, but its sandbox has
// no env, as specToProto leaves env out to keep secrets out of GetSandbox.
func sameDefinition(a, b pool.Spec) bool {
	return proto.Equal(poolSpecToProto(a), poolSpecToProto(b)) && maps.Equal(a.Raw().Template.Env, b.Raw().Template.Env)
}

// GetPool returns a pool's spec, state and live stats.
func (s *Server) GetPool(ctx context.Context, req *v1.GetPoolRequest) (*v1.Pool, error) {
	e, err := s.lookupPool(req.GetPoolId(), req.GetTenantId())
	var notLoaded errPoolNotLoaded
	if errors.As(err, &notLoaded) {
		return s.notLoadedToProto(notLoaded.pool), nil
	}
	if err != nil {
		return nil, err
	}
	return s.poolToProto(e), nil
}

// DeletePool drains the pool for up to wait_ms and reports whether every
// member is gone. While busy members finish, the pool stays registered as
// draining and the caller repeats the call.
func (s *Server) DeletePool(ctx context.Context, req *v1.DeletePoolRequest) (*v1.DeletePoolResponse, error) {
	e, err := s.lookupPool(req.GetPoolId(), req.GetTenantId())
	var notLoaded errPoolNotLoaded
	if errors.As(err, &notLoaded) {
		return s.deleteNotLoaded(notLoaded.pool, req.GetActor().GetKeyId())
	}
	if err != nil {
		return nil, err
	}
	if req.GetWaitMs() < 0 {
		return nil, status.Error(codes.InvalidArgument, "wait_ms must not be negative")
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(req.GetWaitMs())*time.Millisecond)
	defer cancel()
	if !e.draining.Swap(true) {
		rec := poolRecord{ID: e.spec.ID(), Spec: e.spec.Raw(), CreatedAt: e.createdAt, Draining: true}
		if err := s.savePool(rec); err != nil {
			s.logger.Warn("persist pool draining failed", "pool_id", e.spec.ID(), "error", err)
		}
		s.auditPool(e.spec.Raw(), req.GetActor().GetKeyId(), "pool_drain", e.spec.Name())
		s.logger.Info("pool draining", "pool_id", e.spec.ID(), "name", e.spec.Name(), "key_id", req.GetActor().GetKeyId())
	}
	if !e.pool.Drain(ctx) {
		return &v1.DeletePoolResponse{Drained: false}, nil
	}
	e.pool.Close()
	s.createMu.Lock()
	s.pools.remove(e.spec.ID())
	s.createMu.Unlock()
	s.metrics.DeletePoolSeries(e.label())
	if err := s.deletePoolRecord(e.spec.ID()); err != nil {
		s.logger.Warn("delete pool record failed", "pool_id", e.spec.ID(), "error", err)
	}
	s.auditPool(e.spec.Raw(), req.GetActor().GetKeyId(), "pool_delete", e.spec.Name())
	s.logger.Info("pool deleted", "pool_id", e.spec.ID(), "name", e.spec.Name(), "key_id", req.GetActor().GetKeyId())
	return &v1.DeletePoolResponse{Drained: true}, nil
}

// GetPoolEvents returns a pool's own audit trail. It answers from the trail, not
// the registry, because the event that matters most, pool_delete, is written once
// the pool is gone. Each row names its tenant, so a pool of another tenant reads
// as not found, and these rows hold pool names, not a member's raw backend text.
func (s *Server) GetPoolEvents(ctx context.Context, req *v1.GetPoolEventsRequest) (*v1.GetSandboxEventsResponse, error) {
	if !validPoolID(req.GetPoolId()) {
		return nil, status.Error(codes.InvalidArgument, "invalid pool_id")
	}
	if req.GetTenantId() == "" {
		return nil, status.Error(codes.InvalidArgument, "tenant_id is required")
	}
	events, err := s.audit.Events(ctx, req.GetPoolId())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "read events: %v", err)
	}
	events = slices.DeleteFunc(events, func(e sandbox.Event) bool { return e.Actor.TenantID != req.GetTenantId() })
	if len(events) == 0 {
		return nil, status.Errorf(codes.NotFound, "pool %s not found", req.GetPoolId())
	}
	return &v1.GetSandboxEventsResponse{Events: eventsToProto(events)}, nil
}

// auditPool puts who declared or deleted a pool on the audit trail, under the
// pool's id. A pool has no workspace, so the local store's directory is made here;
// the Postgres store takes any id.
func (s *Server) auditPool(raw pool.RawSpec, keyID, kind, detail string) {
	if s.localAudit {
		if err := os.MkdirAll(s.workspaces.LogsDir(string(raw.ID)), 0o755); err != nil {
			s.logger.Warn("create pool audit directory failed", "pool_id", raw.ID, "error", err)
		}
	}
	s.events.append(sandbox.Event{
		SandboxID: string(raw.ID),
		Actor:     actorFor(runtime.SandboxSpec{TenantID: raw.TenantID, AgentID: raw.AgentID}, keyID),
		Reason:    kind + ": " + detail,
		Kind:      kind,
		Detail:    detail,
		RunnerID:  s.runnerID,
	})
}

// deleteNotLoaded removes a refused pool's record. It has no members to drain.
func (s *Server) deleteNotLoaded(n *notLoadedPool, keyID string) (*v1.DeletePoolResponse, error) {
	s.createMu.Lock()
	defer s.createMu.Unlock()
	if err := s.deletePoolRecord(n.rec.ID); err != nil {
		s.logger.Error("delete pool record failed", "pool_id", n.rec.ID, "error", err)
		return nil, status.Error(codes.Internal, "failed to delete pool")
	}
	s.pools.forget(n.rec.ID)
	s.auditPool(n.rec.Spec, keyID, "pool_delete", "not loaded")
	s.logger.Info("pool deleted without having been loaded", "pool_id", n.rec.ID, "name", n.rec.Spec.Name, "key_id", keyID)
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
	if err := validateRunExec(req.GetExec(), e.spec.MaxRun()); err != nil {
		return status.Error(codes.InvalidArgument, err.Error())
	}
	execReq := execRequestFromProto(req.GetExec())
	keyID := req.GetActor().GetKeyId()

	sender := &runSender{stream: stream}
	var (
		placement pool.Placement
		reason    runtime.ExitReason
		execStart time.Time
		started   bool
	)
	done, err := e.pool.Run(stream.Context(), func(ctx context.Context, pl pool.Placement) error {
		placement = pl
		backend, h, err := s.handle(pl.SandboxID)
		if err != nil {
			return fmt.Errorf("%w: %w", pool.ErrNotStarted, err)
		}
		hard, cancel := context.WithTimeout(context.Background(), execReq.Timeout+s.execGrace)
		defer cancel()
		sender.hard = hard
		execStart = time.Now()
		reason, err = s.runExec(ctx, pl.SandboxID, backend, h, execReq, keyID, execOptions{
			poolID:     string(e.spec.ID()),
			sandboxRun: pl.SandboxRun,
			hard:       hard.Done(),
			beforeFirst: func(executionID string) error {
				started = true
				return sender.send(runFrameToProto(e.spec.ID(), pl, executionID))
			},
		}, func(c *v1.ExecChunk) error { return sender.send(execToRunChunk(c)) })
		if err != nil && !started {
			// A client that left says nothing about the member, as in pool.admit:
			// it goes through its reset and stays.
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("%w: %w", pool.ErrNotStarted, err)
		}
		return err
	})
	if started {
		s.observeRun(e, placement, reason, time.Since(execStart), done)
	}
	if err != nil {
		if stream.Context().Err() == nil {
			s.logger.Warn("pool run failed", "pool_id", e.spec.ID(), "sandbox_id", placement.SandboxID, "error", err)
		}
		return poolStatus(err)
	}
	return sender.send(runDoneToProto(done))
}

// Limits on what an exec passes to a process. runsc takes the command, args and
// env on its own argv, and exec fails with E2BIG past these (MAX_ARG_STRLEN, and
// well inside the default ARG_MAX), which would read as a broken member.
const (
	maxExecStringBytes = 128<<10 - 1
	maxExecArgvBytes   = 1 << 20
)

// validateRunExec refuses what a backend would refuse for the request's own
// content. A refused exec reads as a broken member and retires it, so this runs
// before a member is borrowed and a bad request costs none.
func validateRunExec(exec *v1.ExecRequest, maxRun time.Duration) error {
	if exec.GetExecutionId() != "" {
		return errors.New("execution_id is assigned by the runner")
	}
	if exec.GetCommand() == "" {
		return errors.New("command is required")
	}
	timeout, err := secondsFromProto("timeout_seconds", exec.GetTimeoutSeconds())
	switch {
	case err != nil:
		return err
	case timeout == 0:
		return errors.New("timeout_seconds is required")
	case timeout > maxRun:
		return fmt.Errorf("timeout_seconds exceeds the pool's max_run of %s", maxRun)
	}
	total := 0
	for _, list := range [][]string{{exec.GetCommand(), exec.GetCwd()}, exec.GetArgs(), exec.GetEnv()} {
		for _, v := range list {
			total += len(v)
			switch {
			case strings.IndexByte(v, 0) >= 0:
				return errors.New("command, args, cwd and env must not contain a NUL byte")
			case len(v) > maxExecStringBytes:
				return fmt.Errorf("a command, arg, cwd or env entry exceeds %d bytes", maxExecStringBytes)
			}
		}
	}
	if total > maxExecArgvBytes {
		return fmt.Errorf("command, args, cwd and env exceed %d bytes", maxExecArgvBytes)
	}
	return nil
}

// errClientStalled ends a run whose client stopped reading.
var errClientStalled = status.Error(codes.DeadlineExceeded, "client stopped reading the run's output")

// sendGrace is how long a frame may wait for the client once the run's hard
// deadline has passed: the run is over, and only its trailing frames are left.
const sendGrace = time.Second

// runSender sends a run's frames without letting a client that stopped reading
// hold the member. stream.Send blocks on flow control for as long as the client
// stays connected, so each send runs on its own goroutine and the handler waits
// only until the hard deadline. A stalled Send returns when the handler does,
// since gRPC ends the stream then. One goroutine at a time calls send, and a
// frame goes out only after the one before it returned, so Sends never overlap.
type runSender struct {
	stream grpc.ServerStreamingServer[v1.RunChunk]
	// hard is the current attempt's deadline.
	hard context.Context
	// pending is the Send still in flight, if any.
	pending chan error
}

func (r *runSender) send(c *v1.RunChunk) error {
	limit := r.hard
	if limit == nil || limit.Err() != nil {
		grace, cancel := context.WithTimeout(context.Background(), sendGrace)
		defer cancel()
		limit = grace
	}
	if err := r.wait(limit); err != nil {
		return err
	}
	r.pending = make(chan error, 1)
	go func(done chan<- error) { done <- r.stream.Send(c) }(r.pending)
	return r.wait(limit)
}

func (r *runSender) wait(limit context.Context) error {
	if r.pending == nil {
		return nil
	}
	select {
	case err := <-r.pending:
		r.pending = nil
		return err
	case <-limit.Done():
		return errClientStalled
	}
}

// observeRun records one run whose program started. It also observes
// vora_exec_duration_seconds, as a pooled run already counts in the other
// vora_exec_* metrics and a histogram missing pool traffic would disagree with them.
func (s *Server) observeRun(e *poolEntry, pl pool.Placement, reason runtime.ExitReason, took time.Duration, done pool.Done) {
	start := "cold"
	if pl.Warm {
		start = "warm"
	}
	s.metrics.ExecDuration.Observe(took.Seconds())
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
	// DeletePool can finish as soon as this run's member is destroyed, so its
	// delete of the series may have come before the observes above.
	if s.pools.get(e.spec.ID()) == nil {
		s.metrics.DeletePoolSeries(label)
	}
}

// poolSandboxes is the runner side of pool.Sandboxes: one member is one
// ordinary sandbox record with a PoolID, provisioned through the same
// createRecord and startRecord as a plain sandbox. Capacity is not checked per
// member because the pool reserved it, and committed skips a loaded pool's
// records, so a member start takes no createMu and never serialises other creates.
type poolSandboxes struct {
	s     *Server
	actor sandbox.Actor
}

func (a poolSandboxes) Start(ctx context.Context, spec runtime.SandboxSpec, prepare bool) (started pool.Started, err error) {
	s := a.s
	spec.ID = ulid.NewSandbox()
	// What a tenant sees of a failed start is a fixed word (poolStatus, and
	// poolStatsToProto for last_start_error), so the log is where the detail lives.
	defer func() {
		if err != nil {
			s.logger.Warn("pool member failed to start", "sandbox_id", spec.ID, "pool_id", spec.PoolID, "prepare", prepare, "error", err)
		}
	}()
	if err := s.enforceResourceLimits(&spec); err != nil {
		return pool.Started{}, status.Error(codes.InvalidArgument, err.Error())
	}
	rec, err := s.createRecord(ctx, spec)
	if err != nil {
		a.undo(ctx, spec.ID)
		return pool.Started{}, err
	}
	if err := s.startRecord(ctx, rec, ""); err != nil {
		a.undo(ctx, rec.ID)
		return pool.Started{}, err
	}
	started.SandboxID = rec.ID
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
		a.undo(ctx, rec.ID)
		return pool.Started{}, fmt.Errorf("prepare: %w", err)
	}
	started.Reset = reset
	return started, nil
}

// undo removes whatever a failed Start left behind. It runs on a context detached
// from ctx, as a cold start runs on the client's context and a client that
// disconnected mid-start must not leak a half-started sandbox, and under its own
// deadline, as a backend that never answers must not hold the start's slot.
func (a poolSandboxes) undo(ctx context.Context, id string) {
	dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), memberUndoTimeout)
	defer cancel()
	err := a.s.destroy(dctx, id, "")
	if err != nil && status.Code(err) != codes.NotFound {
		a.s.logger.Warn("undo pool member start failed", "sandbox_id", id, "error", err)
	}
}

// memberUndoTimeout matches the pool's default DestroyTimeout.
const memberUndoTimeout = 30 * time.Second

// Destroy is idempotent, as pool.Sandboxes needs: a record the audit pruner
// already removed is a member that is gone, not a destroy to retry for ever.
func (a poolSandboxes) Destroy(ctx context.Context, sandboxID string) error {
	err := a.s.destroy(ctx, sandboxID, "")
	if status.Code(err) == codes.NotFound {
		return nil
	}
	return err
}

// Event records a pool event on the member's trail. Reason repeats kind and
// detail because reason is all the wire's SandboxEvent carries, and because a
// row written before the store had those columns has nothing else.
func (a poolSandboxes) Event(sandboxID, kind, detail string) {
	a.s.events.append(sandbox.Event{
		SandboxID: sandboxID,
		Actor:     a.actor,
		From:      sandbox.StateRunning,
		To:        sandbox.StateRunning,
		Reason:    kind + ": " + detail,
		Kind:      kind,
		Detail:    detail,
		RunnerID:  a.s.runnerID,
	})
}
