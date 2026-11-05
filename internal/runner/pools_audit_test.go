package runner

import (
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	v1 "github.com/vorash/vora/api/proto/v1"
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
