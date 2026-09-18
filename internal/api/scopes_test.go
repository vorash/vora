package api

import (
	"context"
	"net/http"
	"testing"

	v1 "github.com/vorash/vora/api/proto/v1"
)

// TestAdminScopeImpliesSandbox asserts an admin-only key can also drive
// sandboxes. The README creates the operator key with `--scope admin` alone,
// and requiring a second scope for sandbox routes is a trap: it fails with
// "API key lacks scope sandbox" on the very first real deployment.
func TestAdminScopeImpliesSandbox(t *testing.T) {
	fr := &fakeRunner{}
	s, ts := newTestServer(t, fr)

	plaintext, hash, err := NewAPIKey()
	if err != nil {
		t.Fatalf("api key: %v", err)
	}
	if err := s.store.CreateKey(context.Background(), APIKey{
		ID: "key_admin_only", Name: "operator", KeyHash: hash, Scopes: []string{ScopeAdmin},
	}); err != nil {
		t.Fatalf("seed key: %v", err)
	}

	resp := do(t, http.MethodPost, ts.URL+"/v1/sandboxes", plaintext, &v1.SandboxSpec{Image: "vora/base"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("admin key creating a sandbox = %d, want 201", resp.StatusCode)
	}

	// The admin routes still work for the same key.
	resp = do(t, http.MethodGet, ts.URL+"/v1/keys", plaintext, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("admin key listing keys = %d, want 200", resp.StatusCode)
	}
}

// TestSandboxScopeDoesNotImplyAdmin asserts the implication is one-way: a
// sandbox key is not an operator key.
func TestSandboxScopeDoesNotImplyAdmin(t *testing.T) {
	fr := &fakeRunner{}
	s, ts := newTestServer(t, fr)

	plaintext, hash, err := NewAPIKey()
	if err != nil {
		t.Fatalf("api key: %v", err)
	}
	if err := s.store.CreateKey(context.Background(), APIKey{
		ID: "key_sandbox_only", Name: "ci", TenantID: "t1", KeyHash: hash, Scopes: []string{ScopeSandbox},
	}); err != nil {
		t.Fatalf("seed key: %v", err)
	}

	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/v1/keys"},
		{http.MethodGet, "/v1/runners"},
	} {
		resp := do(t, tc.method, ts.URL+tc.path, plaintext, nil)
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("%s %s with a sandbox key = %d, want 403", tc.method, tc.path, resp.StatusCode)
		}
	}
}
