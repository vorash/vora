package runner

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	v1 "github.com/vorash/vora/api/proto/v1"
	"github.com/vorash/vora/internal/pool"
	"github.com/vorash/vora/internal/runtime"
	"github.com/vorash/vora/internal/sandbox"
)

// Members inherit the template's env, so a different env is a different pool.
// specToProto leaves env out to keep secrets out of GetSandbox, which hid this.
func TestRedeclareWithADifferentEnvIsADifferentPool(t *testing.T) {
	h := newWarmHarness(t)
	declare := func(env ...string) *v1.PoolSpec {
		spec := poolSpec(0, 1)
		spec.Sandbox.Env = env
		return spec
	}
	p := h.create(declare("MODEL=v1", "TOKEN=hunter2"))
	if q := h.create(declare("TOKEN=hunter2", "MODEL=v1")); q.GetId() != p.GetId() {
		t.Fatalf("the same env in another order gave %s, want %s", q.GetId(), p.GetId())
	}
	_, err := h.client.CreatePool(h.ctx(), &v1.CreatePoolRequest{Spec: declare("MODEL=v2", "TOKEN=rotated")})
	if status.Code(err) != codes.AlreadyExists {
		t.Fatalf("different env: %v, want AlreadyExists", err)
	}
	if strings.Contains(err.Error(), "hunter2") || strings.Contains(err.Error(), "rotated") {
		t.Fatalf("the refusal quotes a secret: %v", err)
	}
	if env := h.get(p.GetId()).GetSpec().GetSandbox().GetEnv(); len(env) != 0 {
		t.Fatalf("GetPool returned the env: %v", env)
	}
	// The comparison holds against a definition read back from disk.
	next := h.restart(nil, reusableBackend{newPoolBackend(t)})
	if q := next.create(declare("MODEL=v1", "TOKEN=hunter2")); q.GetId() != p.GetId() {
		t.Fatalf("same env after a restart gave %s, want %s", q.GetId(), p.GetId())
	}
	if _, err := next.client.CreatePool(next.ctx(), &v1.CreatePoolRequest{Spec: declare("MODEL=v2")}); status.Code(err) != codes.AlreadyExists {
		t.Fatalf("different env after a restart: %v, want AlreadyExists", err)
	}
}

// Only a pool starts its members, so a member stranded before CREATING by a crash
// has nobody left: DestroySandbox refuses it and the reaper skips it, and it would
// hold a slot for ever. A plain REQUESTED record is waiting for its StartSandbox.
func TestReconcileFailsAMemberStrandedBeforeCreating(t *testing.T) {
	h := newWarmHarness(t)
	const poolID = "pool_01ARZ3NDEKTSV4RRFFQ69G5FAV"
	stranded := map[string]sandbox.State{
		"sbx_01ARZ3NDEKTSV4RRFFQ69G5FAA": sandbox.StateRequested,
		"sbx_01ARZ3NDEKTSV4RRFFQ69G5FAB": sandbox.StateScheduling,
	}
	for id, state := range stranded {
		rec := &sandbox.Record{ID: id, State: state, Spec: runtime.SandboxSpec{ID: id, TenantID: h.tenant, PoolID: poolID}}
		if err := h.s.store.Create(rec); err != nil {
			t.Fatal(err)
		}
	}
	const plain = "sbx_01ARZ3NDEKTSV4RRFFQ69G5FAC"
	if err := h.s.store.Create(&sandbox.Record{ID: plain, State: sandbox.StateRequested, Spec: runtime.SandboxSpec{ID: plain, TenantID: h.tenant}}); err != nil {
		t.Fatal(err)
	}
	if _, ft, _ := h.s.committed(h.tenant); ft != 3 {
		t.Fatalf("committed before reconcile = %d, want 3", ft)
	}
	if err := h.s.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	for id := range stranded {
		if rec, _ := h.s.store.Load(id); rec.State != sandbox.StateFailed {
			t.Errorf("%s = %s, want FAILED", id, rec.State)
		}
	}
	if rec, _ := h.s.store.Load(plain); rec.State != sandbox.StateRequested {
		t.Errorf("plain record = %s, want it left REQUESTED", rec.State)
	}
	if _, ft, _ := h.s.committed(h.tenant); ft != 1 {
		t.Fatalf("committed after reconcile = %d, want only the plain record", ft)
	}
}

func poolLabelled(h *poolHarness, name, label string) int {
	h.t.Helper()
	f := h.metricFamily(name)
	n := 0
	for _, m := range f.GetMetric() {
		for _, l := range m.GetLabel() {
			if l.GetName() == "pool" && l.GetValue() == label {
				n++
			}
		}
	}
	return n
}

// A tenant that keeps declaring and deleting pools under new names must not grow
// the registry for ever.
func TestDeletePoolRemovesItsMetricSeries(t *testing.T) {
	h := newWarmHarness(t)
	other := poolSpec(1, 1)
	other.Name = "other"
	kept := h.create(other)
	h.waitIdle(kept.GetId(), 1)
	gone := h.create(poolSpec(1, 1))
	h.waitIdle(gone.GetId(), 1)
	for _, id := range []string{kept.GetId(), gone.GetId()} {
		if _, err := h.run(h.ctx(), id, 5); err != nil {
			t.Fatal(err)
		}
	}
	perRun := []string{"vora_pool_runs_total", "vora_pool_run_duration_seconds", "vora_pool_startup_seconds", "vora_pool_reset_duration_seconds"}
	for _, name := range perRun {
		if poolLabelled(h, name, testTenant+"/py") == 0 {
			t.Fatalf("%s has no series for the pool before the delete", name)
		}
	}
	resp, err := h.client.DeletePool(h.ctx(), &v1.DeletePoolRequest{PoolId: gone.GetId(), TenantId: h.tenant, WaitMs: 5000})
	if err != nil || !resp.GetDrained() {
		t.Fatalf("DeletePool = %v, %v", resp, err)
	}
	for _, name := range append(perRun, "vora_pool_sandboxes") {
		if n := poolLabelled(h, name, testTenant+"/py"); n != 0 {
			t.Errorf("%s keeps %d series after the delete", name, n)
		}
		if poolLabelled(h, name, testTenant+"/other") == 0 {
			t.Errorf("%s lost the series of a pool that was not deleted", name)
		}
	}
}

// The trail says who declared and who deleted a pool. A redeclare changes nothing.
func TestPoolLifecycleIsOnTheAuditTrailWithItsActor(t *testing.T) {
	h := newWarmHarness(t)
	ctx := h.ctx()
	p, err := h.client.CreatePool(ctx, &v1.CreatePoolRequest{Spec: poolSpec(0, 1), Actor: &v1.Actor{KeyId: "key_declare"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.client.CreatePool(ctx, &v1.CreatePoolRequest{Spec: poolSpec(0, 1), Actor: &v1.Actor{KeyId: "key_again"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.client.DeletePool(ctx, &v1.DeletePoolRequest{PoolId: p.GetId(), TenantId: h.tenant, WaitMs: 5000, Actor: &v1.Actor{KeyId: "key_delete"}}); err != nil {
		t.Fatal(err)
	}
	h.s.events.flush()
	events, err := h.s.audit.Events(context.Background(), p.GetId())
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, ev := range events {
		if ev.Actor.TenantID != h.tenant {
			t.Errorf("%s has tenant %q", ev.Kind, ev.Actor.TenantID)
		}
		got = append(got, ev.Kind+":"+ev.Actor.KeyID)
	}
	want := "pool_create:key_declare pool_drain:key_delete pool_delete:key_delete"
	if strings.Join(got, " ") != want {
		t.Fatalf("events = %v, want %s", got, want)
	}
}

// Shutdown: backends close next, so nothing may start a pool now.
func TestClosePoolsStopsCreatePool(t *testing.T) {
	h := newWarmHarness(t)
	h.s.ClosePools()
	if _, err := h.client.CreatePool(h.ctx(), &v1.CreatePoolRequest{Spec: poolSpec(1, 1)}); status.Code(err) != codes.Unavailable {
		t.Fatalf("CreatePool after ClosePools: %v, want Unavailable", err)
	}
	if live := h.backend.live(); len(live) != 0 {
		t.Fatalf("members started after ClosePools: %v", live)
	}
}

// A backend call that ignores its deadline holds its pool's Close, not shutdown.
func TestClosePoolsIsBoundedWhenABackendNeverAnswers(t *testing.T) {
	h := newWarmHarness(t)
	release := make(chan struct{})
	defer close(release)
	h.s.poolCloseTimeout = 50 * time.Millisecond
	h.backend.set(func(b *poolBackend) {
		b.destroyHook = func(context.Context, string) { <-release }
	})
	p := h.create(poolSpec(1, 1))
	h.waitIdle(p.GetId(), 1)
	if _, err := h.client.DeletePool(h.ctx(), &v1.DeletePoolRequest{PoolId: p.GetId(), TenantId: h.tenant}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return h.get(p.GetId()).GetStats().GetRetiring() == 1 }, "the destroy to begin")
	done := make(chan struct{})
	go func() {
		h.s.ClosePools()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("ClosePools hung on a destroy that never returns")
	}
}

// DeletePool's draining mark is what stops a restart mid-delete from refilling a
// pool the tenant asked to remove, and its last step removes the record for good.
func TestDeletePoolPersistsDrainingThenRemovesItsRecord(t *testing.T) {
	h := newWarmHarness(t)
	release := make(chan struct{})
	h.backend.set(func(b *poolBackend) {
		b.execHook = func(ctx context.Context, _ string, _ runtime.ExecRequest) (runtime.ExecStream, error) {
			s := newCtxStream(ctx, false, runtime.ExecChunk{Exit: &runtime.ExecResult{Reason: runtime.ReasonExited}})
			s.gate = release
			return s, nil
		}
	})
	p := h.create(poolSpec(1, 1))
	h.waitIdle(p.GetId(), 1)
	ran := make(chan error, 1)
	go func() {
		_, err := h.run(h.ctx(), p.GetId(), 5)
		ran <- err
	}()
	waitFor(t, func() bool { return h.get(p.GetId()).GetStats().GetBusy() == 1 }, "the run to hold the member")

	req := &v1.DeletePoolRequest{PoolId: p.GetId(), TenantId: h.tenant}
	if resp, err := h.client.DeletePool(h.ctx(), req); err != nil || resp.GetDrained() {
		t.Fatalf("DeletePool mid-run = %v, %v; want drained=false", resp, err)
	}
	recs := h.records()
	if len(recs) != 1 || !recs[0].Draining {
		t.Fatalf("records mid-delete = %+v, want one marked draining", recs)
	}
	close(release)
	if err := <-ran; err != nil {
		t.Fatal(err)
	}
	req.WaitMs = 5000
	if resp, err := h.client.DeletePool(h.ctx(), req); err != nil || !resp.GetDrained() {
		t.Fatalf("second DeletePool = %v, %v; want drained", resp, err)
	}
	if recs := h.records(); len(recs) != 0 {
		t.Fatalf("records after the delete = %+v, want none", recs)
	}
}

// SweepPools is what ages out an idle member no run ever borrows.
func TestSweepPoolsRetiresAMemberPastMaxAge(t *testing.T) {
	h := newWarmHarness(t)
	var offset atomic.Int64
	h.s.poolOptions.Now = func() time.Time { return time.Now().Add(time.Duration(offset.Load())) }
	p := h.create(poolSpec(1, 1))
	h.waitIdle(p.GetId(), 1)
	id := h.backend.live()[0]

	offset.Store(int64(31 * time.Minute))
	h.s.SweepPools()
	waitFor(t, func() bool { return h.backend.isGone(id) }, "the sweep to retire the aged member")
}

// The global cap counts every tenant's reservations.
func TestCreatePoolChecksTheGlobalCap(t *testing.T) {
	h := newWarmHarness(t)
	h.s.cfg.MaxSandboxes, h.s.cfg.MaxSandboxesPerTenant = 4, 4
	h.create(poolSpec(0, 3))
	other := poolSpec(0, 2)
	other.TenantId = "tenant-b"
	if _, err := h.client.CreatePool(h.ctx(), &v1.CreatePoolRequest{Spec: other}); status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("pool past the global cap: %v, want ResourceExhausted", err)
	}
	fits := poolSpec(0, 1)
	fits.TenantId = "tenant-b"
	if _, err := h.client.CreatePool(h.ctx(), &v1.CreatePoolRequest{Spec: fits}); err != nil {
		t.Fatalf("pool that fits: %v", err)
	}
}

// A live member fails StartSandbox's own state check, so this needs a record
// still REQUESTED: without rejectPooled, a client could start a pool's member.
func TestStartSandboxRefusesAPoolMemberStillRequested(t *testing.T) {
	h := newWarmHarness(t)
	const id = "sbx_01ARZ3NDEKTSV4RRFFQ69G5FAV"
	spec := runtime.SandboxSpec{ID: id, TenantID: h.tenant, Image: "img", Backend: "fake", PoolID: "pool_01ARZ3NDEKTSV4RRFFQ69G5FAV",
		Resources: runtime.ResourceLimits{CPUMillis: 100, MemoryBytes: 1 << 20, Pids: 16}, Network: runtime.NetworkPolicy{Mode: runtime.NetworkNone}}
	if err := h.s.store.Create(&sandbox.Record{ID: id, State: sandbox.StateRequested, Spec: spec}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.client.StartSandbox(h.ctx(), &v1.StartSandboxRequest{SandboxId: id}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("StartSandbox on a REQUESTED member: %v, want FailedPrecondition", err)
	}
	if rec, _ := h.s.store.Load(id); rec.State != sandbox.StateRequested || len(h.backend.live()) != 0 {
		t.Fatalf("state = %s, live = %v; want it untouched", rec.State, h.backend.live())
	}
}

func TestForkChecksCapacity(t *testing.T) {
	h := newWarmHarness(t)
	snap := h.snapshot()
	h.s.cfg.MaxSandboxesPerTenant = 1
	h.startPlain()
	if _, err := h.client.ForkSnapshot(h.ctx(), &v1.ForkSnapshotRequest{SnapshotId: snap}); status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("fork at the cap: %v, want ResourceExhausted", err)
	}
}

// The check and the record that claims the slot are one step under createMu, or
// concurrent forks all see room.
func TestConcurrentForksStopAtTheCap(t *testing.T) {
	h := newWarmHarness(t)
	snap := h.snapshot()
	const limit = 4
	h.s.cfg.MaxSandboxesPerTenant = limit
	var wg sync.WaitGroup
	var forked atomic.Int32
	for range 24 {
		wg.Go(func() {
			if _, err := h.client.ForkSnapshot(h.ctx(), &v1.ForkSnapshotRequest{SnapshotId: snap}); err == nil {
				forked.Add(1)
			} else if status.Code(err) != codes.ResourceExhausted {
				t.Errorf("fork: %v", err)
			}
		})
	}
	wg.Wait()
	if got := forked.Load(); got != limit {
		t.Fatalf("%d forks succeeded, want exactly the cap of %d", got, limit)
	}
}

// A fork whose restore fails was never reported to anyone, so it has to give its
// capacity slot and disk quota back itself, and it leaves the same tombstone as a
// fork that failed to start, so the audit trail can say what happened to it.
func TestForkWithACorruptArchiveGivesItsSlotBack(t *testing.T) {
	h := newWarmHarness(t)
	snap := h.snapshot()
	before, _ := h.s.store.List()
	if err := os.WriteFile(h.s.snapshots.DataPath(snap), []byte("not an archive"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := h.client.ForkSnapshot(h.ctx(), &v1.ForkSnapshotRequest{SnapshotId: snap}); status.Code(err) != codes.Internal {
		t.Fatalf("fork of a corrupt archive: %v, want Internal", err)
	}
	if g, ft, _ := h.s.committed(h.tenant); g != 0 || ft != 0 {
		t.Fatalf("committed after the failed fork = %d, %d; want 0", g, ft)
	}
	recs, _ := h.s.store.List()
	if len(recs) != len(before)+1 {
		t.Fatalf("%d records, want the fork's tombstone on top of %d", len(recs), len(before))
	}
	for _, rec := range recs {
		if rec.State != sandbox.StateDestroyed {
			t.Fatalf("record %s is left %s, want DESTROYED", rec.ID, rec.State)
		}
	}
}

// A cold start runs on the client's context. Once it is cancelled the undo still
// has to reach the backend, and a backend that never answers must not hold it.
func TestStartThatFailsAfterTheClientLeftIsUndone(t *testing.T) {
	h := newWarmHarness(t)
	p := h.create(poolSpec(0, 1))
	spec := h.s.pools.get(pool.ID(p.GetId())).spec.MemberSpec()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var hadDeadline atomic.Bool
	h.backend.set(func(b *poolBackend) {
		b.prepareHook = func(context.Context, string) error {
			cancel()
			return context.Canceled
		}
		b.destroyHook = func(c context.Context, _ string) {
			_, ok := c.Deadline()
			hadDeadline.Store(ok)
		}
	})
	_, err := poolSandboxes{s: h.s}.Start(ctx, spec, true)
	if err == nil {
		t.Fatal("Start succeeded past a failed Prepare")
	}
	if live := h.backend.live(); len(live) != 0 {
		t.Fatalf("backend still holds %v: the undo ran on the cancelled context", live)
	}
	if !hadDeadline.Load() {
		t.Fatal("the undo's destroy has no deadline")
	}
}

// PruneAudit removes a destroyed member's record. If its pool still has a destroy
// to retry, NotFound means it is gone, not that the slot stays taken for ever.
func TestDestroyOfAMemberWhoseRecordIsGoneFreesTheSlot(t *testing.T) {
	h := newWarmHarness(t)
	p := h.create(poolSpec(1, 1))
	h.waitIdle(p.GetId(), 1)
	id := h.backend.live()[0]
	if err := h.s.destroy(context.Background(), id, ""); err != nil {
		t.Fatal(err)
	}
	if err := h.s.store.Delete(id); err != nil {
		t.Fatal(err)
	}
	if !h.s.pools.get(pool.ID(p.GetId())).pool.RetireExpired(id) {
		t.Fatal("the pool does not own its member")
	}
	waitFor(t, func() bool {
		st := h.get(p.GetId()).GetStats()
		return st.GetDestroyedTotal() == 1 && st.GetRetiring() == 0 && st.GetIdle() == 1
	}, "the slot to free and the pool to refill")
}

// brokenStream is an exec whose stream fails on the first read.
type brokenStream struct{ err error }

func (b brokenStream) Recv() (runtime.ExecChunk, error) { return runtime.ExecChunk{}, b.err }
func (b brokenStream) Close() error                     { return nil }

// A backend's own words can name host paths and runtime internals. They belong
// in the runner's log, and a tenant sees a fixed answer.
func TestBackendErrorTextStaysOutOfWhatATenantSees(t *testing.T) {
	const secret = "/var/lib/vora/runsc/private"
	h := newWarmHarness(t)
	h.backend.set(func(b *poolBackend) {
		b.startHook = func(context.Context, string) error { return errors.New("runsc: " + secret + ": boom") }
	})
	cold := h.create(poolSpec(0, 1))
	if _, err := h.run(h.ctx(), cold.GetId(), 5); status.Code(err) != codes.Unavailable || strings.Contains(err.Error(), secret) {
		t.Fatalf("cold start failure: %v, want Unavailable without the backend's text", err)
	}
	last := h.get(cold.GetId()).GetStats().GetLastStartError()
	if last == "" || strings.Contains(last, secret) {
		t.Fatalf("last_start_error = %q, want it set, without the backend's text", last)
	}

	h.backend.set(func(b *poolBackend) {
		b.startHook = nil
		b.execHook = func(context.Context, string, runtime.ExecRequest) (runtime.ExecStream, error) {
			return nil, errors.New("exec " + secret + ": refused")
		}
	})
	second := poolSpec(0, 1)
	second.Name = "second"
	// The retry waits on the first member's destroy, however long that takes here.
	wait := patience.Milliseconds()
	second.BorrowTimeoutMs = &wait
	refused := h.create(second)
	if _, err := h.run(h.ctx(), refused.GetId(), 5); status.Code(err) != codes.Unavailable || strings.Contains(err.Error(), secret) {
		t.Fatalf("refused exec: %v, want Unavailable without the backend's text", err)
	}

	h.backend.set(func(b *poolBackend) {
		b.execHook = func(ctx context.Context, _ string, _ runtime.ExecRequest) (runtime.ExecStream, error) {
			return brokenStream{errors.New("stream " + secret + ": reset")}, nil
		}
	})
	third := poolSpec(0, 1)
	third.Name = "third"
	broken := h.create(third)
	if _, err := h.run(h.ctx(), broken.GetId(), 5); status.Code(err) != codes.Internal || strings.Contains(err.Error(), secret) {
		t.Fatalf("broken stream: %v, want Internal without the backend's text", err)
	}
}

// A member start does slow disk work and is covered by its pool's reservation, so
// it must not wait for, or hold up, every other create.
func TestMemberStartDoesNotTakeTheCreateLock(t *testing.T) {
	h := newWarmHarness(t)
	p := h.create(poolSpec(0, 1))
	h.s.createMu.Lock()
	defer h.s.createMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := h.run(ctx, p.GetId(), 5); err != nil {
		t.Fatalf("a cold run with the create lock held: %v", err)
	}
}
