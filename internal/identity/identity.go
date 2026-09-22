// Package identity defines the canonical agent identity. The SPIFFE-shaped URI
// is the authz subject and the audit attribution value; it is derived from a
// tenant and an agent handle and never stored. No SPIFFE protocol lives here —
// an SVID issuer later asserts the same URI and nothing else changes.
package identity

import (
	"fmt"
	"net/url"
	"strings"
)

// TrustDomain is the SPIFFE trust domain Vora agent identities live in.
const TrustDomain = "vora.sh"

// Agent identifies one agent within one tenant.
type Agent struct {
	Tenant string
	Name   string
}

// ValidateName checks an agent handle or tenant id is usable inside the URI.
// Handles are attacker-supplied at the API boundary, so this guards the URI
// structure itself.
func ValidateName(s string) error {
	if len(s) == 0 || len(s) > 64 {
		return fmt.Errorf("identity: handle must be between 1 and 64 characters")
	}
	for i, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
		case (r == '-' || r == '_') && i > 0:
		default:
			return fmt.Errorf("identity: handle %q contains %q; allowed are a-z, 0-9, '-' and '_'", s, string(r))
		}
	}
	return nil
}

// URI returns the canonical SPIFFE-shaped identity for the agent.
func (a Agent) URI() string {
	return "spiffe://" + TrustDomain + "/tenant/" + url.PathEscape(a.Tenant) + "/agent/" + url.PathEscape(a.Name)
}

// Parse parses a canonical agent identity URI, strictly.
func Parse(uri string) (Agent, error) {
	rest, ok := strings.CutPrefix(uri, "spiffe://"+TrustDomain+"/tenant/")
	if !ok {
		return Agent{}, fmt.Errorf("identity: %q is not a vora.sh agent identity", uri)
	}
	tenant, agent, ok := strings.Cut(rest, "/agent/")
	if !ok || tenant == "" || agent == "" || strings.Contains(agent, "/") {
		return Agent{}, fmt.Errorf("identity: %q is not a vora.sh agent identity", uri)
	}
	t, err := url.PathUnescape(tenant)
	if err != nil {
		return Agent{}, fmt.Errorf("identity: %q has an invalid tenant segment: %w", uri, err)
	}
	n, err := url.PathUnescape(agent)
	if err != nil {
		return Agent{}, fmt.Errorf("identity: %q has an invalid agent segment: %w", uri, err)
	}
	a := Agent{Tenant: t, Name: n}
	if a.URI() != uri {
		return Agent{}, fmt.Errorf("identity: %q is not canonical", uri)
	}
	return a, nil
}
