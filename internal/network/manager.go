// Package network prepares the per-sandbox network namespace used by
// NetworkRestricted sandboxes.
//
// A sandbox gets a network namespace with a point-to-point veth to the runner
// and an nftables rule that DNATs every outbound TCP connection to the
// sandbox's egress proxy (see docs/adr/012-egress-gateway.md). The sandbox can
// only reach the proxy; the proxy decides what is allowed by TLS SNI.
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
)

// Net describes one prepared sandbox network.
type Net struct {
	ID        string
	NetnsPID  int
	GatewayIP string
	SandboxIP string
	ProxyPort string

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

	mu   sync.Mutex
	next uint32
	nets map[string]*Net
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
		logger:   logger,
		ports:    servicePorts{proxy: mc.ProxyPort, broker: mc.BrokerPort, dns: DefaultDNSPort},
		stateDir: filepath.Join(stateDir, "netns"),
		nets:     map[string]*Net{},
	}
	_ = os.MkdirAll(m.stateDir, 0o755)
	m.reapLeftovers()
	return m
}

// Create prepares a network namespace for a sandbox. It is idempotent.
func (m *Manager) Create(ctx context.Context, id string) (*Net, error) {
	if id == "" {
		return nil, fmt.Errorf("network: empty sandbox id")
	}
	m.mu.Lock()
	if n, ok := m.nets[id]; ok {
		m.mu.Unlock()
		return n, nil
	}
	idx := m.next
	m.next++
	m.mu.Unlock()

	gw, sb := subnetFor(idx)
	n := &Net{
		ID:        id,
		GatewayIP: gw,
		SandboxIP: sb,
		ProxyPort: m.ports.proxy,
		iface:     ifacePrefix + strconv.FormatUint(uint64(idx), 10),
	}

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
	m.logger.Info("network ready", "sandbox_id", id, "gateway", n.GatewayIP, "sandbox_ip", n.SandboxIP)
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
	steps := [][]string{
		{"ip", "link", "set", "lo", "up"},
		{"ip", "addr", "add", n.SandboxIP + "/30", "dev", n.peerIface()},
		{"ip", "link", "set", n.peerIface(), "up"},
		{"ip", "route", "add", "default", "via", n.GatewayIP},
	}
	for _, s := range steps {
		if err := runIn(ctx, n.NetnsPID, s[0], s[1:]...); err != nil {
			return err
		}
	}
	rules := nftRuleset(n.GatewayIP, m.ports)
	cmd := exec.CommandContext(ctx, "nsenter", "-t", pid, "-n", "nft", "-f", "-")
	cmd.Stdin = strings.NewReader(rules)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("network: nft: %w: %s", err, out)
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
}

func (n *Net) peerIface() string {
	return strings.Replace(n.iface, ifacePrefix, peerPrefix, 1)
}

// writeState records a live network so a later process can reap it.
func (m *Manager) writeState(n *Net) {
	st := netState{ID: n.ID, NetnsPID: n.NetnsPID, GatewayIP: n.GatewayIP, SandboxIP: n.SandboxIP, ProxyPort: n.ProxyPort, Iface: n.iface}
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
		_ = run(ctx, "ip", "link", "del", n.iface)
		cancel()
		m.removeState(id)
		m.logger.Warn("reaped leftover sandbox network", "sandbox_id", id)
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
	ID        string `json:"id"`
	NetnsPID  int    `json:"netns_pid"`
	GatewayIP string `json:"gateway_ip"`
	SandboxIP string `json:"sandbox_ip"`
	ProxyPort string `json:"proxy_port"`
	Iface     string `json:"iface"`
}

func (s netState) net() *Net {
	return &Net{ID: s.ID, NetnsPID: s.NetnsPID, GatewayIP: s.GatewayIP, SandboxIP: s.SandboxIP, ProxyPort: s.ProxyPort, iface: s.Iface}
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
