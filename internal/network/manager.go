// Package network prepares the per-sandbox network namespace used by sandboxes
// that need egress or peer connectivity (everything except NetworkNone).
//
// Every such sandbox gets a network namespace with a point-to-point veth to the
// runner. What it can then reach depends on the mode:
//
//   - NetworkRestricted: an nftables rule DNATs every outbound TCP connection
//     to the sandbox's egress proxy, and non-gateway traffic is dropped (see
//     docs/adr/012-egress-gateway.md).
//   - NetworkFull: public egress is routed and masqueraded by the runner, while
//     private ranges (RFC1918, link-local, loopback) stay blocked, so sandboxes
//     cannot reach each other, the host LAN, or the metadata service.
//   - NetworkPrivate: no public egress; a sandbox can reach the other sandboxes
//     of its tenant on a shared per-tenant /24 the runner routes between. Every
//     other destination, including the host's private ranges, is dropped.
//
// The policy mirrors ADR 004: default-deny for private ranges, which only
// NetworkPrivate relaxes, and only for the caller's tenant.
//
// Everything here is rootless: the runner runs as uid 0 inside its rootlesskit
// user namespace and owns the namespaces it creates.
package network

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/vorash/vora/internal/runtime"
)

const (
	// DefaultProxyPort is the port the egress proxy listens on, on the
	// gateway address of each sandbox veth.
	DefaultProxyPort = "8443"
	// DefaultBrokerPort is the port the credential broker listens on (ADR 013).
	DefaultBrokerPort = "8080"
	// DefaultDNSPort is the port the sandbox resolver listens on. resolv.conf
	// cannot carry a port, so this is fixed.
	DefaultDNSPort = "53"
	ifacePrefix    = "vora-h"
	peerPrefix     = "vora-s"
	holderCmd      = "sleep"
	holderArg      = "infinity"

	// maxPrivateHostsPerTenant bounds the per-tenant /24 space (10.9.0.0/16).
	maxPrivateTenants = 256
	// maxPrivateHostsPerTenant bounds the tenants per /24. Each sandbox gets
	// its own /30 inside the tenant's /24, so the runner owns a unique gateway
	// address per veth; 64 /30s fit in one /24.
	maxPrivateHostsPerTenant = 64
)

// privateDestinations are the ranges a sandbox must never reach unless its mode
// explicitly permits a peer, per ADR 004.
const privateDestinations = "{ 10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16, 169.254.0.0/16, 127.0.0.0/8 }"

// Enforcement selects where a NetworkRestricted sandbox's egress policy is
// applied. Bubblewrap uses the sandbox netns output hook; gVisor's userspace
// netstack bypasses that hook, so its policy is applied in the runner netns
// instead, on the inbound frames arriving from the sandbox's veth.
type Enforcement string

const (
	// EnforceSandbox applies DNAT in the sandbox's own network namespace.
	EnforceSandbox Enforcement = "sandbox"
	// EnforceRunner applies DNAT in the runner's network namespace, keyed by
	// the sandbox's veth. Required for gVisor.
	EnforceRunner Enforcement = "runner"
)

// Policy selects what a sandbox's network may reach.
type Policy struct {
	// Mode is NetworkRestricted, NetworkFull or NetworkPrivate.
	Mode runtime.NetworkMode
	// TenantID scopes NetworkPrivate peer connectivity. It is required for
	// NetworkPrivate and ignored otherwise.
	TenantID string
	// Enforcement selects where restricted-egress policy is applied. Empty
	// means EnforceSandbox. Ignored for NetworkFull/NetworkPrivate, which are
	// always runner-side.
	Enforcement Enforcement
}

// Net describes one prepared sandbox network.
type Net struct {
	ID          string
	Mode        runtime.NetworkMode
	Enforcement Enforcement
	TenantID    string
	NetnsPID    int
	GatewayIP   string
	SandboxIP   string
	SandboxCIDR string
	ProxyPort   string

	iface  string
	holder *exec.Cmd
}

// NetnsPath is the netns path for nsenter.
func (n *Net) NetnsPath() string { return fmt.Sprintf("/proc/%d/ns/net", n.NetnsPID) }

// ProxyAddr is the address the egress proxy must listen on.
func (n *Net) ProxyAddr() string { return n.ServiceAddr(n.ProxyPort) }

// ServiceAddr is an address on the sandbox gateway at the given port.
func (n *Net) ServiceAddr(port string) string { return net.JoinHostPort(n.GatewayIP, port) }

// ManagerConfig selects the gateway ports the per-sandbox network accepts.
// Empty values fall back to the package defaults.
type ManagerConfig struct {
	// ProxyPort serves the SNI egress proxy.
	ProxyPort string
	// BrokerPort serves the credential broker.
	BrokerPort string
}

// servicePorts are the gateway endpoints a sandbox netns may reach. Everything
// else, including non-TCP protocols, is dropped.
type servicePorts struct {
	proxy  string
	broker string
	dns    string
}

// Manager owns the per-sandbox networks.
type Manager struct {
	logger   *slog.Logger
	ports    servicePorts
	stateDir string

	mu          sync.Mutex
	next        uint32
	nets        map[string]*Net
	tenantIndex map[string]uint32
	tenantHosts map[string]uint32
	nextTenant  uint32
}

// NewManager returns a manager and reaps any networks left behind by a
// previous runner process (fail-closed restart).
func NewManager(stateDir string, mc ManagerConfig, logger *slog.Logger) *Manager {
	if logger == nil {
		logger = slog.Default()
	}
	if mc.ProxyPort == "" {
		mc.ProxyPort = DefaultProxyPort
	}
	if mc.BrokerPort == "" {
		mc.BrokerPort = DefaultBrokerPort
	}
	m := &Manager{
		logger:      logger,
		ports:       servicePorts{proxy: mc.ProxyPort, broker: mc.BrokerPort, dns: DefaultDNSPort},
		stateDir:    filepath.Join(stateDir, "netns"),
		nets:        map[string]*Net{},
		tenantIndex: map[string]uint32{},
		tenantHosts: map[string]uint32{},
	}
	_ = os.MkdirAll(m.stateDir, 0o755)
	m.reapLeftovers()
	return m
}

// Create prepares a network namespace for a sandbox. It is idempotent.
func (m *Manager) Create(ctx context.Context, id string, pol Policy) (*Net, error) {
	if id == "" {
		return nil, fmt.Errorf("network: empty sandbox id")
	}
	switch pol.Mode {
	case runtime.NetworkRestricted, runtime.NetworkFull, runtime.NetworkPrivate:
	default:
		return nil, fmt.Errorf("network: mode %q does not use a namespace", pol.Mode)
	}

	m.mu.Lock()
	if n, ok := m.nets[id]; ok {
		m.mu.Unlock()
		return n, nil
	}
	idx := m.next
	m.next++
	enforcement := pol.Enforcement
	if enforcement == "" {
		enforcement = EnforceSandbox
	}
	n := &Net{ID: id, Mode: pol.Mode, Enforcement: enforcement, TenantID: pol.TenantID, ProxyPort: m.ports.proxy}
	if pol.Mode == runtime.NetworkPrivate {
		if pol.TenantID == "" {
			m.mu.Unlock()
			return nil, fmt.Errorf("network: %q requires a tenant", pol.Mode)
		}
		tIdx, ok := m.tenantIndex[pol.TenantID]
		if !ok {
			if m.nextTenant >= maxPrivateTenants {
				m.mu.Unlock()
				return nil, fmt.Errorf("network: tenant space exhausted")
			}
			tIdx = m.nextTenant
			m.nextTenant++
			m.tenantIndex[pol.TenantID] = tIdx
		}
		host := m.tenantHosts[pol.TenantID]
		if host >= maxPrivateHostsPerTenant {
			m.mu.Unlock()
			return nil, fmt.Errorf("network: tenant %s has too many sandboxes", pol.TenantID)
		}
		m.tenantHosts[pol.TenantID]++
		n.GatewayIP, n.SandboxIP, n.SandboxCIDR = tenantAddresses(tIdx, host)
	} else {
		gw, sb := subnetFor(idx)
		n.GatewayIP, n.SandboxIP, n.SandboxCIDR = gw, sb, gw+"/30"
	}
	n.iface = ifacePrefix + strconv.FormatUint(uint64(idx), 10)
	m.mu.Unlock()

	holder := exec.Command("unshare", "--net", holderCmd, holderArg)
	if err := holder.Start(); err != nil {
		return nil, fmt.Errorf("network: start netns holder: %w", err)
	}
	n.holder = holder
	n.NetnsPID = holder.Process.Pid

	if err := m.setup(ctx, n); err != nil {
		m.teardown(n)
		return nil, err
	}
	m.writeState(n)
	m.mu.Lock()
	m.nets[id] = n
	m.mu.Unlock()
	m.logger.Info("network ready", "sandbox_id", id, "mode", string(n.Mode), "gateway", n.GatewayIP, "sandbox_ip", n.SandboxIP)
	return n, nil
}

// Destroy tears down a sandbox network.
func (m *Manager) Destroy(_ context.Context, id string) error {
	m.mu.Lock()
	n, ok := m.nets[id]
	delete(m.nets, id)
	m.mu.Unlock()
	if !ok {
		if st, err := m.loadState(id); err == nil {
			n = st
		}
	}
	if n == nil {
		return nil
	}
	m.teardown(n)
	m.removeState(id)
	return nil
}

// List returns the ids this manager created.
func (m *Manager) List() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	ids := make([]string, 0, len(m.nets))
	for id := range m.nets {
		ids = append(ids, id)
	}
	return ids
}

func (m *Manager) setup(ctx context.Context, n *Net) error {
	pid := strconv.Itoa(n.NetnsPID)
	if err := run(ctx, "ip", "link", "add", n.iface, "type", "veth", "peer", "name", n.peerIface()); err != nil {
		return err
	}
	if err := run(ctx, "ip", "link", "set", n.peerIface(), "netns", pid); err != nil {
		return err
	}
	if err := run(ctx, "ip", "addr", "add", n.GatewayIP+"/30", "dev", n.iface); err != nil {
		return err
	}
	if err := run(ctx, "ip", "link", "set", n.iface, "up"); err != nil {
		return err
	}
	// For a shared per-tenant /24 the sandbox still uses a /30 veth to its own
	// gateway; peers are reached through the gateway route.
	sbPrefix := "/30"
	steps := [][]string{
		{"ip", "link", "set", "lo", "up"},
		{"ip", "addr", "add", n.SandboxIP + sbPrefix, "dev", n.peerIface()},
		{"ip", "link", "set", n.peerIface(), "up"},
		{"ip", "route", "add", "default", "via", n.GatewayIP},
	}
	for _, s := range steps {
		if err := runIn(ctx, n.NetnsPID, s[0], s[1:]...); err != nil {
			return err
		}
	}
	if n.Enforcement == EnforceRunner && n.Mode == runtime.NetworkRestricted {
		// gVisor: policy lives in the runner netns, on the inbound frames from
		// the sandbox veth. No forwarding is needed; non-DNAT'd traffic is
		// dropped in the forward chain.
		if err := applyRules(ctx, runnerRestrictedRules(n, m.ports)); err != nil {
			return err
		}
		return nil
	}
	if err := m.applyInNetns(ctx, n.NetnsPID, sandboxRules(n, m.ports)); err != nil {
		return err
	}
	if rules := runnerRules(n); rules != "" {
		if err := applyRules(ctx, rules); err != nil {
			return err
		}
		if err := enableForwarding(); err != nil {
			return err
		}
	}
	return nil
}

// applyInNetns loads an nftables ruleset inside the sandbox network namespace.
func (m *Manager) applyInNetns(ctx context.Context, pid int, rules string) error {
	cmd := exec.CommandContext(ctx, "nsenter", "-t", strconv.Itoa(pid), "-n", "nft", "-f", "-")
	cmd.Stdin = strings.NewReader(rules)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("network: nft: %w: %s", err, out)
	}
	return nil
}

// applyRules loads an nftables ruleset in the runner's own network namespace.
func applyRules(ctx context.Context, rules string) error {
	cmd := exec.CommandContext(ctx, "nft", "-f", "-")
	cmd.Stdin = strings.NewReader(rules)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("network: nft: %w: %s", err, out)
	}
	return nil
}

// enableForwarding turns on IPv4 forwarding in the runner's network namespace.
// It is required for NetworkFull (NAT to the uplink) and NetworkPrivate
// (routing between a tenant's sandboxes).
func enableForwarding() error {
	const path = "/proc/sys/net/ipv4/ip_forward"
	if data, err := os.ReadFile(path); err == nil && strings.TrimSpace(string(data)) == "1" {
		return nil
	}
	if err := os.WriteFile(path, []byte("1\n"), 0o644); err != nil {
		return fmt.Errorf("network: enable ip_forward: %w", err)
	}
	return nil
}

func (m *Manager) teardown(n *Net) {
	if n.holder != nil && n.holder.Process != nil {
		_ = n.holder.Process.Kill()
		_, _ = n.holder.Process.Wait()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = run(ctx, "ip", "link", "del", n.iface)
	// Runner-side tables are named per sandbox so removing one cannot disturb
	// another sandbox's forwarding or NAT.
	_ = run(ctx, "nft", "delete", "table", "ip", fwdTable(n.ID))
	_ = run(ctx, "nft", "delete", "table", "ip", natTable(n.ID))
	_ = run(ctx, "nft", "delete", "table", "inet", filtTable(n.ID))
}

func (n *Net) peerIface() string {
	return strings.Replace(n.iface, ifacePrefix, peerPrefix, 1)
}

// writeState records a live network so a later process can reap it.
func (m *Manager) writeState(n *Net) {
	st := netState{
		ID: n.ID, Mode: string(n.Mode), Enforcement: string(n.Enforcement), TenantID: n.TenantID,
		NetnsPID: n.NetnsPID, GatewayIP: n.GatewayIP, SandboxIP: n.SandboxIP,
		SandboxCIDR: n.SandboxCIDR, ProxyPort: n.ProxyPort, Iface: n.iface,
	}
	data, err := json.Marshal(st)
	if err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(m.stateDir, n.ID+".json"), data, 0o644)
}

func (m *Manager) removeState(id string) {
	_ = os.Remove(filepath.Join(m.stateDir, id+".json"))
}

func (m *Manager) loadState(id string) (*Net, error) {
	data, err := os.ReadFile(filepath.Join(m.stateDir, id+".json"))
	if err != nil {
		return nil, err
	}
	var st netState
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, err
	}
	return st.net(), nil
}

// reapLeftovers removes networks recorded by a previous process.
func (m *Manager) reapLeftovers() {
	entries, err := os.ReadDir(m.stateDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(e.Name(), ".json")
		n, err := m.loadState(id)
		if err != nil {
			_ = os.Remove(filepath.Join(m.stateDir, e.Name()))
			continue
		}
		if holderAlive(n.NetnsPID) {
			if p, err := os.FindProcess(n.NetnsPID); err == nil {
				_ = p.Kill()
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		m.teardownContext(ctx, n)
		cancel()
		m.removeState(id)
		m.logger.Warn("reaped leftover sandbox network", "sandbox_id", id)
	}
}

// teardownContext is teardown with a caller-supplied context, used by the
// startup reaper. The holder is already killed by the caller.
func (m *Manager) teardownContext(ctx context.Context, n *Net) {
	_ = run(ctx, "ip", "link", "del", n.iface)
	_ = run(ctx, "nft", "delete", "table", "ip", fwdTable(n.ID))
	_ = run(ctx, "nft", "delete", "table", "ip", natTable(n.ID))
	_ = run(ctx, "nft", "delete", "table", "inet", filtTable(n.ID))
}

// holderAlive reports whether pid is still the netns holder we started, not a
// recycled PID. Missing /proc entries and mismatched command lines are treated
// as not-ours so reapLeftovers will not SIGKILL an unrelated process.
func holderAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		return false
	}
	cmd := strings.ReplaceAll(string(data), "\x00", " ")
	return strings.Contains(cmd, "unshare") && strings.Contains(cmd, holderCmd) && strings.Contains(cmd, holderArg)
}

type netState struct {
	ID          string `json:"id"`
	Mode        string `json:"mode,omitempty"`
	Enforcement string `json:"enforcement,omitempty"`
	TenantID    string `json:"tenant_id,omitempty"`
	NetnsPID    int    `json:"netns_pid"`
	GatewayIP   string `json:"gateway_ip"`
	SandboxIP   string `json:"sandbox_ip"`
	SandboxCIDR string `json:"sandbox_cidr,omitempty"`
	ProxyPort   string `json:"proxy_port"`
	Iface       string `json:"iface"`
}

func (s netState) net() *Net {
	return &Net{
		ID: s.ID, Mode: runtime.NetworkMode(s.Mode), Enforcement: Enforcement(s.Enforcement), TenantID: s.TenantID,
		NetnsPID: s.NetnsPID, GatewayIP: s.GatewayIP, SandboxIP: s.SandboxIP,
		SandboxCIDR: s.SandboxCIDR, ProxyPort: s.ProxyPort, iface: s.Iface,
	}
}

// sandboxRules is the nftables ruleset loaded inside the sandbox namespace.
func sandboxRules(n *Net, ports servicePorts) string {
	switch n.Mode {
	case runtime.NetworkFull:
		// Egress is routed, but private ranges stay blocked so the sandbox
		// cannot reach peers, the host LAN, or the metadata service.
		return fmt.Sprintf(`table inet vora_filter {
	chain out {
		type filter hook output priority 0; policy accept;
		oifname "lo" accept
		ip daddr %s drop
	}
}
`, privateDestinations)
	case runtime.NetworkPrivate:
		// Only the tenant's own /24 is reachable; public egress and every other
		// private range are dropped.
		return fmt.Sprintf(`table inet vora_filter {
	chain out {
		type filter hook output priority 0; policy drop;
		oifname "lo" accept
		ip daddr %s accept
	}
}
`, n.SandboxCIDR)
	default:
		return nftRuleset(n.GatewayIP, ports)
	}
}

// runnerRules is the nftables ruleset loaded in the runner's namespace. It is
// empty for modes that need no runner-side forwarding.
func runnerRules(n *Net) string {
	switch n.Mode {
	case runtime.NetworkFull:
		return fmt.Sprintf(`table ip %s {
	chain forward {
		type filter hook forward priority 0; policy accept;
		ct state established,related accept
		ip saddr %s ip daddr %s drop
	}
}
table ip %s {
	chain postrouting {
		type nat hook postrouting priority 100;
		ip saddr %s masquerade
	}
}
`, fwdTable(n.ID), n.SandboxIP, privateDestinations, natTable(n.ID), n.SandboxIP)
	case runtime.NetworkPrivate:
		return fmt.Sprintf(`table ip %s {
	chain forward {
		type filter hook forward priority 0; policy accept;
		ct state established,related accept
		ip saddr %s ip daddr %s accept
		ip saddr %s drop
	}
}
`, fwdTable(n.ID), n.SandboxIP, n.SandboxCIDR, n.SandboxIP)
	default:
		return ""
	}
}

// fwdTable and natTable are per-sandbox table names. A shared table with a drop
// policy would drop every other sandbox's forwarded traffic when evaluated, so
// each sandbox gets its own policy-accept table that only drops its own
// traffic.
func fwdTable(id string) string  { return tableName("vora_fwd", id) }
func natTable(id string) string  { return tableName("vora_nat", id) }
func filtTable(id string) string { return tableName("vora_filt", id) }

// runnerRestrictedRules is the NetworkRestricted policy for a sandbox whose
// egress frames arrive in the runner netns (gVisor's userspace netstack). It
// mirrors nftRuleset but lives on the runner's inbound path: DNAT the sandbox's
// TCP (and UDP/53) to the gateway services, accept only those service ports for
// the sandbox's interface, and drop everything else it sends, so nothing is
// forwarded onward. Each sandbox gets its own tables with policy accept, so a
// drop here cannot affect another sandbox's traffic.
func runnerRestrictedRules(n *Net, ports servicePorts) string {
	var nat, input strings.Builder
	fmt.Fprintf(&nat, "\t\tiifname %q ip daddr != %s meta l4proto tcp dnat to %s:%s\n",
		n.iface, n.GatewayIP, n.GatewayIP, ports.proxy)
	if ports.dns != "" {
		fmt.Fprintf(&nat, "\t\tiifname %q ip daddr != %s meta l4proto udp th dport %s dnat to %s:%s\n",
			n.iface, n.GatewayIP, ports.dns, n.GatewayIP, ports.dns)
	}
	fmt.Fprintf(&input, "\t\tiifname %q ip daddr %s meta l4proto tcp th dport %s accept\n", n.iface, n.GatewayIP, ports.proxy)
	if ports.broker != "" {
		fmt.Fprintf(&input, "\t\tiifname %q ip daddr %s meta l4proto tcp th dport %s accept\n", n.iface, n.GatewayIP, ports.broker)
	}
	if ports.dns != "" {
		fmt.Fprintf(&input, "\t\tiifname %q ip daddr %s meta l4proto udp th dport %s accept\n", n.iface, n.GatewayIP, ports.dns)
		fmt.Fprintf(&input, "\t\tiifname %q ip daddr %s meta l4proto tcp th dport %s accept\n", n.iface, n.GatewayIP, ports.dns)
	}
	fmt.Fprintf(&input, "\t\tiifname %q drop\n", n.iface)
	return fmt.Sprintf(`table ip %s {
	chain prerouting {
		type nat hook prerouting priority -100; policy accept;
%s	}
}
table inet %s {
	chain input {
		type filter hook input priority 0; policy accept;
%s	}
	chain forward {
		type filter hook forward priority 0; policy accept;
		iifname %q drop
	}
}
`, natTable(n.ID), nat.String(), filtTable(n.ID), input.String(), n.iface)
}

func tableName(prefix, id string) string {
	var b strings.Builder
	b.WriteString(prefix)
	b.WriteByte('_')
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

// nftRuleset transparently redirects non-loopback TCP to the egress proxy and
// non-loopback UDP/53 to the sandbox resolver, then drops every packet that is
// not loopback traffic or addressed to one of the accepted gateway service
// ports. The filter is the enforcement boundary; DNAT alone is insufficient
// because direct gateway and non-TCP traffic would bypass it.
func nftRuleset(gateway string, ports servicePorts) string {
	var nat, accept strings.Builder
	fmt.Fprintf(&nat, "\t\toifname != \"lo\" ip daddr != %s meta l4proto tcp dnat to %s:%s\n", gateway, gateway, ports.proxy)
	if ports.dns != "" {
		// Even a workload with a hardcoded resolver gets fail-closed answers
		// from the gateway instead of a silent drop.
		fmt.Fprintf(&nat, "\t\toifname != \"lo\" ip daddr != %s meta l4proto udp th dport %s dnat to %s:%s\n",
			gateway, ports.dns, gateway, ports.dns)
	}
	fmt.Fprintf(&accept, "\t\tip daddr %s meta l4proto tcp th dport %s accept\n", gateway, ports.proxy)
	if ports.broker != "" {
		fmt.Fprintf(&accept, "\t\tip daddr %s meta l4proto tcp th dport %s accept\n", gateway, ports.broker)
	}
	if ports.dns != "" {
		fmt.Fprintf(&accept, "\t\tip daddr %s meta l4proto udp th dport %s accept\n", gateway, ports.dns)
		fmt.Fprintf(&accept, "\t\tip daddr %s meta l4proto tcp th dport %s accept\n", gateway, ports.dns)
	}
	return fmt.Sprintf(`table ip vora_nat {
	chain out {
		type nat hook output priority -100; policy accept;
%s	}
}
table inet vora_filter {
	chain out {
		type filter hook output priority 0; policy drop;
		oifname "lo" accept
%s	}
}
`, nat.String(), accept.String())
}

// subnetFor returns the gateway and sandbox addresses for the idx-th /30 in
// 10.7.0.0/16.
func subnetFor(idx uint32) (gateway, sandbox string) {
	third := (idx / 64) % 256
	fourth := (idx % 64) * 4
	base := net.IPv4(10, 7, byte(third), byte(fourth)).To4()
	gw := net.IPv4(base[0], base[1], base[2], base[3]+1)
	sb := net.IPv4(base[0], base[1], base[2], base[3]+2)
	return gw.String(), sb.String()
}

// tenantAddresses returns the gateway, sandbox and /24 for the host-th sandbox
// of the tenantIndex-th tenant (10.9.<tenantIndex>.0/24). Each sandbox gets its
// own /30 inside the tenant /24 so the runner has a unique gateway address per
// veth.
func tenantAddresses(tenantIndex, host uint32) (gateway, sandbox, cidr string) {
	t := byte(tenantIndex & 0xff)
	base := byte(host*4 + 1)
	gw := net.IPv4(10, 9, t, base)
	sb := net.IPv4(10, 9, t, base+1)
	return gw.String(), sb.String(), fmt.Sprintf("10.9.%d.0/24", t)
}

func run(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("network: %s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

func runIn(ctx context.Context, pid int, name string, args ...string) error {
	full := append([]string{"-t", strconv.Itoa(pid), "-n", name}, args...)
	return run(ctx, "nsenter", full...)
}
