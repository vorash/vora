package runner

import (
	"context"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	v1 "github.com/vorash/vora/api/proto/v1"
	"github.com/vorash/vora/internal/egress"
	"github.com/vorash/vora/internal/runtime"
)

// TestValidateBrokerGrants asserts the grant/credential contract is enforced
// before a sandbox is created: a missing, extra, duplicate or non-allowlisted
// credential must fail rather than start a sandbox whose aliases cannot work.
func TestValidateBrokerGrants(t *testing.T) {
	spec := runtime.SandboxSpec{
		Network: runtime.NetworkPolicy{
			Mode:           runtime.NetworkRestricted,
			AllowedDomains: []string{"api.example.com"},
			BrokerAliases:  []string{"github"},
		},
	}
	ok := egress.Credential{Alias: "github", Host: "api.example.com", Scheme: egress.SchemeBearer, Value: "v"}

	tests := []struct {
		name    string
		spec    runtime.SandboxSpec
		creds   []egress.Credential
		wantErr string
	}{
		{"ok", spec, []egress.Credential{ok}, ""},
		{"no grants no creds", runtime.SandboxSpec{}, nil, ""},
		{"missing credential", spec, nil, "has no credential"},
		{"unregistered credential", runtime.SandboxSpec{}, []egress.Credential{ok}, "not granted"},
		{"duplicate credential", spec, []egress.Credential{ok, ok}, "duplicate"},
		{
			"host not allowlisted",
			spec,
			[]egress.Credential{{Alias: "github", Host: "evil.example.com", Scheme: egress.SchemeBearer, Value: "v"}},
			"not on the allowlist",
		},
		{"empty alias grant", runtime.SandboxSpec{Network: runtime.NetworkPolicy{BrokerAliases: []string{""}}}, nil, "must not be empty"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateBrokerGrants(tc.spec, tc.creds)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("validateBrokerGrants = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("validateBrokerGrants = %v, want error containing %q", err, tc.wantErr)
			}
		})
	}
}

// TestRejectBrokerSecrets asserts a plaintext runner endpoint refuses create
// requests that carry credentials, and leaves every other request alone.
func TestRejectBrokerSecrets(t *testing.T) {
	called := false
	handler := func(context.Context, any) (any, error) {
		called = true
		return &v1.CreateSandboxResponse{}, nil
	}

	withSecrets := &v1.CreateSandboxRequest{
		Spec:          &v1.SandboxSpec{},
		BrokerSecrets: []*v1.BrokerCredential{{Alias: "github", Host: "api.example.com", Value: "secret"}},
	}
	if _, err := RejectBrokerSecrets(context.Background(), withSecrets, &grpc.UnaryServerInfo{}, handler); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("code = %v, want FailedPrecondition", status.Code(err))
	}
	if called {
		t.Fatal("the handler ran despite brokered credentials")
	}

	for _, req := range []any{
		&v1.CreateSandboxRequest{Spec: &v1.SandboxSpec{}},
		&v1.GetSandboxRequest{SandboxId: "sbx_1"},
	} {
		called = false
		if _, err := RejectBrokerSecrets(context.Background(), req, &grpc.UnaryServerInfo{}, handler); err != nil {
			t.Fatalf("request %T rejected: %v", req, err)
		}
		if !called {
			t.Fatalf("request %T did not reach the handler", req)
		}
	}
}

// TestStageAndTakeBrokerCredentials asserts staged credentials are consumed
// exactly once, so a second start cannot reuse another sandbox's secrets.
func TestStageAndTakeBrokerCredentials(t *testing.T) {
	s := testServer(t, testRegistry(t))
	creds := []egress.Credential{{Alias: "github", Value: "secret"}}
	s.stageBrokerCredentials("sbx_1", creds)

	got := s.takeBrokerCredentials("sbx_1")
	if len(got) != 1 || got[0].Alias != "github" {
		t.Fatalf("take = %+v", got)
	}
	if again := s.takeBrokerCredentials("sbx_1"); again != nil {
		t.Fatalf("credentials were not consumed: %+v", again)
	}
	s.stageBrokerCredentials("sbx_2", creds)
	s.dropBrokerCredentials("sbx_2")
	if got := s.takeBrokerCredentials("sbx_2"); got != nil {
		t.Fatalf("dropped credentials were still staged: %+v", got)
	}

	zeroCredentials(creds)
	if creds[0].Value != "" {
		t.Fatal("zeroCredentials left the value in place")
	}
}
