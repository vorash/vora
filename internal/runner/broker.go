package runner

import (
	"context"
	"fmt"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	v1 "github.com/vorash/vora/api/proto/v1"
	"github.com/vorash/vora/internal/egress"
	"github.com/vorash/vora/internal/runtime"
)

// validateBrokerGrants enforces the grant/credential contract before a sandbox
// is created: every granted alias has exactly one credential, no credential is
// sent without a grant, and every credential's host is on the sandbox
// allowlist. A mismatch fails closed so a sandbox can never start "brokered"
// with a missing or extra credential.
func validateBrokerGrants(spec runtime.SandboxSpec, creds []egress.Credential) error {
	granted := make(map[string]bool, len(spec.Network.BrokerAliases))
	for _, alias := range spec.Network.BrokerAliases {
		if alias == "" {
			return fmt.Errorf("broker alias must not be empty")
		}
		granted[alias] = true
	}
	policy := egress.Policy{AllowedDomains: spec.Network.AllowedDomains}
	seen := make(map[string]bool, len(creds))
	for _, c := range creds {
		if !granted[c.Alias] {
			return fmt.Errorf("credential %q is not granted by the spec", c.Alias)
		}
		if seen[c.Alias] {
			return fmt.Errorf("duplicate credential for alias %q", c.Alias)
		}
		seen[c.Alias] = true
		if !policy.Allows(c.Host) {
			return fmt.Errorf("credential %q is bound to %q, which is not on the allowlist", c.Alias, c.Host)
		}
	}
	for alias := range granted {
		if !seen[alias] {
			return fmt.Errorf("alias %q has no credential", alias)
		}
	}
	return nil
}

// stageBrokerCredentials holds resolved credentials until the sandbox starts.
func (s *Server) stageBrokerCredentials(id string, creds []egress.Credential) {
	s.brokerMu.Lock()
	defer s.brokerMu.Unlock()
	s.brokerCreds[id] = creds
}

// takeBrokerCredentials removes and returns a sandbox's staged credentials.
func (s *Server) takeBrokerCredentials(id string) []egress.Credential {
	s.brokerMu.Lock()
	defer s.brokerMu.Unlock()
	creds := s.brokerCreds[id]
	delete(s.brokerCreds, id)
	return creds
}

// dropBrokerCredentials discards credentials for a sandbox that will not start.
func (s *Server) dropBrokerCredentials(id string) {
	s.brokerMu.Lock()
	defer s.brokerMu.Unlock()
	delete(s.brokerCreds, id)
}

// zeroCredentials drops plaintext references as soon as a start call returns.
// It is best effort: the broker keeps its own copy for the sandbox's lifetime.
func zeroCredentials(creds []egress.Credential) {
	for i := range creds {
		creds[i].Value = ""
	}
}

// RejectBrokerSecrets is a gRPC interceptor for a runner endpoint without
// transport encryption. It refuses create requests that carry brokered
// credentials so a control plane cannot ship secrets over a channel anyone on
// the path can read. Operators who accept that risk set
// VORA_ALLOW_INSECURE_BROKER=1 and do not install this interceptor.
func RejectBrokerSecrets(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	if create, ok := req.(*v1.CreateSandboxRequest); ok && len(create.GetBrokerSecrets()) > 0 {
		return nil, status.Error(codes.FailedPrecondition,
			"refusing brokered credentials over a plaintext runner channel: enable VORA_RUNNER_TLS_* or set VORA_ALLOW_INSECURE_BROKER=1")
	}
	return handler(ctx, req)
}
