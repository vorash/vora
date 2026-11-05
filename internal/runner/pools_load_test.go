package runner

import (
	"context"
	"os"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	v1 "github.com/vorash/vora/api/proto/v1"
	"github.com/vorash/vora/internal/config"
	"github.com/vorash/vora/internal/pool"
	"github.com/vorash/vora/internal/runtime"
	"github.com/vorash/vora/internal/sandbox"
	"github.com/vorash/vora/internal/ulid"
)

// restart stops h's runner and boots a new one over the same state. change edits
// its config, and backend is what it registers, none if nil.
func (h *poolHarness) restart(change func(*config.Config), backend runtime.Backend) *poolHarness {
	h.t.Helper()
	h.s.ClosePools()
	cfg := h.s.cfg
	if change != nil {
		change(&cfg)
	}
	reg := runtime.NewRegistry()
	next := &poolHarness{t: h.t, tenant: h.tenant}
	if backend != nil {
		if err := reg.Register(backend); err != nil {
			h.t.Fatal(err)
		}
		next.backend = backend.(reusableBackend).poolBackend
	}
	next.s = NewServer(cfg, sandbox.NewStore(cfg.SandboxesDir()), reg, "run_next", h.s.logger)
	tunePoolServer(h.t, next.s)
	if err := next.s.Reconcile(context.Background()); err != nil {
		h.t.Fatal(err)
	}
	if err := next.s.LoadPools(); err != nil {
		h.t.Fatal(err)
	}
	next.client = serveBufconn(h.t, next.s)
	return next
}

func (h *poolHarness) wantNotLoaded(id, reason string) {
	h.t.Helper()
	p := h.get(id)
	if p.GetState() != v1.PoolState_POOL_NOT_LOADED || p.GetNotLoadedReason() != reason {
		h.t.Fatalf("pool %s = %s (%q), want POOL_NOT_LOADED (%q)", id, p.GetState(), p.GetNotLoadedReason(), reason)
	}
	if _, err := h.run(h.ctx(), id, 5); status.Code(err) != codes.FailedPrecondition {
		h.t.Fatalf("run on a pool not loaded: %v, want FailedPrecondition", err)
	}
	f := h.metricFamily("vora_pool_not_loaded")
	for _, m := range f.GetMetric() {
		labels := map[string]string{}
		for _, l := range m.GetLabel() {
			labels[l.GetName()] = l.GetValue()
		}
		if labels["pool"] == p.GetSpec().GetTenantId()+"/"+p.GetSpec().GetName() && labels["reason"] == reason && m.GetGauge().GetValue() == 1 {
			return
		}
	}
	h.t.Fatalf("vora_pool_not_loaded has no series for %s: %v", id, f)
}

func (h *poolHarness) records() []poolRecord {
	h.t.Helper()
	recs, err := h.s.loadPoolRecords()
	if err != nil {
		h.t.Fatal(err)
	}
	return recs
}

// A backend that fails to register once must not cost a tenant its pool for good.
func TestBootWithABackendMissingKeepsThePool(t *testing.T) {
	h := newWarmHarness(t)
	p := h.create(poolSpec(1, 1))
	h.waitIdle(p.GetId(), 1)

	down := h.restart(nil, nil)
	down.wantNotLoaded(p.GetId(), notLoadedBackendMissing)
	if _, err := down.client.CreatePool(down.ctx(), &v1.CreatePoolRequest{Spec: poolSpec(1, 1)}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("redeclare of a pool not loaded: %v, want FailedPrecondition, not a second pool", err)
	}
	if got := down.records(); len(got) != 1 {
		t.Fatalf("records = %d, want the one definition", len(got))
	}
	if g, ft, _ := down.s.committed(testTenant); g != 0 || ft != 0 {
		t.Fatalf("a pool not loaded holds capacity: %d, %d", g, ft)
	}

	back := down.restart(nil, reusableBackend{newPoolBackend(t)})
	reloaded := back.get(p.GetId())
	if reloaded.GetState() != v1.PoolState_POOL_ACTIVE {
		t.Fatalf("pool on the boot with its backend back = %s, want POOL_ACTIVE", reloaded.GetState())
	}
	back.waitIdle(p.GetId(), 1)
	if f := back.metricFamily("vora_pool_not_loaded"); f != nil {
		t.Fatalf("vora_pool_not_loaded = %v, want no series once it loaded", f.GetMetric())
	}
}

// A pool declared under the unsafe grant has no business resetting its way around
// losing it: it runs nothing at all until the grant is back, and then unsafely.
func TestPoolWithoutItsUnsafeGrantRefusesToRun(t *testing.T) {
	h := newWarmHarness(t)
	h.s.cfg.PoolUnsafeReuse = true
	spec := poolSpec(1, 1)
	spec.Reuse = v1.PoolReuse_POOL_REUSE_UNSAFE_NO_RESET
	p := h.create(spec)
	h.waitIdle(p.GetId(), 1)

	ungranted := reusableBackend{newPoolBackend(t)}
	down := h.restart(func(c *config.Config) { c.PoolUnsafeReuse = false }, ungranted)
	down.wantNotLoaded(p.GetId(), notLoadedUnsafeRefused)
	if live := ungranted.live(); len(live) != 0 {
		t.Fatalf("a pool without its grant started members: %v", live)
	}

	back := down.restart(func(c *config.Config) { c.PoolUnsafeReuse = true }, reusableBackend{newPoolBackend(t)})
	back.waitIdle(p.GetId(), 1)
	frames, err := back.run(back.ctx(), p.GetId(), 5)
	if err != nil {
		t.Fatal(err)
	}
	if frames[0].GetRun().GetReuse() != v1.PoolReuse_POOL_REUSE_UNSAFE_NO_RESET {
		t.Fatalf("run = %v, want the unsafe mode it was declared with", frames[0].GetRun())
	}
}

// Caps lowered across a restart: three pools of 4 under a cap of 4 used to start
// 12 members. The oldest fits, the others wait, and no member of theirs starts.
func TestLoadChecksTheSumOfReservationsAgainstTheCaps(t *testing.T) {
	h := newWarmHarness(t)
	h.s.cfg.MaxSandboxes, h.s.cfg.MaxSandboxesPerTenant = 64, 64
	var ids []string
	for _, name := range []string{"first", "second", "third"} {
		spec := poolSpec(4, 4)
		spec.Name = name
		ids = append(ids, h.create(spec).GetId())
		time.Sleep(2 * time.Millisecond) // created_at decides who fits
	}

	lowered := reusableBackend{newPoolBackend(t)}
	low := h.restart(func(c *config.Config) { c.MaxSandboxes, c.MaxSandboxesPerTenant = 64, 4 }, lowered)
	low.waitIdle(ids[0], 4)
	low.wantNotLoaded(ids[1], notLoadedCapacity)
	low.wantNotLoaded(ids[2], notLoadedCapacity)
	if g, ft, _ := low.s.committed(testTenant); g != 4 || ft != 4 {
		t.Fatalf("committed = %d, %d; want 4 under the cap", g, ft)
	}
	time.Sleep(50 * time.Millisecond)
	if live := lowered.live(); len(live) != 4 {
		t.Fatalf("%d members running on a runner capped at 4", len(live))
	}
	if _, err := low.client.CreateSandbox(low.ctx(), &v1.CreateSandboxRequest{Spec: plainSpec(testTenant)}); status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("plain create at the cap: %v, want ResourceExhausted", err)
	}

	// The same through the global cap, with the tenant's own to spare.
	global := low.restart(func(c *config.Config) { c.MaxSandboxes, c.MaxSandboxesPerTenant = 4, 64 }, reusableBackend{newPoolBackend(t)})
	global.waitIdle(ids[0], 4)
	global.wantNotLoaded(ids[1], notLoadedCapacity)

	raised := global.restart(func(c *config.Config) { c.MaxSandboxes, c.MaxSandboxesPerTenant = 64, 64 }, reusableBackend{newPoolBackend(t)})
	for _, id := range ids {
		raised.waitIdle(id, 4)
	}
}

// Two records for one (tenant, name) can only come from a damaged state dir. The
// older keeps the name, as it is the pool the control plane has a route to.
func TestLoadRefusesTheLaterOfTwoPoolsWithOneName(t *testing.T) {
	h := newWarmHarness(t)
	p := h.create(poolSpec(0, 1))
	dup := h.s.pools.get(pool.ID(p.GetId())).spec.Raw()
	dup.ID = pool.ID(ulid.NewPool())
	if err := h.s.savePool(poolRecord{ID: dup.ID, Spec: dup, CreatedAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}

	next := h.restart(nil, reusableBackend{newPoolBackend(t)})
	if got := next.get(p.GetId()).GetState(); got != v1.PoolState_POOL_ACTIVE {
		t.Fatalf("older pool = %s, want POOL_ACTIVE", got)
	}
	next.wantNotLoaded(string(dup.ID), notLoadedDuplicate)
	if q := next.create(poolSpec(0, 1)); q.GetId() != p.GetId() {
		t.Fatalf("redeclare = %s, want the loaded pool %s", q.GetId(), p.GetId())
	}
}

// With nothing to drain, a delete is how a tenant gives up on a pool that will
// never load here. It also frees the name.
func TestDeletePoolThatIsNotLoaded(t *testing.T) {
	h := newWarmHarness(t)
	p := h.create(poolSpec(0, 1))
	down := h.restart(nil, nil)
	req := &v1.DeletePoolRequest{PoolId: p.GetId(), TenantId: "tenant-b"}
	if _, err := down.client.DeletePool(down.ctx(), req); status.Code(err) != codes.NotFound {
		t.Fatalf("delete from another tenant: %v, want NotFound", err)
	}
	if _, err := down.client.GetPool(down.ctx(), &v1.GetPoolRequest{PoolId: p.GetId(), TenantId: "tenant-b"}); status.Code(err) != codes.NotFound {
		t.Fatalf("get from another tenant: %v, want NotFound", err)
	}
	req.TenantId = testTenant
	resp, err := down.client.DeletePool(down.ctx(), req)
	if err != nil || !resp.GetDrained() {
		t.Fatalf("DeletePool = %v, %v; want drained", resp, err)
	}
	if got := down.records(); len(got) != 0 {
		t.Fatalf("records after the delete = %d", len(got))
	}
	if _, err := down.client.GetPool(down.ctx(), &v1.GetPoolRequest{PoolId: p.GetId(), TenantId: testTenant}); status.Code(err) != codes.NotFound {
		t.Fatalf("get after the delete: %v, want NotFound", err)
	}
}

// main serves before Reconcile and LoadPools have run. Until then the capacity
// books lack every reservation, so a plain create could take a pool's slots.
func TestCreatesAreRefusedUntilPoolsAreLoaded(t *testing.T) {
	h := newWarmHarness(t)
	snap := h.snapshot()
	reg := runtime.NewRegistry()
	if err := reg.Register(reusableBackend{newPoolBackend(t)}); err != nil {
		t.Fatal(err)
	}
	s := NewServer(h.s.cfg, sandbox.NewStore(h.s.cfg.SandboxesDir()), reg, "run_next", h.s.logger)
	tunePoolServer(t, s)
	ctx := context.Background()
	if _, err := s.CreatePool(ctx, &v1.CreatePoolRequest{Spec: poolSpec(0, 1)}); status.Code(err) != codes.Unavailable {
		t.Fatalf("CreatePool before load: %v, want Unavailable", err)
	}
	if _, err := s.CreateSandbox(ctx, &v1.CreateSandboxRequest{Spec: plainSpec(testTenant)}); status.Code(err) != codes.Unavailable {
		t.Fatalf("CreateSandbox before load: %v, want Unavailable", err)
	}
	if _, err := s.ForkSnapshot(ctx, &v1.ForkSnapshotRequest{SnapshotId: snap}); status.Code(err) != codes.Unavailable {
		t.Fatalf("ForkSnapshot before load: %v, want Unavailable", err)
	}
	if err := s.LoadPools(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreatePool(ctx, &v1.CreatePoolRequest{Spec: poolSpec(0, 1)}); err != nil {
		t.Fatalf("CreatePool after load: %v", err)
	}
	if _, err := s.CreateSandbox(ctx, &v1.CreateSandboxRequest{Spec: plainSpec(testTenant)}); err != nil {
		t.Fatalf("CreateSandbox after load: %v", err)
	}
}

// A state dir that cannot be read is not "no pools": creates stay refused.
func TestUnreadablePoolsDirLeavesCreatesRefused(t *testing.T) {
	h := newWarmHarness(t)
	s := NewServer(h.s.cfg, sandbox.NewStore(h.s.cfg.SandboxesDir()), runtime.NewRegistry(), "run_next", h.s.logger)
	if err := os.RemoveAll(s.cfg.PoolsDir()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.cfg.PoolsDir(), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.LoadPools(); err == nil {
		t.Fatal("LoadPools over an unreadable pools dir succeeded")
	}
	if _, err := s.CreateSandbox(context.Background(), &v1.CreateSandboxRequest{Spec: plainSpec(testTenant)}); status.Code(err) != codes.Unavailable {
		t.Fatalf("CreateSandbox after a failed load: %v, want Unavailable", err)
	}
}

// A draining record is the one thing a load deletes.
func TestLoadRemovesARecordThatWasDraining(t *testing.T) {
	h := newWarmHarness(t)
	p := h.create(poolSpec(0, 1))
	raw := h.s.pools.get(pool.ID(p.GetId())).spec.Raw()
	if err := h.s.savePool(poolRecord{ID: raw.ID, Spec: raw, CreatedAt: time.Now(), Draining: true}); err != nil {
		t.Fatal(err)
	}
	for _, backend := range []runtime.Backend{nil, reusableBackend{newPoolBackend(t)}} {
		next := h.restart(nil, backend)
		if got := next.records(); len(got) != 0 {
			t.Fatalf("records = %v, want the draining one removed", got)
		}
		if _, err := next.client.GetPool(next.ctx(), &v1.GetPoolRequest{PoolId: p.GetId(), TenantId: testTenant}); status.Code(err) != codes.NotFound {
			t.Fatalf("pool that was draining: %v, want NotFound", err)
		}
		h = next
	}
}
