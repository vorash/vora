package runner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	v1 "github.com/vorash/vora/api/proto/v1"
	"github.com/vorash/vora/internal/sandbox"
)

// A deleted pool's trail is what answers "who removed this", so it has to stay
// readable after the pool is gone, and only by the tenant that owned it.
func TestGetPoolEvents(t *testing.T) {
	h := newWarmHarness(t)
	ctx := h.ctx()
	p := h.create(poolSpec(0, 1))
	if _, err := h.client.DeletePool(ctx, &v1.DeletePoolRequest{PoolId: p.GetId(), TenantId: h.tenant, WaitMs: 5000, Actor: &v1.Actor{KeyId: "key_delete"}}); err != nil {
		t.Fatal(err)
	}
	h.s.events.flush()

	got, err := h.client.GetPoolEvents(ctx, &v1.GetPoolEventsRequest{PoolId: p.GetId(), TenantId: h.tenant})
	if err != nil {
		t.Fatalf("GetPoolEvents after delete: %v", err)
	}
	var reasons []string
	for _, e := range got.GetEvents() {
		reasons = append(reasons, strings.SplitN(e.GetReason(), ":", 2)[0])
	}
	if want := "pool_create pool_drain pool_delete"; strings.Join(reasons, " ") != want {
		t.Fatalf("events = %v, want %s", reasons, want)
	}

	for _, c := range []struct {
		name string
		req  *v1.GetPoolEventsRequest
		want codes.Code
	}{
		{"another tenant", &v1.GetPoolEventsRequest{PoolId: p.GetId(), TenantId: "tenant-other"}, codes.NotFound},
		{"no tenant", &v1.GetPoolEventsRequest{PoolId: p.GetId()}, codes.InvalidArgument},
		{"a path for an id", &v1.GetPoolEventsRequest{PoolId: "../../etc", TenantId: h.tenant}, codes.InvalidArgument},
		{"a sandbox id", &v1.GetPoolEventsRequest{PoolId: "sbx_01KB0000000000000000000000", TenantId: h.tenant}, codes.InvalidArgument},
	} {
		if _, err := h.client.GetPoolEvents(ctx, c.req); status.Code(err) != c.want {
			t.Errorf("%s: %v, want %s", c.name, err, c.want)
		}
	}
}

func TestPrunePoolTrails(t *testing.T) {
	h := newWarmHarness(t)
	ctx := h.ctx()
	s := h.s
	s.cfg.AuditRetention = 24 * time.Hour

	deleted := func() string {
		p := h.create(poolSpec(0, 1))
		if _, err := h.client.DeletePool(ctx, &v1.DeletePoolRequest{PoolId: p.GetId(), TenantId: h.tenant, WaitMs: 5000}); err != nil {
			t.Fatal(err)
		}
		return p.GetId()
	}
	oldGone, youngGone := deleted(), deleted()
	live := h.create(poolSpec(0, 1)).GetId()
	// flush ends the queue; later events are written inline.
	s.events.flush()

	age := func(id string) {
		t.Helper()
		old := time.Now().Add(-48 * time.Hour)
		if err := os.Chtimes(filepath.Join(s.workspaces.LogsDir(id), sandbox.EventsFile), old, old); err != nil {
			t.Fatal(err)
		}
	}
	age(oldGone)
	age(live)
	// A directory whose first event never landed has nothing but its own age.
	empty := "pool_01KB0000000000000000000000"
	if err := os.MkdirAll(s.workspaces.LogsDir(empty), 0o755); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(s.workspaces.LogsDir(empty), old, old); err != nil {
		t.Fatal(err)
	}

	exists := func(id string) bool {
		_, err := os.Stat(s.workspaces.Paths(id).RootDir)
		return err == nil
	}

	off := s.cfg.AuditRetention
	s.cfg.AuditRetention = 0
	if err := s.PruneAudit(ctx); err != nil {
		t.Fatal(err)
	}
	if !exists(oldGone) {
		t.Fatal("a pool's trail was removed with retention off")
	}
	s.cfg.AuditRetention = off

	if err := s.PruneAudit(ctx); err != nil {
		t.Fatal(err)
	}
	if exists(oldGone) {
		t.Error("the trail of a deleted pool outlived the retention limit")
	}
	if exists(empty) {
		t.Error("a pool directory with no events outlived the retention limit")
	}
	if !exists(youngGone) {
		t.Error("the trail of a recently deleted pool was removed")
	}
	if !exists(live) {
		t.Error("the trail of a pool that still exists was removed")
	}
	if _, err := h.client.GetPool(ctx, &v1.GetPoolRequest{PoolId: live, TenantId: h.tenant}); err != nil {
		t.Errorf("the live pool: %v", err)
	}
}

// A pool's local directory holds nothing but its trail, and retention decides
// when that goes. With an external store the runner has no say in the rows.
func TestPrunePoolTrailsSkipsExternalAuditStore(t *testing.T) {
	h := newWarmHarness(t)
	s := h.s
	s.cfg.AuditRetention = time.Nanosecond
	p := h.create(poolSpec(0, 1))
	if _, err := h.client.DeletePool(h.ctx(), &v1.DeletePoolRequest{PoolId: p.GetId(), TenantId: h.tenant, WaitMs: 5000}); err != nil {
		t.Fatal(err)
	}
	s.events.flush()
	s.SetAudit(failingCounts{s.audit})
	if err := s.PruneAudit(h.ctx()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(s.workspaces.Paths(p.GetId()).RootDir); err != nil {
		t.Fatalf("pruned with an external audit store: %v", err)
	}
}
