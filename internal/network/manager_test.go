package network

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
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

// testRunner is one runner process: a manager with its own state directory on
// a host it may share with other runners.
type testRunner struct {
	t        *testing.T
	m        *Manager
	stateDir string
}

func newTestRunner(t *testing.T, h host) *testRunner {
	dir := t.TempDir()
	return &testRunner{t: t, m: newManager(dir, ManagerConfig{}, slog.New(slog.NewTextHandler(io.Discard, nil)), h), stateDir: dir}
}

// restart is the runner process coming back on the same state directory.
func (r *testRunner) restart(h host) *testRunner {
	return &testRunner{t: r.t, m: newManager(r.stateDir, ManagerConfig{}, slog.New(slog.NewTextHandler(io.Discard, nil)), h), stateDir: r.stateDir}
}

func (r *testRunner) create(pol Policy) *Net {
	r.t.Helper()
	n, err := r.m.Create(context.Background(), ulid.NewSandbox(), pol)
	if err != nil {
		r.t.Fatalf("Create(%s): %v", pol.Mode, err)
	}
	return n
}

func (r *testRunner) destroy(n *Net) {
	r.t.Helper()
	if err := r.m.Destroy(context.Background(), n.ID); err != nil {
		r.t.Fatalf("Destroy(%s): %v", n.ID, err)
	}
}

func (r *testRunner) stateFiles() []string {
	files, _ := filepath.Glob(filepath.Join(r.stateDir, "netns", "*.json"))
	return files
}

func restricted() Policy           { return Policy{Mode: runtime.NetworkRestricted} }
func full() Policy                 { return Policy{Mode: runtime.NetworkFull} }
func private(tenant string) Policy { return Policy{Mode: runtime.NetworkPrivate, TenantID: tenant} }

func expectAddrs(t *testing.T, n *Net, iface, gateway, sandbox, cidr string) {
	t.Helper()
	if n.iface != iface || n.GatewayIP != gateway || n.SandboxIP != sandbox || n.SandboxCIDR != cidr {
		t.Errorf("%s: got %s %s %s %s; want %s %s %s %s", n.Mode,
			n.iface, n.GatewayIP, n.SandboxIP, n.SandboxCIDR, iface, gateway, sandbox, cidr)
	}
}

// A single fresh runner keeps the names and addresses ADR 012 and ADR 016
// document, so existing deployments and docs stay valid.
func TestFreshRunnerKeepsDocumentedNamesAndAddresses(t *testing.T) {
	r := newTestRunner(t, newFakeHost())
	expectAddrs(t, r.create(restricted()), "vora-h0", "10.7.0.1", "10.7.0.2", "10.7.0.1/30")
	expectAddrs(t, r.create(full()), "vora-h1", "10.7.0.5", "10.7.0.6", "10.7.0.5/30")
	expectAddrs(t, r.create(private("a")), "vora-h2", "10.9.0.1", "10.9.0.2", "10.9.0.0/24")
	expectAddrs(t, r.create(private("a")), "vora-h3", "10.9.0.5", "10.9.0.6", "10.9.0.0/24")
	expectAddrs(t, r.create(private("b")), "vora-h4", "10.9.1.1", "10.9.1.2", "10.9.1.0/24")
}

// Two runners in one network namespace never hand out the same interface,
// gateway or tenant /24, even for the same tenant id on both.
func TestRunnersSharingAHostNeverCollide(t *testing.T) {
	h := newFakeHost()
	a, b := newTestRunner(t, h), newTestRunner(t, h)
	nets := []*Net{
		a.create(restricted()), b.create(restricted()),
		a.create(full()), b.create(full()),
		a.create(private("tenant")), b.create(private("tenant")),
	}
	ifaces, gateways := map[string]bool{}, map[string]bool{}
	for _, n := range nets {
		if ifaces[n.iface] {
			t.Errorf("interface %s handed out twice", n.iface)
		}
		if gateways[n.GatewayIP] {
			t.Errorf("gateway %s handed out twice", n.GatewayIP)
		}
		ifaces[n.iface], gateways[n.GatewayIP] = true, true
	}
	if ca, cb := nets[4].SandboxCIDR, nets[5].SandboxCIDR; ca == cb {
		t.Errorf("tenant /24 %s shared across runners: a private sandbox on one runner could reach another runner's tenant", ca)
	}
	if h.linkCount() != len(nets) {
		t.Errorf("host has %d links for %d sandboxes", h.linkCount(), len(nets))
	}
}

// A create that fails after claiming its interface removes only that
// interface, and one that fails before claiming removes nothing.
func TestFailedCreateLeavesOtherRunnersInterfaces(t *testing.T) {
	h := newFakeHost()
	a, b := newTestRunner(t, h), newTestRunner(t, h)
	live := a.create(restricted())
	livePrivate := a.create(private("tenant"))

	h.runErr = errors.New("ip addr add: boom")
	if _, err := b.m.Create(context.Background(), ulid.NewSandbox(), private("other")); err == nil {
		t.Fatal("Create succeeded although plumbing failed")
	}
	h.runErr = nil
	h.claimErr = errors.New("ip link add: Operation not permitted")
	if _, err := b.m.Create(context.Background(), ulid.NewSandbox(), restricted()); err == nil {
		t.Fatal("Create succeeded although the claim failed")
	}
	h.claimErr = nil

	for _, n := range []*Net{live, livePrivate} {
		if owner, ok := h.owner(n.iface); !ok || owner != n.ID {
			t.Errorf("%s: runner A's interface gone or relabelled after runner B's failed create (owner %q, present %v)", n.iface, owner, ok)
		}
	}
	if h.linkCount() != 2 {
		t.Errorf("host has %d links, want runner A's 2: a failed create left its own behind", h.linkCount())
	}
	if h.hasTable(tenantTablePrefix + "1") {
		t.Error("runner B's failed private create kept its tenant /24 claim")
	}
	if !h.hasTable(tenantTablePrefix + "0") {
		t.Error("runner A's tenant /24 claim was released by runner B's failure")
	}
	if files := b.stateFiles(); len(files) != 0 {
		t.Errorf("runner B recorded state for failed creates: %v", files)
	}
}

// A restarted runner reaps what its previous process owned and nothing else,
// even when another runner has since claimed one of its old interface names.
func TestRestartReapsOnlyWhatThisRunnerOwns(t *testing.T) {
	h := newFakeHost()
	a := newTestRunner(t, h)
	aRestricted := a.create(restricted())
	aPrivate := a.create(private("tenant"))
	// Runner A dies. The restricted sandbox's namespace dies with it, so its
	// pair vanishes; the private sandbox's namespace outlives the runner.
	h.vanish(aRestricted.iface)

	b := newTestRunner(t, h)
	bNet := b.create(restricted())
	if bNet.iface != aRestricted.iface {
		t.Fatalf("runner B claimed %s, test needs it to reuse %s", bNet.iface, aRestricted.iface)
	}

	a2 := a.restart(h)
	if owner, ok := h.owner(bNet.iface); !ok || owner != bNet.ID {
		t.Errorf("runner A's restart removed runner B's live interface %s (owner %q, present %v)", bNet.iface, owner, ok)
	}
	if _, ok := h.owner(aPrivate.iface); ok {
		t.Errorf("runner A's restart left its own leftover %s", aPrivate.iface)
	}
	if h.hasTable(tenantTablePrefix + "0") {
		t.Error("runner A's restart left its tenant /24 claimed")
	}
	if files := a2.stateFiles(); len(files) != 0 {
		t.Errorf("stale state survived the restart: %v", files)
	}
	// The reclaimed /24 is free for the restarted runner.
	expectAddrs(t, a2.create(private("tenant")), "vora-h1", "10.9.0.1", "10.9.0.2", "10.9.0.0/24")
}

// A tenant's /24 is held while any of its sandboxes lives and returned with
// the last one, so the 256-tenant space is not consumed by tenant churn.
func TestTenantClaimReleasedWithLastSandbox(t *testing.T) {
	h := newFakeHost()
	r := newTestRunner(t, h)
	first, second := r.create(private("tenant")), r.create(private("tenant"))
	r.destroy(first)
	if !h.hasTable(tenantTablePrefix + "0") {
		t.Fatal("tenant /24 released while a sandbox still uses it")
	}
	r.destroy(second)
	if h.hasTable(tenantTablePrefix + "0") {
		t.Fatal("tenant /24 kept after its last sandbox")
	}
	for i := 0; i < maxPrivateTenants+1; i++ {
		r.destroy(r.create(private(fmt.Sprintf("tenant-%d", i))))
	}
	r.create(private("another"))
}

func TestPrivateHostSlotIsReusable(t *testing.T) {
	r := newTestRunner(t, newFakeHost())
	nets := make([]*Net, maxPrivateHostsPerTenant)
	for i := range nets {
		nets[i] = r.create(private("tenant"))
	}
	if _, err := r.m.Create(context.Background(), ulid.NewSandbox(), private("tenant")); err == nil {
		t.Fatal("65th sandbox fit in a /24 of 64 /30s")
	}
	r.destroy(nets[7])
	if n := r.create(private("tenant")); n.SandboxIP != nets[7].SandboxIP {
		t.Fatalf("freed slot %s not reused, got %s", nets[7].SandboxIP, n.SandboxIP)
	}
}

// Indexes wrap, so a long-lived runner is not exhausted after 16384 sandboxes;
// only a host with every index live refuses a create.
func TestIndexWrapsAndFailsOnlyWhenAllHeld(t *testing.T) {
	h := newFakeHost()
	r := newTestRunner(t, h)
	r.m.next = maxIndexes - 1
	expectAddrs(t, r.create(restricted()), "vora-h16383", "10.7.255.253", "10.7.255.254", "10.7.255.253/30")
	expectAddrs(t, r.create(restricted()), "vora-h0", "10.7.0.1", "10.7.0.2", "10.7.0.1/30")

	for i := 0; i < maxIndexes; i++ {
		h.links[fmt.Sprintf("vora-h%d", i)] = "someone-else"
	}
	if _, err := r.m.Create(context.Background(), ulid.NewSandbox(), restricted()); err == nil {
		t.Fatal("Create succeeded with every index held")
	}
	if h.linkCount() != maxIndexes {
		t.Fatalf("failed create changed the host: %d links", h.linkCount())
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
