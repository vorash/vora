package network

import (
	"os"
	"testing"
)

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

func TestHolderAliveRejectsCurrentProcess(t *testing.T) {
	if holderAlive(os.Getpid()) {
		t.Fatal("current test process should not look like a netns holder")
	}
	if holderAlive(0) || holderAlive(-1) {
		t.Fatal("invalid pids must not look alive")
	}
}

func TestNftRulesetRedirectsOnlyTCPToGatewayProxy(t *testing.T) {
	rules := nftRuleset("10.7.0.1", "8443", "8080")
	for _, want := range []string{
		"table ip vora_nat",
		"oifname != \"lo\" ip daddr != 10.7.0.1 meta l4proto tcp dnat to 10.7.0.1:8443",
		"table inet vora_filter",
		"type filter hook output priority 0; policy drop",
		"oifname \"lo\" accept",
		"ip daddr 10.7.0.1 meta l4proto tcp th dport 8443 accept",
		// The broker port is the only other accepted endpoint; everything
		// else, including UDP, stays dropped.
		"ip daddr 10.7.0.1 meta l4proto tcp th dport 8080 accept",
	} {
		if !contains(rules, want) {
			t.Errorf("ruleset missing %q:\n%s", want, rules)
		}
	}
	if contains(rules, "udp") {
		t.Errorf("ruleset accepts UDP before the DNS phase:\n%s", rules)
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
