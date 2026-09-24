package network

import (
	"context"
	"os"
	"testing"

	"github.com/vorash/vora/internal/runtime"
	"github.com/vorash/vora/internal/ulid"
)

func TestManagerRejectsNonCanonicalIDs(t *testing.T) {
	m := NewManager(t.TempDir(), ManagerConfig{}, nil)
	for _, id := range []string{".hidden", "-flag", "x.slice", "sbx_short"} {
		if _, err := m.Create(context.Background(), id, Policy{Mode: runtime.NetworkFull}); err == nil {
			t.Errorf("manager accepted unsafe id %q", id)
		}
	}
}

func TestAllocatorFailsBeforeSubnetWrap(t *testing.T) {
	m := NewManager(t.TempDir(), ManagerConfig{}, nil)
	m.next = 16384
	if _, err := m.Create(context.Background(), ulid.NewSandbox(), Policy{Mode: runtime.NetworkFull}); err == nil {
		t.Fatal("wrapped subnet index can reuse a live sandbox address")
	}
}

func TestPrivateHostSlotIsReusable(t *testing.T) {
	m := NewManager(t.TempDir(), ManagerConfig{}, nil)
	m.tenantHosts["tenant"] = maxPrivateHostsPerTenant
	m.releaseTenantHost(&Net{Mode: runtime.NetworkPrivate, TenantID: "tenant", hostSlot: 7})
	m.mu.Lock()
	slot, ok := m.allocateTenantHost("tenant")
	m.mu.Unlock()
	if !ok || slot != 7 {
		t.Fatalf("freed slot unavailable: slot=%d ok=%v", slot, ok)
	}
}

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
	rules := nftRuleset("10.7.0.1", servicePorts{proxy: "8443", broker: "8080", dns: "53"})
	for _, want := range []string{
		"table ip vora_nat",
		"oifname != \"lo\" ip daddr != 10.7.0.1 meta l4proto tcp dnat to 10.7.0.1:8443",
		"table inet vora_filter",
		"type filter hook output priority 0; policy drop",
		"oifname \"lo\" accept",
		"ip daddr 10.7.0.1 meta l4proto tcp th dport 8443 accept",
		// The broker port is the only other accepted TCP endpoint.
		"ip daddr 10.7.0.1 meta l4proto tcp th dport 8080 accept",
		// DNS goes to the gateway resolver on 53 (UDP for queries, TCP for
		// truncated answers) and a hardcoded resolver is redirected there
		// rather than silently dropped.
		"oifname != \"lo\" ip daddr != 10.7.0.1 meta l4proto udp th dport 53 dnat to 10.7.0.1:53",
		"ip daddr 10.7.0.1 meta l4proto udp th dport 53 accept",
		"ip daddr 10.7.0.1 meta l4proto tcp th dport 53 accept",
	} {
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

func TestTenantAddresses(t *testing.T) {
	cases := []struct {
		tenant, host           uint32
		gateway, sandbox, cidr string
	}{
		{0, 0, "10.9.0.1", "10.9.0.2", "10.9.0.0/24"},
		{0, 1, "10.9.0.5", "10.9.0.6", "10.9.0.0/24"},
		{3, 0, "10.9.3.1", "10.9.3.2", "10.9.3.0/24"},
		{255, 63, "10.9.255.253", "10.9.255.254", "10.9.255.0/24"},
	}
	for _, c := range cases {
		gw, sb, cidr := tenantAddresses(c.tenant, c.host)
		if gw != c.gateway || sb != c.sandbox || cidr != c.cidr {
			t.Errorf("tenantAddresses(%d,%d) = %s, %s, %s; want %s, %s, %s",
				c.tenant, c.host, gw, sb, cidr, c.gateway, c.sandbox, c.cidr)
		}
	}
}

func TestFullSandboxRulesBlockPrivateRanges(t *testing.T) {
	n := &Net{ID: "sbx_full", Mode: runtime.NetworkFull, GatewayIP: "10.7.0.1", SandboxIP: "10.7.0.2"}
	rules := sandboxRules(n, servicePorts{})
	for _, want := range []string{
		"type filter hook output priority 0; policy accept",
		"oifname \"lo\" accept",
		"meta nfproto ipv6 drop",
		"100.64.0.0/10",
		"ip daddr { 0.0.0.0/8, 10.0.0.0/8, 100.64.0.0/10, 127.0.0.0/8, 169.254.0.0/16, 172.16.0.0/12, 192.168.0.0/16, 224.0.0.0/4, 240.0.0.0/4 } drop",
	} {
		if !contains(rules, want) {
			t.Errorf("full sandbox rules missing %q:\n%s", want, rules)
		}
	}
	// Full mode must not DNAT through the restricted proxy.
	if contains(rules, "dnat to") {
		t.Errorf("full sandbox rules must not redirect to the proxy:\n%s", rules)
	}
}

func TestPrivateSandboxRulesAllowOnlyTenantSubnet(t *testing.T) {
	n := &Net{ID: "sbx_priv", Mode: runtime.NetworkPrivate, GatewayIP: "10.9.3.1", SandboxIP: "10.9.3.2", SandboxCIDR: "10.9.3.0/24"}
	rules := sandboxRules(n, servicePorts{})
	if !contains(rules, "type filter hook output priority 0; policy drop") {
		t.Errorf("private sandbox rules must default-deny:\n%s", rules)
	}
	if !contains(rules, "ip daddr 10.9.3.0/24 accept") {
		t.Errorf("private sandbox rules must allow the tenant subnet:\n%s", rules)
	}
	// No public egress: every non-tenant destination falls through to drop.
	if contains(rules, "policy accept") {
		t.Errorf("private sandbox rules must not have an accept policy:\n%s", rules)
	}
}

func TestFullRunnerRulesMasquerade(t *testing.T) {
	n := &Net{ID: "sbx_full", Mode: runtime.NetworkFull, SandboxIP: "10.7.0.2"}
	rules := runnerRules(n)
	for _, want := range []string{
		"table ip vora_fwd_sbx_full",
		"ip saddr 10.7.0.2 ip daddr { 0.0.0.0/8, 10.0.0.0/8, 100.64.0.0/10, 127.0.0.0/8, 169.254.0.0/16, 172.16.0.0/12, 192.168.0.0/16, 224.0.0.0/4, 240.0.0.0/4 } drop",
		"table ip vora_nat_sbx_full",
		"type nat hook postrouting priority 100",
		"ip saddr 10.7.0.2 masquerade",
	} {
		if !contains(rules, want) {
			t.Errorf("full runner rules missing %q:\n%s", want, rules)
		}
	}
}

func TestPrivateRunnerRulesAllowOnlyTenantSubnet(t *testing.T) {
	n := &Net{ID: "sbx_priv", Mode: runtime.NetworkPrivate, SandboxIP: "10.9.3.2", SandboxCIDR: "10.9.3.0/24"}
	rules := runnerRules(n)
	for _, want := range []string{
		"table ip vora_fwd_sbx_priv",
		"ip saddr 10.9.3.2 ip daddr 10.9.3.0/24 accept",
		"ip saddr 10.9.3.2 drop",
	} {
		if !contains(rules, want) {
			t.Errorf("private runner rules missing %q:\n%s", want, rules)
		}
	}
	// No NAT: private traffic stays inside the runner's namespace.
	if contains(rules, "masquerade") {
		t.Errorf("private runner rules must not masquerade:\n%s", rules)
	}
}

func TestFullAndPrivateRunnerRulesDropHostInput(t *testing.T) {
	for _, mode := range []runtime.NetworkMode{runtime.NetworkFull, runtime.NetworkPrivate} {
		n := &Net{ID: "sbx_test", Mode: mode, SandboxIP: "10.7.0.2", SandboxCIDR: "10.9.0.0/24", iface: "veth-test"}
		rules := runnerRules(n)
		for _, want := range []string{"type filter hook input", "iifname \"veth-test\" ct state established,related accept", "iifname \"veth-test\" drop"} {
			if !contains(rules, want) {
				t.Errorf("%s allows traffic to host: missing %q in %s", mode, want, rules)
			}
		}
	}
}

func TestRestrictedRunnerRulesAreEmpty(t *testing.T) {
	n := &Net{ID: "sbx_r", Mode: runtime.NetworkRestricted, SandboxIP: "10.7.0.2"}
	if rules := runnerRules(n); rules != "" {
		t.Fatalf("restricted mode needs no runner-side rules, got:\n%s", rules)
	}
}

func TestTableNameIsSanitized(t *testing.T) {
	if got := tableName("vora_fwd", "sbx-01.H/x"); got != "vora_fwd_sbx_01_H_x" {
		t.Fatalf("tableName = %q", got)
	}
}

func TestRunnerRestrictedRules(t *testing.T) {
	n := &Net{
		ID: "sbx_gv", Mode: runtime.NetworkRestricted, Enforcement: EnforceRunner,
		GatewayIP: "10.7.0.1", SandboxIP: "10.7.0.2", iface: "vora-h0",
	}
	rules := runnerRestrictedRules(n, servicePorts{proxy: "8443", broker: "8080", dns: "53"})
	for _, want := range []string{
		"table ip vora_nat_sbx_gv",
		"type nat hook prerouting priority -100",
		`iifname "vora-h0" ip daddr != 10.7.0.1 meta l4proto tcp dnat to 10.7.0.1:8443`,
		`iifname "vora-h0" ip daddr != 10.7.0.1 meta l4proto udp th dport 53 dnat to 10.7.0.1:53`,
		"table inet vora_filt_sbx_gv",
		`iifname "vora-h0" ip daddr 10.7.0.1 meta l4proto tcp th dport 8443 accept`,
		`iifname "vora-h0" ip daddr 10.7.0.1 meta l4proto tcp th dport 8080 accept`,
		`iifname "vora-h0" ip daddr 10.7.0.1 meta l4proto udp th dport 53 accept`,
		// Everything else from the sandbox is dropped, and nothing is forwarded.
		`iifname "vora-h0" drop`,
	} {
		if !contains(rules, want) {
			t.Errorf("runner restricted rules missing %q:\n%s", want, rules)
		}
	}
}
