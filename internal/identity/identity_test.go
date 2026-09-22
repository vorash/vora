package identity

import "testing"

func TestURIRoundTrip(t *testing.T) {
	for _, a := range []Agent{
		{Tenant: "acme", Name: "analyst"},
		{Tenant: "t-1", Name: "agent_2"},
		{Tenant: "weird tenant", Name: "a/b"},
	} {
		uri := a.URI()
		got, err := Parse(uri)
		if err != nil {
			t.Fatalf("Parse(%q): %v", uri, err)
		}
		if got != a {
			t.Fatalf("round trip: got %+v, want %+v", got, a)
		}
	}
}

func TestURIShape(t *testing.T) {
	if got := (Agent{Tenant: "acme", Name: "analyst"}).URI(); got != "spiffe://vora.sh/tenant/acme/agent/analyst" {
		t.Fatalf("URI() = %q", got)
	}
}

func TestParseRejectsForeignAndForged(t *testing.T) {
	for _, uri := range []string{
		"",
		"spiffe://other.sh/tenant/acme/agent/analyst",
		"https://vora.sh/tenant/acme/agent/analyst",
		"spiffe://vora.sh/tenant/acme",
		"spiffe://vora.sh/tenant//agent/analyst",
		"spiffe://vora.sh/tenant/acme/agent/",
		"spiffe://vora.sh/tenant/acme/agent/analyst/extra",
		"spiffe://vora.sh/tenant/acme/agent/a/../b",
		"spiffe://vora.sh/tenant/acme/agent/%zz",
	} {
		if _, err := Parse(uri); err == nil {
			t.Errorf("Parse(%q) accepted a forged identity", uri)
		}
	}
}

func TestValidateName(t *testing.T) {
	for _, ok := range []string{"a", "analyst", "agent-2", "agent_2", "a1"} {
		if err := ValidateName(ok); err != nil {
			t.Errorf("ValidateName(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"", "-lead", "_lead", "Upper", "has space", "a/b", "a.b", "spiffe://x", string(make([]byte, 65))} {
		if err := ValidateName(bad); err == nil {
			t.Errorf("ValidateName(%q) accepted an invalid handle", bad)
		}
	}
}
