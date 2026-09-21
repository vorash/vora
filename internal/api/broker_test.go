package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	v1 "github.com/vorash/vora/api/proto/v1"
	"github.com/vorash/vora/internal/secret"
)

// brokerServer builds a control plane with a secret store, a registered fake
// runner and a tenant-scoped admin key, which is everything the brokered
// create path touches.
func brokerServer(t *testing.T, allowInsecure, withSecrets bool) (*fakeRunner, *httptest.Server, secret.Store, string) {
	t.Helper()
	sealer, err := secret.NewSealer(testMasterKey)
	if err != nil {
		t.Fatalf("NewSealer: %v", err)
	}
	var secrets secret.Store
	if withSecrets {
		secrets = secret.NewMemStore(sealer)
	}
	store := NewMemStore()
	plaintext, hash, err := NewAPIKey()
	if err != nil {
		t.Fatalf("NewAPIKey: %v", err)
	}
	if err := store.CreateKey(context.Background(), APIKey{
		ID: "key_t1", Name: "t1", TenantID: "t1", KeyHash: hash,
		Scopes: []string{ScopeAdmin, ScopeSandbox},
	}); err != nil {
		t.Fatalf("seed key: %v", err)
	}
	fr := &fakeRunner{}
	s := New(Options{
		Store:               store,
		BootstrapKey:        bootstrap,
		Secrets:             secrets,
		AllowInsecureBroker: allowInsecure,
		DialRunner:          func(string) (RunnerClient, error) { return fr, nil },
	})
	if _, err := s.Register(context.Background(), &v1.RegisterRunnerRequest{
		RunnerId:     "run_test",
		Address:      "127.0.0.1:9200",
		Backends:     []string{"bwrap"},
		NetworkModes: []string{"none", "restricted", "full", "private"},
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return fr, ts, secrets, plaintext
}

func brokeredSpec(aliases ...string) *v1.SandboxSpec {
	return &v1.SandboxSpec{
		Image:    "vora/base",
		TenantId: "t1",
		Network: &v1.NetworkPolicy{
			Mode:           v1.NetworkMode_NETWORK_RESTRICTED,
			AllowedDomains: []string{"api.example.com"},
			BrokerAliases:  aliases,
		},
	}
}

// TestCreateSandboxResolvesBrokerSecrets asserts the control plane unseals the
// granted alias and hands the credential to the runner, leaving only aliases in
// the spec, and then drops its own plaintext copy.
func TestCreateSandboxResolvesBrokerSecrets(t *testing.T) {
	fr, ts, secrets, key := brokerServer(t, true, true)
	if err := secrets.Put(context.Background(), secret.Record{
		TenantID: "t1", Alias: "github", Host: "api.example.com", Scheme: secret.SchemeBearer,
	}, "ghp_brokered_secret"); err != nil {
		t.Fatalf("put: %v", err)
	}

	resp := do(t, http.MethodPost, ts.URL+"/v1/sandboxes", key, brokeredSpec("github"))
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}
	if len(fr.creds) != 1 || len(fr.creds[0]) != 1 {
		t.Fatalf("runner credentials = %+v", fr.creds)
	}
	got := fr.creds[0][0]
	if got.GetAlias() != "github" || got.GetHost() != "api.example.com" || got.GetValue() != "ghp_brokered_secret" {
		t.Fatalf("credential = %+v", got)
	}
	// The persisted spec carries the grant, never the value.
	if len(fr.specs) != 1 {
		t.Fatalf("runner specs = %+v", fr.specs)
	}
	spec := fr.specs[0].GetNetwork()
	if len(spec.GetBrokerAliases()) != 1 || spec.GetBrokerAliases()[0] != "github" {
		t.Fatalf("spec aliases = %+v", spec.GetBrokerAliases())
	}
}

// TestCreateSandboxBrokerFailsClosed asserts every way a brokered create can be
// ill-formed fails before a sandbox is created, and never leaks the value into
// the error.
func TestCreateSandboxBrokerFailsClosed(t *testing.T) {
	tests := []struct {
		name          string
		allowInsecure bool
		withSecrets   bool
		key           string
		spec          *v1.SandboxSpec
		seed          bool
		wantStatus    int
		wantErr       string
	}{
		{
			name: "requires tls on the runner channel", allowInsecure: false, withSecrets: true,
			wantStatus: http.StatusBadRequest, wantErr: "TLS",
		},
		{
			name: "requires a secret store", allowInsecure: true, withSecrets: false,
			wantStatus: http.StatusServiceUnavailable, wantErr: "not configured",
		},
		{
			name: "unknown alias", allowInsecure: true, withSecrets: true,
			wantStatus: http.StatusBadRequest, wantErr: "unknown credential alias",
		},
		{
			name: "host not allowlisted", allowInsecure: true, withSecrets: true, seed: true,
			spec: func() *v1.SandboxSpec {
				s := brokeredSpec("github")
				s.Network.AllowedDomains = []string{"other.example.com"}
				return s
			}(),
			wantStatus: http.StatusBadRequest, wantErr: "not on the sandbox allowlist",
		},
		{
			name: "global admin must name a tenant", allowInsecure: true, withSecrets: true, key: bootstrap,
			spec: func() *v1.SandboxSpec {
				s := brokeredSpec("github")
				s.TenantId = ""
				return s
			}(),
			seed: true, wantStatus: http.StatusBadRequest, wantErr: "requires a tenant_id",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fr, ts, secrets, key := brokerServer(t, tc.allowInsecure, tc.withSecrets)
			if tc.key != "" {
				key = tc.key
			}
			if tc.seed && secrets != nil {
				if err := secrets.Put(context.Background(), secret.Record{
					TenantID: "t1", Alias: "github", Host: "api.example.com", Scheme: secret.SchemeBearer,
				}, "ghp_secret_value"); err != nil {
					t.Fatalf("put: %v", err)
				}
			}
			spec := tc.spec
			if spec == nil {
				spec = brokeredSpec("github")
			}
			resp := do(t, http.MethodPost, ts.URL+"/v1/sandboxes", key, spec)
			body := readBody(t, resp)
			if resp.StatusCode != tc.wantStatus {
				t.Fatalf("status = %d, want %d: %s", resp.StatusCode, tc.wantStatus, body)
			}
			if !strings.Contains(string(body), tc.wantErr) {
				t.Fatalf("error = %s, want it to mention %q", body, tc.wantErr)
			}
			if strings.Contains(string(body), "ghp_secret_value") {
				t.Fatal("the error leaked the credential value")
			}
			if len(fr.specs) != 0 {
				t.Fatalf("a sandbox was created despite the error: %+v", fr.specs)
			}
		})
	}
}

// TestCreateSandboxWithoutBrokerUnchanged asserts a plain create still reaches
// the runner and carries no credentials.
func TestCreateSandboxWithoutBrokerUnchanged(t *testing.T) {
	fr, ts, _, key := brokerServer(t, false, false)
	resp := do(t, http.MethodPost, ts.URL+"/v1/sandboxes", key, &v1.SandboxSpec{
		Image:    "vora/base",
		TenantId: "t1",
		Network:  &v1.NetworkPolicy{Mode: v1.NetworkMode_NETWORK_NONE},
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}
	if len(fr.creds) != 1 || len(fr.creds[0]) != 0 {
		t.Fatalf("credentials = %+v, want none", fr.creds)
	}
}
