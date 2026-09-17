package network

import "testing"

func TestSubnetFor(t *testing.T) {
	cases := []struct {
		idx     uint32
		gateway string
		sandbox string
	}{
		{0, "10.7.0.1", "10.7.0.2"},
		{1, "10.7.0.5", "10.7.0.6"},
		{63, "10.7.0.253", "10.7.0.254"},
		{64, "10.7.1.1", "10.7.1.2"},
		{129, "10.7.2.5", "10.7.2.6"},
	}
	for _, c := range cases {
		gw, sb := subnetFor(c.idx)
		if gw != c.gateway || sb != c.sandbox {
			t.Errorf("subnetFor(%d) = %s, %s; want %s, %s", c.idx, gw, sb, c.gateway, c.sandbox)
		}
	}
}

func TestNftRulesetRedirectsToGateway(t *testing.T) {
	rules := nftRuleset("10.7.0.1", "8443")
	for _, want := range []string{"table ip vora", "hook output", "dnat to 10.7.0.1:8443", "ip daddr != 10.7.0.1"} {
		if !contains(rules, want) {
			t.Errorf("ruleset missing %q:\n%s", want, rules)
		}
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
