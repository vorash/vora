package api

import (
	"context"
	"net/http"
	"testing"

	"google.golang.org/protobuf/encoding/protojson"

	v1 "github.com/vorash/vora/api/proto/v1"
)

// TestSnapshotAndForkRoutes walk the happy path through the control plane:
// capture creates a snapshot route pinned to the runner that holds the data,
// fork lands the new sandbox's route right next to it, and delete cleans both.
func TestSnapshotAndForkRoutes(t *testing.T) {
	fr := &fakeRunner{}
	s, ts := newTestServer(t, fr)

	// The sandbox lives on run_test (registered by the harness).
	if err := s.store.SetRoute(context.Background(), SandboxRoute{SandboxID: "sbx_test", RunnerID: "run_test"}); err != nil {
		t.Fatal(err)
	}

	resp := do(t, http.MethodPost, ts.URL+"/v1/sandboxes/sbx_test/snapshot", bootstrap, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("snapshot status = %d, want 201", resp.StatusCode)
	}
	var snap v1.Snapshot
	if err := protojson.Unmarshal(readBody(t, resp), &snap); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if snap.GetId() != "snap_test" {
		t.Fatalf("snapshot = %v", &snap)
	}
	sroute, err := s.store.SnapshotRoute(context.Background(), "snap_test")
	if err != nil || sroute.RunnerID != "run_test" || sroute.SandboxID != "sbx_test" {
		t.Fatalf("snapshot route = %+v, %v", sroute, err)
	}

	resp = do(t, http.MethodPost, ts.URL+"/v1/snapshots/snap_test/fork", bootstrap, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("fork status = %d, want 201", resp.StatusCode)
	}
	var sb v1.Sandbox
	if err := protojson.Unmarshal(readBody(t, resp), &sb); err != nil {
		t.Fatalf("decode: %v", err)
	}
	resp.Body.Close()
	if sb.GetId() != "sbx_fork" {
		t.Fatalf("fork = %v", &sb)
	}
	if len(fr.forked) != 1 || fr.forked[0] != "snap_test" {
		t.Fatalf("runner did not receive the fork: %v", fr.forked)
	}
	froute, err := s.store.Route(context.Background(), "sbx_fork")
	if err != nil || froute.RunnerID != "run_test" {
		t.Fatalf("fork route = %+v, %v", froute, err)
	}

	resp = do(t, http.MethodDelete, ts.URL+"/v1/snapshots/snap_test", bootstrap, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("delete status = %d, want 200", resp.StatusCode)
	}
	if _, err := s.store.SnapshotRoute(context.Background(), "snap_test"); err == nil {
		t.Fatal("snapshot route survived the delete")
	}
	if len(fr.deletedSnaps) != 1 || fr.deletedSnaps[0] != "snap_test" {
		t.Fatalf("runner did not receive the delete: %v", fr.deletedSnaps)
	}
}

// TestSnapshotTenantIsolation is the boundary: a snapshot carries its tenant's
// state, so a foreign-tenant key must not be able to read, fork or delete it —
// and must not learn that it exists (404, not 403).
func TestSnapshotTenantIsolation(t *testing.T) {
	fr := &fakeRunner{snapshotTenant: "t1"}
	s, ts := newTestServer(t, fr)
	ctx := context.Background()

	for id, tenant := range map[string]string{"key_a": "t1", "key_b": "t2"} {
		if err := s.store.CreateKey(ctx, APIKey{
			ID: id, Name: id, TenantID: tenant, KeyHash: HashKey(id), Scopes: []string{ScopeSandbox},
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.store.SetSnapshotRoute(ctx, SnapshotRoute{
		SnapshotID: "snap_test", RunnerID: "run_test", TenantID: "t1", SandboxID: "sbx_test",
	}); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/v1/snapshots/snap_test"},
		{http.MethodPost, "/v1/snapshots/snap_test/fork"},
		{http.MethodDelete, "/v1/snapshots/snap_test"},
	} {
		resp := do(t, tc.method, ts.URL+tc.path, "key_b", nil)
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s %s as foreign tenant: status = %d, want 404", tc.method, tc.path, resp.StatusCode)
		}
	}
	if len(fr.forked) != 0 {
		t.Fatalf("foreign tenant reached the runner: forked = %v", fr.forked)
	}

	resp := do(t, http.MethodGet, ts.URL+"/v1/snapshots/snap_test", "key_a", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("owner get: status = %d, want 200", resp.StatusCode)
	}

	// Listing shows the caller only their own snapshots.
	resp = do(t, http.MethodGet, ts.URL+"/v1/snapshots", "key_b", nil)
	var list v1.ListSnapshotsResponse
	if err := protojson.Unmarshal(readBody(t, resp), &list); err != nil {
		t.Fatalf("decode: %v", err)
	}
	resp.Body.Close()
	if len(list.GetSnapshots()) != 0 {
		t.Fatalf("foreign tenant listed %v", list.GetSnapshots())
	}
	resp = do(t, http.MethodGet, ts.URL+"/v1/snapshots", "key_a", nil)
	if err := protojson.Unmarshal(readBody(t, resp), &list); err != nil {
		t.Fatalf("decode: %v", err)
	}
	resp.Body.Close()
	if len(list.GetSnapshots()) != 1 {
		t.Fatalf("owner listed %v, want 1 snapshot", list.GetSnapshots())
	}
}
