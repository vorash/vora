package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"google.golang.org/protobuf/encoding/protojson"

	v1 "github.com/vorash/vora/api/proto/v1"
)

// Why these matter: the agent identity is both an authz subject and the audit
// attribution. A key that can act as another agent forges both.

func addAgentTestKey(t *testing.T, s *Server, tenant, agent string) (string, APIKey) {
	t.Helper()
	plaintext, key := newStoredTestKey(t, tenant, ScopeSandbox)
	key.AgentID = agent
	if err := s.store.CreateKey(context.Background(), key); err != nil {
		t.Fatal(err)
	}
	return plaintext, key
}

// TestCreateSandboxForcesAgentFromKey asserts an agent-bound key cannot claim
// another agent's identity, mirroring the tenant rule.
func TestCreateSandboxForcesAgentFromKey(t *testing.T) {
	fr := &fakeRunner{}
	s, ts := newTestServer(t, fr)
	agentKey, _ := addAgentTestKey(t, s, "tenant-a", "analyst")

	resp := do(t, http.MethodPost, ts.URL+"/v1/sandboxes", agentKey,
		&v1.SandboxSpec{TenantId: "tenant-a", AgentId: "impostor"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create = %d, want 201", resp.StatusCode)
	}
	if got := fr.specs[0].GetAgentId(); got != "analyst" {
		t.Fatalf("spec agent = %q, want %q (binding must be forced from the key)", got, "analyst")
	}
	if got := fr.specs[0].GetTenantId(); got != "tenant-a" {
		t.Fatalf("spec tenant = %q, want tenant-a", got)
	}
}

// TestAgentSandboxIsolation asserts a bound key cannot act on another agent's
// sandbox in its tenant: every route reads as "not found", while an unbound
// tenant key keeps its legacy reach across the tenant.
func TestAgentSandboxIsolation(t *testing.T) {
	fr := &fakeRunner{sandboxes: []*v1.Sandbox{
		{Id: "sbx_a", Spec: &v1.SandboxSpec{TenantId: "tenant-a", AgentId: "agent-a"}},
		{Id: "sbx_b", Spec: &v1.SandboxSpec{TenantId: "tenant-a", AgentId: "agent-b"}},
	}}
	s, ts := newTestServer(t, fr)
	agentA, _ := addAgentTestKey(t, s, "tenant-a", "agent-a")
	tenantKey := addTestKey(t, s, "tenant-a", ScopeSandbox)
	_ = s.store.SetRoute(context.Background(), SandboxRoute{SandboxID: "sbx_a", RunnerID: "run_test", TenantID: "tenant-a", AgentID: "agent-a"})
	_ = s.store.SetRoute(context.Background(), SandboxRoute{SandboxID: "sbx_b", RunnerID: "run_test", TenantID: "tenant-a", AgentID: "agent-b"})

	for _, path := range []string{
		"/v1/sandboxes/sbx_b",
		"/v1/sandboxes/sbx_b/stats",
		"/v1/sandboxes/sbx_b/events",
		"/v1/sandboxes/sbx_b/execs",
		"/v1/sandboxes/sbx_b/egress",
	} {
		resp := do(t, http.MethodGet, ts.URL+path, agentA, nil)
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s with a foreign agent key = %d, want 404", path, resp.StatusCode)
		}
	}
	for _, call := range []struct{ method, path string }{
		{http.MethodPost, "/v1/sandboxes/sbx_b/exec"},
		{http.MethodDelete, "/v1/sandboxes/sbx_b"},
		{http.MethodPost, "/v1/sandboxes/sbx_b/snapshot"},
	} {
		resp := do(t, call.method, ts.URL+call.path, agentA, &v1.ExecRequest{Command: "true"})
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s %s with a foreign agent key = %d, want 404", call.method, call.path, resp.StatusCode)
		}
	}

	resp := do(t, http.MethodGet, ts.URL+"/v1/sandboxes/sbx_a", agentA, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("own sandbox = %d, want 200", resp.StatusCode)
	}
	for _, id := range []string{"sbx_a", "sbx_b"} {
		resp := do(t, http.MethodGet, ts.URL+"/v1/sandboxes/"+id, tenantKey, nil)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("unbound tenant key on %s = %d, want 200 (legacy reach)", id, resp.StatusCode)
		}
	}
}

// TestListSandboxesFiltersByAgent asserts the list endpoint never leaks other
// agents' sandbox ids.
func TestListSandboxesFiltersByAgent(t *testing.T) {
	fr := &fakeRunner{sandboxes: []*v1.Sandbox{
		{Id: "sbx_a", Spec: &v1.SandboxSpec{TenantId: "tenant-a", AgentId: "agent-a"}},
		{Id: "sbx_b", Spec: &v1.SandboxSpec{TenantId: "tenant-a", AgentId: "agent-b"}},
	}}
	s, ts := newTestServer(t, fr)
	agentA, _ := addAgentTestKey(t, s, "tenant-a", "agent-a")

	resp := do(t, http.MethodGet, ts.URL+"/v1/sandboxes", agentA, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list = %d", resp.StatusCode)
	}
	var listed v1.ListSandboxesResponse
	if err := protojson.Unmarshal(readBody(t, resp), &listed); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(listed.GetSandboxes()) != 1 || listed.GetSandboxes()[0].GetId() != "sbx_a" {
		t.Fatalf("agent list = %+v, want only sbx_a", listed.GetSandboxes())
	}
}

// TestSnapshotForkAgentIsolation asserts snapshots inherit the agent scoping.
func TestSnapshotForkAgentIsolation(t *testing.T) {
	fr := &fakeRunner{snapshotTenant: "tenant-a"}
	s, ts := newTestServer(t, fr)
	agentA, _ := addAgentTestKey(t, s, "tenant-a", "agent-a")
	_ = s.store.SetRoute(context.Background(), SandboxRoute{SandboxID: "sbx_b", RunnerID: "run_test", TenantID: "tenant-a", AgentID: "agent-b"})
	_ = s.store.SetSnapshotRoute(context.Background(), SnapshotRoute{SnapshotID: "snap_b", RunnerID: "run_test", TenantID: "tenant-a", AgentID: "agent-b", SandboxID: "sbx_b"})

	for _, call := range []struct{ method, path string }{
		{http.MethodGet, "/v1/snapshots/snap_b"},
		{http.MethodPost, "/v1/snapshots/snap_b/fork"},
		{http.MethodDelete, "/v1/snapshots/snap_b"},
	} {
		resp := do(t, call.method, ts.URL+call.path, agentA, nil)
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s %s with a foreign agent key = %d, want 404", call.method, call.path, resp.StatusCode)
		}
	}
}

// TestCreateKeyAgentBinding asserts the binding rules: no admin keys bound to
// an agent (admin is not a workload credential), no forged handles.
func TestCreateKeyAgentBinding(t *testing.T) {
	_, ts := newTestServer(t, &fakeRunner{})

	for name, body := range map[string]map[string]any{
		"admin scope":   {"name": "ops", "tenant_id": "tenant-a", "agent_id": "analyst", "scopes": []string{ScopeAdmin, ScopeSandbox}},
		"forged handle": {"name": "bad", "tenant_id": "tenant-a", "agent_id": "a/../b", "scopes": []string{ScopeSandbox}},
	} {
		resp := do(t, http.MethodPost, ts.URL+"/v1/keys", bootstrap, body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: create = %d, want 400", name, resp.StatusCode)
		}
	}

	resp := do(t, http.MethodPost, ts.URL+"/v1/keys", bootstrap, map[string]any{
		"name": "workload", "tenant_id": "tenant-a", "agent_id": "analyst", "scopes": []string{ScopeSandbox},
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create bound key = %d, want 201", resp.StatusCode)
	}
	var out struct {
		APIKey APIKey `json:"api_key"`
	}
	if err := json.Unmarshal(readBody(t, resp), &out); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if out.APIKey.AgentID != "analyst" {
		t.Fatalf("key agent = %q, want analyst", out.APIKey.AgentID)
	}
}

// TestRequestActionsCarryKeyActor asserts request-driven actions reach the
// runner attributed to the key that caused them.
func TestRequestActionsCarryKeyActor(t *testing.T) {
	fr := &fakeRunner{}
	s, ts := newTestServer(t, fr)
	agentKey, key := addAgentTestKey(t, s, "tenant-a", "analyst")

	spec := &v1.SandboxSpec{TenantId: "tenant-a"}
	resp := do(t, http.MethodPost, ts.URL+"/v1/sandboxes", agentKey, spec)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create = %d", resp.StatusCode)
	}
	resp = do(t, http.MethodPost, ts.URL+"/v1/sandboxes/sbx_test/exec", agentKey, &v1.ExecRequest{Command: "true"})
	resp.Body.Close()
	resp = do(t, http.MethodDelete, ts.URL+"/v1/sandboxes/sbx_test", agentKey, nil)
	resp.Body.Close()

	if len(fr.actors) != 3 {
		t.Fatalf("runner saw %d actors, want 3 (start, exec, destroy)", len(fr.actors))
	}
	for i, actor := range fr.actors {
		if actor.GetKeyId() != key.ID {
			t.Errorf("actor %d key = %q, want %q", i, actor.GetKeyId(), key.ID)
		}
	}
}
