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
//
// Several runners may share one network namespace (vorash/vora#110). They do
// not coordinate, so every name that must be unique across them is claimed
// from the kernel: the host veth name, whose index also fixes the sandbox /30,
// and an nftables table per private-mode tenant /24. A claim that finds the
// name taken moves on to the next index, and a release only removes a link
// that carries this sandbox's owner label.
package network

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/vorash/vora/internal/runtime"
	"github.com/vorash/vora/internal/ulid"
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

	// maxIndexes bounds the veth indexes, and with them the /30s in
	// 10.7.0.0/16.
	maxIndexes = 16384
	// maxPrivateTenants bounds the per-tenant /24 space (10.9.0.0/16).
	maxPrivateTenants = 256
	// maxPrivateHostsPerTenant bounds the sandboxes per tenant /24. Each
	// sandbox gets its own /30 inside the tenant's /24, so the runner owns a
	// unique gateway address per veth; 64 /30s fit in one /24.
	maxPrivateHostsPerTenant = 64
	// tenantTablePrefix names the empty nftables table that claims a tenant
	// /24 for one runner.
	tenantTablePrefix = "vora_tenant_"
	// ownerPrefix prefixes the ULID that labels one runner's claims.
	ownerPrefix = "own"
)

// privateDestinations are the ranges a sandbox must never reach unless its mode
// explicitly permits a peer, per ADR 004.
const privateDestinations = "{ 0.0.0.0/8, 10.0.0.0/8, 100.64.0.0/10, 127.0.0.0/8, 169.254.0.0/16, 172.16.0.0/12, 192.168.0.0/16, 224.0.0.0/4, 240.0.0.0/4 }"

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

	iface    string
	hostSlot uint32
	tenant   *tenantNet
	holder   netnsHolder
}

// tenantNet is one runner's claim on a private-mode /24. The table claim makes
// the /24 exclusively this runner's, so host slots inside it are allocated in
// memory. The claim is released with the tenant's last sandbox.
type tenantNet struct {
	index    uint32
	live     uint32
	nextHost uint32
	free     []uint32
}

func (t *tenantNet) table() string {
	return tenantTablePrefix + strconv.FormatUint(uint64(t.index), 10)
}

func (t *tenantNet) allocHost() (uint32, bool) {
	var slot uint32
	switch {
	case len(t.free) > 0:
		slot = t.free[len(t.free)-1]
		t.free = t.free[:len(t.free)-1]
	case t.nextHost < maxPrivateHostsPerTenant:
		slot = t.nextHost
		t.nextHost++
	default:
		return 0, false
	}
	t.live++
	return slot, true
}

func (t *tenantNet) freeHost(slot uint32) {
	t.free = append(t.free, slot)
	t.live--
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
	host     host
	// owner labels this runner's tenant /24 claims. It lives in the state
	// directory so a restarted runner releases its own claims and nobody
	// else's, even after a reboot that wiped the kernel objects but not the
	// state files.
	owner string

	mu sync.Mutex
	// next and nextTenant are hints for where to start the next claim. The
	// kernel decides whether an index is free.
	next       uint32
	nextTenant uint32
	nets       map[string]*Net
	tenants    map[string]*tenantNet
}

// NewManager returns a manager and reaps any networks left behind by a
// previous runner process (fail-closed restart).
func NewManager(stateDir string, mc ManagerConfig, logger *slog.Logger) *Manager {
	return newManager(stateDir, mc, logger, shellHost{})
}

func newManager(stateDir string, mc ManagerConfig, logger *slog.Logger, h host) *Manager {
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
		logger:   logger,
		ports:    servicePorts{proxy: mc.ProxyPort, broker: mc.BrokerPort, dns: DefaultDNSPort},
		stateDir: filepath.Join(stateDir, "netns"),
		host:     h,
		nets:     map[string]*Net{},
		tenants:  map[string]*tenantNet{},
	}
	_ = os.MkdirAll(m.stateDir, 0o755)
	m.owner = loadOwner(filepath.Join(m.stateDir, "owner"), logger)
	m.reapLeftovers()
	return m
}

// loadOwner reads the runner's claim label, minting one on first start. A
// label that cannot be persisted is still used, so claims stay labelled; the
// next process then cannot match them and leaves them in place, which is the
// safe side.
func loadOwner(path string, logger *slog.Logger) string {
	if data, err := os.ReadFile(path); err == nil && isOwner(strings.TrimSpace(string(data))) {
		return strings.TrimSpace(string(data))
	}
	owner := ulid.New(ownerPrefix)
	if err := os.WriteFile(path, []byte(owner+"\n"), 0o644); err != nil {
		logger.Warn("cannot persist network owner label; leftovers of this process will not be reaped", "path", path, "error", err)
	}
	return owner
}

// isOwner accepts own_<ULID>, which is also a valid nftables chain name.
func isOwner(s string) bool {
	rest, ok := strings.CutPrefix(s, ownerPrefix+"_")
	if !ok || len(rest) != 26 {
		return false
	}
	for _, r := range rest {
		if !(r >= '0' && r <= '9' || r >= 'A' && r <= 'Z') {
			return false
		}
	}
	return true
}

// Create prepares a network namespace for a sandbox. It is idempotent.
func (m *Manager) Create(ctx context.Context, id string, pol Policy) (*Net, error) {
	if !ulid.ValidPrefix(id, ulid.PrefixSandbox) {
		return nil, fmt.Errorf("network: invalid sandbox id %q", id)
	}
	switch pol.Mode {
	case runtime.NetworkRestricted, runtime.NetworkFull, runtime.NetworkPrivate:
	default:
		return nil, fmt.Errorf("network: mode %q does not use a namespace", pol.Mode)
	}

	if pol.Mode == runtime.NetworkPrivate && pol.TenantID == "" {
		return nil, fmt.Errorf("network: %q requires a tenant", pol.Mode)
	}

	m.mu.Lock()
	if n, ok := m.nets[id]; ok {
		m.mu.Unlock()
		return n, nil
	}
	m.mu.Unlock()
	enforcement := pol.Enforcement
	if enforcement == "" {
		enforcement = EnforceSandbox
	}
	n := &Net{ID: id, Mode: pol.Mode, Enforcement: enforcement, TenantID: pol.TenantID, ProxyPort: m.ports.proxy}

	holder, err := m.host.holdNetns()
	if err != nil {
		return nil, fmt.Errorf("network: start netns holder: %w", err)
	}
	n.holder = holder
	n.NetnsPID = holder.pid()

	if err := m.allocate(ctx, n); err != nil {
		m.release(n)
		return nil, err
	}
	// Persisted before the namespace is plumbed, so a crash from here on is
	// reaped by the next process.
	m.writeState(n)
	if err := m.setup(ctx, n); err != nil {
		m.release(n)
		m.removeState(id)
		return nil, err
	}
	m.mu.Lock()
	m.nets[id] = n
	m.mu.Unlock()
	m.logger.Info("network ready", "sandbox_id", id, "mode", string(n.Mode), "gateway", n.GatewayIP, "sandbox_ip", n.SandboxIP)
	return n, nil
}

// allocate claims the names n needs from the kernel: a tenant /24 first for
// NetworkPrivate, then the veth index. Addresses follow from the claims.
func (m *Manager) allocate(ctx context.Context, n *Net) error {
	if n.Mode == runtime.NetworkPrivate {
		t, slot, err := m.allocatePrivate(ctx, n.TenantID)
		if err != nil {
			return err
		}
		n.tenant, n.hostSlot = t, slot
		n.GatewayIP, n.SandboxIP, n.SandboxCIDR = tenantAddresses(t.index, slot)
	}
	m.mu.Lock()
	start := m.next
	m.mu.Unlock()
	for tries := uint32(0); tries < maxIndexes; tries++ {
		idx := (start + tries) % maxIndexes
		iface := ifacePrefix + strconv.FormatUint(uint64(idx), 10)
		err := m.host.claimLink(ctx, iface, peerPrefix+strconv.FormatUint(uint64(idx), 10), n.NetnsPID, n.ID)
		if errors.Is(err, errExists) {
			continue
		}
		if err != nil {
			return err
		}
		n.iface = iface
		if n.Mode != runtime.NetworkPrivate {
			gw, sb := subnetFor(idx)
			n.GatewayIP, n.SandboxIP, n.SandboxCIDR = gw, sb, gw+"/30"
		}
		m.mu.Lock()
		m.next = (idx + 1) % maxIndexes
		m.mu.Unlock()
		return nil
	}
	return fmt.Errorf("network: subnet space exhausted")
}

// allocatePrivate returns the tenant's /24 and a free host slot in it,
// claiming a /24 from the kernel when this runner holds none for the tenant.
// Lookup and slot allocation happen under one lock so a concurrent release of
// the tenant's last sandbox cannot free the /24 underneath a new one.
func (m *Manager) allocatePrivate(ctx context.Context, tenant string) (*tenantNet, uint32, error) {
	for {
		m.mu.Lock()
		if t, ok := m.tenants[tenant]; ok {
			slot, ok := t.allocHost()
			m.mu.Unlock()
			if !ok {
				return nil, 0, fmt.Errorf("network: tenant %s has too many sandboxes", tenant)
			}
			return t, slot, nil
		}
		start := m.nextTenant
		m.mu.Unlock()

		t, err := m.claimTenant(ctx, start)
		if err != nil {
			return nil, 0, err
		}
		m.mu.Lock()
		if _, ok := m.tenants[tenant]; ok {
			// Another Create claimed a /24 for this tenant meanwhile.
			m.mu.Unlock()
			_ = m.host.releaseTable(ctx, t.table(), m.owner)
			continue
		}
		m.tenants[tenant] = t
		m.nextTenant = (t.index + 1) % maxPrivateTenants
		slot, _ := t.allocHost()
		m.mu.Unlock()
		return t, slot, nil
	}
}

func (m *Manager) claimTenant(ctx context.Context, start uint32) (*tenantNet, error) {
	for tries := uint32(0); tries < maxPrivateTenants; tries++ {
		t := &tenantNet{index: (start + tries) % maxPrivateTenants}
		err := m.host.claimTable(ctx, t.table(), m.owner)
		if errors.Is(err, errExists) {
			continue
		}
		if err != nil {
			return nil, err
		}
		return t, nil
	}
	return nil, fmt.Errorf("network: tenant space exhausted")
}

// release undoes whatever Create had done for n so far: the kernel objects it
// claimed, the holder, and the tenant slot. A link n never claimed is left
// alone, so a failed claim can never touch another runner's interface.
func (m *Manager) release(n *Net) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	m.teardown(ctx, n)
	if n.tenant == nil {
		return
	}
	m.mu.Lock()
	n.tenant.freeHost(n.hostSlot)
	last := n.tenant.live == 0
	if last {
		delete(m.tenants, n.TenantID)
	}
	m.mu.Unlock()
	if last {
		_ = m.host.releaseTable(ctx, n.tenant.table(), m.owner)
	}
}

// Destroy tears down a sandbox network.
func (m *Manager) Destroy(_ context.Context, id string) error {
	if !ulid.ValidPrefix(id, ulid.PrefixSandbox) {
		return fmt.Errorf("network: invalid sandbox id %q", id)
	}
	m.mu.Lock()
	n, ok := m.nets[id]
	delete(m.nets, id)
	m.mu.Unlock()
	if !ok {
		st, err := m.loadState(id)
		if err != nil {
			return nil
		}
		n = st.net()
	}
	m.release(n)
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

// setup plumbs the veth pair allocate claimed and loads the mode's rules.
func (m *Manager) setup(ctx context.Context, n *Net) error {
	if err := m.host.run(ctx, "ip", "addr", "add", n.GatewayIP+"/30", "dev", n.iface); err != nil {
		return err
	}
	if err := m.host.run(ctx, "ip", "link", "set", n.iface, "up"); err != nil {
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
		if err := m.runIn(ctx, n.NetnsPID, s[0], s[1:]...); err != nil {
			return err
		}
	}
	if n.Enforcement == EnforceRunner && n.Mode == runtime.NetworkRestricted {
		// gVisor: policy lives in the runner netns, on the inbound frames from
		// the sandbox veth. No forwarding is needed; non-DNAT'd traffic is
		// dropped in the forward chain.
		return m.host.nft(ctx, 0, runnerRestrictedRules(n, m.ports))
	}
	if err := m.host.nft(ctx, n.NetnsPID, sandboxRules(n, m.ports)); err != nil {
		return err
	}
	if rules := runnerRules(n); rules != "" {
		if err := m.host.nft(ctx, 0, rules); err != nil {
			return err
		}
		if err := m.host.enableForwarding(); err != nil {
			return err
		}
	}
	return nil
}

func (m *Manager) runIn(ctx context.Context, pid int, name string, args ...string) error {
	full := append([]string{"-t", strconv.Itoa(pid), "-n", name}, args...)
	return m.host.run(ctx, "nsenter", full...)
}

// teardown removes the kernel objects n owns. The link goes first, while the
// holder still pins its name: once the holder dies the pair may vanish and
// another runner may claim the name. Only a link labelled with n's id is
// deleted, so a link n never claimed, or one re-claimed by another sandbox
// since this state was written, is left alone.
func (m *Manager) teardown(ctx context.Context, n *Net) {
	if n.iface != "" {
		if err := m.host.releaseLink(ctx, n.iface, n.ID); errors.Is(err, errForeign) {
			m.logger.Warn("leaving interface owned by another sandbox", "sandbox_id", n.ID, "iface", n.iface)
		}
	}
	if n.holder != nil {
		n.holder.kill()
	}
	// Runner-side tables are named per sandbox so removing one cannot disturb
	// another sandbox's forwarding or NAT.
	_ = m.host.run(ctx, "nft", "delete", "table", "ip", fwdTable(n.ID))
	_ = m.host.run(ctx, "nft", "delete", "table", "ip", natTable(n.ID))
	_ = m.host.run(ctx, "nft", "delete", "table", "inet", filtTable(n.ID))
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
	if n.tenant != nil {
		st.TenantTable = n.tenant.table()
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

func (m *Manager) loadState(id string) (netState, error) {
	var st netState
	data, err := os.ReadFile(filepath.Join(m.stateDir, id+".json"))
	if err != nil {
		return st, err
	}
	if err := json.Unmarshal(data, &st); err != nil {
		return st, err
	}
	return st, nil
}

// reapLeftovers removes networks recorded by a previous process, then the
// tenant /24 claims those networks were using. A table named in stale state
// may by now be another runner's, so it is released only if it carries this
// runner's label.
func (m *Manager) reapLeftovers() {
	entries, err := os.ReadDir(m.stateDir)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	tenantTables := map[string]bool{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(e.Name(), ".json")
		st, err := m.loadState(id)
		if err != nil {
			_ = os.Remove(filepath.Join(m.stateDir, e.Name()))
			continue
		}
		if holderAlive(st.NetnsPID) {
			if p, err := os.FindProcess(st.NetnsPID); err == nil {
				_ = p.Kill()
			}
		}
		m.teardown(ctx, st.net())
		if st.TenantTable != "" {
			tenantTables[st.TenantTable] = true
		}
		m.removeState(id)
		m.logger.Warn("reaped leftover sandbox network", "sandbox_id", id)
	}
	for table := range tenantTables {
		if err := m.host.releaseTable(ctx, table, m.owner); errors.Is(err, errForeign) {
			m.logger.Warn("leaving tenant table owned by another runner", "table", table)
		}
	}
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
	// TenantTable is the nftables table that claims this network's tenant
	// /24. Absent in state written before claims existed, so a reap of such
	// state frees no table.
	TenantTable string `json:"tenant_table,omitempty"`
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
		meta nfproto ipv6 drop
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
	chain input {
		type filter hook input priority 0; policy accept;
		iifname %q ct state established,related accept
		iifname %q drop
	}
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
`, fwdTable(n.ID), n.iface, n.iface, n.SandboxIP, privateDestinations, natTable(n.ID), n.SandboxIP)
	case runtime.NetworkPrivate:
		return fmt.Sprintf(`table ip %s {
	chain input {
		type filter hook input priority 0; policy accept;
		iifname %q ct state established,related accept
		iifname %q drop
	}
	chain forward {
		type filter hook forward priority 0; policy accept;
		ct state established,related accept
		ip saddr %s ip daddr %s accept
		ip saddr %s drop
	}
}
`, fwdTable(n.ID), n.iface, n.iface, n.SandboxIP, n.SandboxCIDR, n.SandboxIP)
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
