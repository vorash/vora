package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/vorash/vora/internal/secret"
)

const testMasterKey = "unit-test-master-key-long-enough"

func secretsServer(t *testing.T) (*httptest.Server, secret.Store) {
	t.Helper()
	sealer, err := secret.NewSealer(testMasterKey)
	if err != nil {
		t.Fatalf("NewSealer: %v", err)
	}
	store := NewMemStore()
	// Tenant administrators: full admin scope, pinned to their tenant.
	for _, tenant := range []string{"t1", "t2"} {
		token := "vora_" + tenant
		if err := store.CreateKey(context.Background(), APIKey{
			ID: "key_" + tenant, Name: tenant, TenantID: tenant,
			KeyHash: HashKey(token), Scopes: []string{ScopeAdmin, ScopeSandbox},
		}); err != nil {
			t.Fatalf("seed key: %v", err)
		}
	}
	secrets := secret.NewMemStore(sealer)
	s := New(Options{
		Store:        store,
		BootstrapKey: bootstrap,
		Secrets:      secrets,
	})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts, secrets
}

func decodeJSONBody(t *testing.T, resp *http.Response, out any) {
	t.Helper()
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		t.Fatalf("decode body: %v", err)
	}
}

// TestSecretsDisabledWithoutStore asserts brokering stays off, with an explicit
// error, when no master key is configured.
func TestSecretsDisabledWithoutStore(t *testing.T) {
	_, ts := newTestServer(t, &fakeRunner{})
	for _, tc := range []struct{ method, path string }{
		{http.MethodPost, "/v1/secrets"},
		{http.MethodGet, "/v1/secrets"},
		{http.MethodDelete, "/v1/secrets/github"},
	} {
		resp := do(t, tc.method, ts.URL+tc.path, bootstrap, map[string]any{})
		resp.Body.Close()
		if resp.StatusCode != http.StatusServiceUnavailable {
			t.Errorf("%s %s: status = %d, want 503", tc.method, tc.path, resp.StatusCode)
		}
	}
}

// TestSecretsLifecycle asserts the value is write-only: create returns metadata,
// list omits the value, and delete is idempotent from the caller's view.
func TestSecretsLifecycle(t *testing.T) {
	ts, _ := secretsServer(t)
	const t1 = "vora_t1"

	resp := do(t, http.MethodPost, ts.URL+"/v1/secrets", t1, map[string]any{
		"alias": "github", "host": "api.github.com", "scheme": "bearer", "value": "ghp_secret_value",
	})
	var created struct {
		Secret secret.Record `json:"secret"`
	}
	decodeJSONBody(t, resp, &created)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d, want 201", resp.StatusCode)
	}
	if created.Secret.Alias != "github" || created.Secret.Host != "api.github.com" || created.Secret.TenantID != "t1" {
		t.Fatalf("created = %+v", created.Secret)
	}
	if strings.Contains(mustJSON(t, created), "ghp_secret_value") {
		t.Fatal("create response leaked the value")
	}

	resp = do(t, http.MethodGet, ts.URL+"/v1/secrets", t1, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list status = %d, want 200", resp.StatusCode)
	}
	body := readBody(t, resp)
	if strings.Contains(string(body), "ghp_secret_value") {
		t.Fatal("list response leaked the value")
	}
	var listed struct {
		Secrets []secret.Record `json:"secrets"`
	}
	if err := json.Unmarshal(body, &listed); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(listed.Secrets) != 1 || listed.Secrets[0].Alias != "github" {
		t.Fatalf("listed = %+v", listed.Secrets)
	}

	resp = do(t, http.MethodDelete, ts.URL+"/v1/secrets/github", t1, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete status = %d, want 204", resp.StatusCode)
	}
	resp = do(t, http.MethodDelete, ts.URL+"/v1/secrets/github", t1, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("second delete status = %d, want 404", resp.StatusCode)
	}
}

// TestSecretsTenantIsolation asserts a tenant can neither observe nor manage
// another tenant's alias, even when the alias is known.
func TestSecretsTenantIsolation(t *testing.T) {
	ts, secrets := secretsServer(t)

	resp := do(t, http.MethodPost, ts.URL+"/v1/secrets", bootstrap, map[string]any{
		"alias": "github", "host": "api.github.com", "scheme": "bearer",
		"value": "t2_value", "tenant_id": "t2",
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("global admin create status = %d, want 201", resp.StatusCode)
	}

	// t1 sees nothing.
	resp = do(t, http.MethodGet, ts.URL+"/v1/secrets", "vora_t1", nil)
	var listed struct {
		Secrets []secret.Record `json:"secrets"`
	}
	decodeJSONBody(t, resp, &listed)
	if len(listed.Secrets) != 0 {
		t.Fatalf("t1 sees t2 secrets: %+v", listed.Secrets)
	}

	// t1 creating the same alias must not disturb t2's value.
	resp = do(t, http.MethodPost, ts.URL+"/v1/secrets", "vora_t1", map[string]any{
		"alias": "github", "host": "evil.example.com", "scheme": "bearer", "value": "x",
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("t1 create status = %d, want 201 (its own alias)", resp.StatusCode)
	}
	if got := revealSecret(t, secrets, "t2", "github"); got != "t2_value" {
		t.Fatalf("t2 value = %q, want unchanged", got)
	}
	if got := revealSecret(t, secrets, "t1", "github"); got != "x" {
		t.Fatalf("t1 value = %q", got)
	}

	// t1 cannot delete t2's credential, even by naming the tenant.
	resp = do(t, http.MethodDelete, ts.URL+"/v1/secrets/github?tenant_id=t2", "vora_t1", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-tenant delete status = %d, want 403", resp.StatusCode)
	}

	// A global administrator must name the tenant.
	resp = do(t, http.MethodPost, ts.URL+"/v1/secrets", bootstrap, map[string]any{
		"alias": "openai", "host": "api.openai.com", "scheme": "bearer", "value": "x",
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("global admin without tenant status = %d, want 403", resp.StatusCode)
	}
}

// TestSecretsValidation asserts bad records are rejected before storage.
func TestSecretsValidation(t *testing.T) {
	ts, _ := secretsServer(t)
	const t1 = "vora_t1"
	tests := []struct {
		name string
		body map[string]any
	}{
		{"bad alias", map[string]any{"alias": "GitHub", "host": "api.github.com", "scheme": "bearer", "value": "x"}},
		{"ip host", map[string]any{"alias": "github", "host": "10.0.0.1", "scheme": "bearer", "value": "x"}},
		{"unknown scheme", map[string]any{"alias": "github", "host": "api.github.com", "scheme": "magic", "value": "x"}},
		{"empty value", map[string]any{"alias": "github", "host": "api.github.com", "scheme": "bearer", "value": ""}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resp := do(t, http.MethodPost, ts.URL+"/v1/secrets", t1, tc.body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", resp.StatusCode)
			}
		})
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(data)
}

// revealSecret reads a value straight from the store: the HTTP API has no
// reveal endpoint, by design.
func revealSecret(t *testing.T, store secret.Store, tenant, alias string) string {
	t.Helper()
	_, value, err := store.Reveal(context.Background(), tenant, alias)
	if err != nil {
		t.Fatalf("reveal %s/%s: %v", tenant, alias, err)
	}
	return value
}
