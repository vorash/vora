package api

import (
	"context"
	"errors"
	"fmt"

	v1 "github.com/vorash/vora/api/proto/v1"
	"github.com/vorash/vora/internal/egress"
	"github.com/vorash/vora/internal/secret"
)

// resolveBrokerSecrets loads and unseals the credentials for the aliases a
// create request grants. It fails before any runner is dialed when the store is
// absent, the channel to the runner is unencrypted, the alias is unknown to the
// tenant, or the credential's bound host is not on the sandbox allowlist — so a
// grant can never silently become a no-op.
func (s *Server) resolveBrokerSecrets(ctx context.Context, spec *v1.SandboxSpec) ([]*v1.BrokerCredential, error) {
	aliases := spec.GetNetwork().GetBrokerAliases()
	if len(aliases) == 0 {
		return nil, nil
	}
	if s.secrets() == nil {
		return nil, errSecretsNotConfigured
	}
	if !s.opts.RunnerTLS.Enabled() && !s.opts.AllowInsecureBroker {
		return nil, errors.New("credential brokering requires TLS on the API-to-runner channel; configure VORA_RUNNER_TLS_* or set VORA_ALLOW_INSECURE_BROKER=1")
	}
	tenant := spec.GetTenantId()
	if tenant == "" {
		return nil, errors.New("a brokered sandbox requires a tenant_id")
	}
	policy := egress.Policy{AllowedDomains: spec.GetNetwork().GetAllowedDomains()}
	out := make([]*v1.BrokerCredential, 0, len(aliases))
	for _, alias := range aliases {
		rec, value, err := s.secrets().Reveal(ctx, tenant, alias)
		if errors.Is(err, secret.ErrNotFound) {
			return nil, fmt.Errorf("unknown credential alias %q", alias)
		}
		if err != nil {
			return nil, fmt.Errorf("load credential %q: %w", alias, err)
		}
		if !policy.Allows(rec.Host) {
			return nil, fmt.Errorf("credential %q is bound to %q, which is not on the sandbox allowlist", alias, rec.Host)
		}
		out = append(out, &v1.BrokerCredential{
			Alias:  alias,
			Host:   rec.Host,
			Scheme: rec.Scheme,
			Header: rec.Header,
			Value:  value,
		})
	}
	return out, nil
}

// zeroBrokerSecrets drops plaintext references as soon as the create call
// returns. Best effort: the runner already has its own copy by then.
func zeroBrokerSecrets(creds []*v1.BrokerCredential) {
	for _, c := range creds {
		c.Value = ""
	}
}
