package api

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	v1 "github.com/vorash/vora/api/proto/v1"
)

func createTestSandbox(t *testing.T, url string) {
	t.Helper()
	spec := &v1.SandboxSpec{Image: "vora/base", Resources: &v1.ResourceLimits{CpuMillis: 500, MemoryBytes: 1 << 20}, Network: &v1.NetworkPolicy{Mode: v1.NetworkMode_NETWORK_NONE}}
	resp := do(t, http.MethodPost, url+"/v1/sandboxes", bootstrap, spec)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d, want 201", resp.StatusCode)
	}
}

// A sandbox-scoped key can destroy its own sandbox. The runner keeps the audit
// trail as a tombstone, but it is only useful if the API can still reach it:
// dropping the route on destroy would turn every audit read into a 404 and
// hide the record from API callers all the same.
func TestAuditReadableThroughAPIAfterDestroy(t *testing.T) {
	fr := &fakeRunner{}
	_, ts := newTestServer(t, fr)
	createTestSandbox(t, ts.URL)

	resp := do(t, http.MethodDelete, ts.URL+"/v1/sandboxes/sbx_test", bootstrap, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("destroy status = %d, want 204", resp.StatusCode)
	}
	for _, path := range []string{"events", "execs", "egress"} {
		resp := do(t, http.MethodGet, ts.URL+"/v1/sandboxes/sbx_test/"+path, bootstrap, nil)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s after destroy = %d, want 200", path, resp.StatusCode)
		}
	}
}

// The tombstone route must not keep charging the runner for resources the
// destroyed sandbox no longer uses, or placement would starve the runner.
func TestDestroyedRouteReleasesPlacementCapacity(t *testing.T) {
	fr := &fakeRunner{}
	s, ts := newTestServer(t, fr)
	createTestSandbox(t, ts.URL)
	if got := s.committedByRunner(context.Background())["run_test"]; got.count != 1 || got.cpu != 500 {
		t.Fatalf("committed before destroy = %+v, want one sandbox of 500 millicores", got)
	}

	resp := do(t, http.MethodDelete, ts.URL+"/v1/sandboxes/sbx_test", bootstrap, nil)
	resp.Body.Close()

	route, err := s.store.Route(context.Background(), "sbx_test")
	if err != nil || !route.Destroyed {
		t.Fatalf("route after destroy = %+v, %v; want a destroyed tombstone", route, err)
	}
	if got := s.committedByRunner(context.Background())["run_test"]; got != (sandboxResources{}) {
		t.Fatalf("committed after destroy = %+v, want nothing", got)
	}
}

// When retention has pruned the runner's tombstone, the runner answers
// NotFound. The route then points at nothing, so the API drops it rather than
// keeping an entry no caller can ever clear.
func TestDestroyDropsRouteWhenRunnerForgotSandbox(t *testing.T) {
	fr := &fakeRunner{}
	s, ts := newTestServer(t, fr)
	createTestSandbox(t, ts.URL)
	fr.destroyErr = status.Error(codes.NotFound, "sandbox sbx_test not found")

	resp := do(t, http.MethodDelete, ts.URL+"/v1/sandboxes/sbx_test", bootstrap, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("destroy status = %d, want 404", resp.StatusCode)
	}
	if _, err := s.store.Route(context.Background(), "sbx_test"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("route after runner NotFound = %v, want removed", err)
	}
}
