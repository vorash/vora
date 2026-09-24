//go:build integration

package integration

import (
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/vorash/vora/internal/network"
	"github.com/vorash/vora/internal/runtime"
	"github.com/vorash/vora/internal/ulid"
)

// TestNetworkRestrictedDNAT proves the enforcement primitive: in a prepared
// sandbox netns, every outbound TCP connection goes to the gateway proxy and
// nothing else is reachable. It needs a user namespace with CAP_NET_ADMIN and
// therefore runs under rootlesskit (see `make test-network`).
func TestNetworkRestrictedDNAT(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("needs uid 0 in a user namespace (run under rootlesskit)")
	}
	for _, bin := range []string{"unshare", "ip", "nft", "nsenter", "bash", "timeout"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not found", bin)
		}
	}

	m := network.NewManager(t.TempDir(), network.ManagerConfig{ProxyPort: network.DefaultProxyPort}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	id := ulid.NewSandbox()
	n, err := m.Create(ctx, id, network.Policy{Mode: runtime.NetworkRestricted})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = m.Destroy(context.Background(), id) })

	ln, err := net.Listen("tcp", n.ProxyAddr())
	if err != nil {
		t.Fatalf("listen on %s: %v", n.ProxyAddr(), err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_, _ = c.Write([]byte("PROXY-REACHED\n"))
			_ = c.Close()
		}
	}()

	// Arbitrary TCP egress is redirected to the gateway proxy.
	if out := nsenterRun(t, n.NetnsPID, "bash", "-c", "exec 3<>/dev/tcp/1.2.3.4/80; head -1 <&3"); !strings.Contains(out, "PROXY-REACHED") {
		t.Fatalf("expected the connection to reach the proxy, got %q", out)
	}

	// A live TCP service on the gateway must not be reachable. This proves the
	// policy, rather than relying on unused gateway ports being refused.
	gatewayTCP, err := net.Listen("tcp", net.JoinHostPort(n.GatewayIP, "0"))
	if err != nil {
		t.Fatalf("listen on gateway: %v", err)
	}
	defer gatewayTCP.Close()
	gatewayPort := gatewayTCP.Addr().(*net.TCPAddr).Port
	out, err := nsenterErr(n.NetnsPID, "timeout", "2", "bash", "-c",
		"exec 3<>/dev/tcp/"+n.GatewayIP+"/"+strconv.Itoa(gatewayPort)+" && echo UNEXPECTED")
	if err == nil && strings.Contains(out, "UNEXPECTED") {
		t.Fatalf("live gateway service should be blocked, got %q", out)
	}

	// UDP to the gateway must be dropped, even when a live receiver exists.
	gatewayUDP, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP(n.GatewayIP)})
	if err != nil {
		t.Fatalf("listen UDP on gateway: %v", err)
	}
	defer gatewayUDP.Close()
	// A local EPERM is also an acceptable fail-closed result; regardless of
	// sender behavior, the gateway receiver must observe no datagram.
	_, _ = nsenterErr(n.NetnsPID, "bash", "-c", "printf ping >/dev/udp/"+n.GatewayIP+"/"+strconv.Itoa(gatewayUDP.LocalAddr().(*net.UDPAddr).Port))
	if err := gatewayUDP.SetReadDeadline(time.Now().Add(500 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4)
	if _, _, err := gatewayUDP.ReadFromUDP(buf); err == nil {
		t.Fatal("gateway received UDP that should have been dropped")
	} else if ne, ok := err.(net.Error); !ok || !ne.Timeout() {
		t.Fatalf("read gateway UDP: %v", err)
	}

	// Loopback is private to the sandbox netns and must not be transparently
	// redirected to the gateway proxy.
	out, err = nsenterErr(n.NetnsPID, "timeout", "2", "bash", "-c", "exec 3<>/dev/tcp/127.0.0.1/443; head -1 <&3")
	if err == nil || strings.Contains(out, "PROXY-REACHED") {
		t.Fatalf("loopback unexpectedly escaped to the proxy: %q, %v", out, err)
	}

	// Teardown must remove both manager state and the namespace/veth resources.
	netnsPath := n.NetnsPath()
	if err := m.Destroy(context.Background(), id); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if ids := m.List(); len(ids) != 0 {
		t.Fatalf("networks after destroy = %v, want none", ids)
	}
	if _, err := os.Stat(netnsPath); !os.IsNotExist(err) {
		t.Fatalf("netns still exists after destroy: %s: %v", netnsPath, err)
	}
	if out, err := exec.Command("ip", "link", "show", "vora-h0").CombinedOutput(); err == nil {
		t.Fatalf("host veth still exists after destroy: %s", out)
	}
}

func nsenterRun(t *testing.T, pid int, args ...string) string {
	t.Helper()
	out, err := nsenterErr(pid, args...)
	if err != nil {
		t.Fatalf("nsenter %v: %v: %s", args, err, out)
	}
	return out
}

func nsenterErr(pid int, args ...string) (string, error) {
	full := append([]string{"-t", strconv.Itoa(pid), "-n"}, args...)
	cmd := exec.Command("nsenter", full...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// requireNetnsTools skips unless the test can build a sandbox network, which
// needs uid 0 (in a user namespace) and the netlink/nft tooling.
func requireNetnsTools(t *testing.T) {
	t.Helper()
	if os.Getuid() != 0 {
		t.Skip("needs uid 0 in a user namespace (run under rootlesskit)")
	}
	for _, bin := range []string{"unshare", "ip", "nft", "nsenter", "bash", "timeout"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not found", bin)
		}
	}
}

// TestNetworkFullBlocksPrivateRanges proves that NetworkFull gives routed
// egress without exposing private ranges: RFC1918, link-local (metadata) and
// loopback stay blocked, and the runner's own gateway services are private too.
func TestNetworkFullBlocksPrivateRanges(t *testing.T) {
	requireNetnsTools(t)

	m := network.NewManager(t.TempDir(), network.ManagerConfig{}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	id := ulid.NewSandbox()
	n, err := m.Create(ctx, id, network.Policy{Mode: runtime.NetworkFull})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = m.Destroy(context.Background(), id) })

	for _, dst := range []string{"10.0.0.1", "192.168.1.1", "169.254.169.254"} {
		out, err := nsenterErr(n.NetnsPID, "timeout", "2", "bash", "-c", "exec 3<>/dev/tcp/"+dst+"/80 && echo UNEXPECTED")
		if err == nil || strings.Contains(out, "UNEXPECTED") {
			t.Fatalf("private range %s reachable in NetworkFull: %q (%v)", dst, out, err)
		}
	}

	// A live service on the gateway (which sits in 10/8) must be blocked.
	gw, err := net.Listen("tcp", net.JoinHostPort(n.GatewayIP, "0"))
	if err != nil {
		t.Fatalf("listen on gateway: %v", err)
	}
	defer gw.Close()
	port := strconv.Itoa(gw.Addr().(*net.TCPAddr).Port)
	if out, err := nsenterErr(n.NetnsPID, "timeout", "2", "bash", "-c", "exec 3<>/dev/tcp/"+n.GatewayIP+"/"+port+" && echo UNEXPECTED"); err == nil || strings.Contains(out, "UNEXPECTED") {
		t.Fatalf("gateway service reachable in NetworkFull: %q (%v)", out, err)
	}

	// A host service bound on a routable address is local INPUT, not FORWARD.
	// It must remain unreachable even though full mode permits public egress.
	if hostIP := hostNonLoopbackIPv4(t); hostIP != "" {
		ln, err := net.Listen("tcp", net.JoinHostPort(hostIP, "0"))
		if err != nil {
			t.Fatal(err)
		}
		defer ln.Close()
		port := strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
		if out, err := nsenterErr(n.NetnsPID, "timeout", "2", "bash", "-c", "exec 3<>/dev/tcp/"+hostIP+"/"+port+" && echo UNEXPECTED"); err == nil || strings.Contains(out, "UNEXPECTED") {
			t.Fatalf("host-local TCP service reachable from full sandbox: %q (%v)", out, err)
		}
	}

	// The blocking rule is actually installed in the sandbox namespace.
	if rules := nsenterRun(t, n.NetnsPID, "nft", "list", "ruleset"); !strings.Contains(rules, "169.254.0.0/16") {
		t.Fatalf("private-range drop rule not installed:\n%s", rules)
	}
}

// TestNetworkPrivateIsolatesTenants proves that NetworkPrivate lets two
// sandboxes of one tenant reach each other while denying public egress, other
// tenants, and the host's private ranges.
func TestNetworkPrivateIsolatesTenants(t *testing.T) {
	requireNetnsTools(t)
	serve := buildNetServe(t)

	m := network.NewManager(t.TempDir(), network.ManagerConfig{}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	aID, bID, cID := ulid.NewSandbox(), ulid.NewSandbox(), ulid.NewSandbox()
	a, err := m.Create(ctx, aID, network.Policy{Mode: runtime.NetworkPrivate, TenantID: "tenant-a"})
	if err != nil {
		t.Fatalf("Create a: %v", err)
	}
	b, err := m.Create(ctx, bID, network.Policy{Mode: runtime.NetworkPrivate, TenantID: "tenant-a"})
	if err != nil {
		t.Fatalf("Create b: %v", err)
	}
	c, err := m.Create(ctx, cID, network.Policy{Mode: runtime.NetworkPrivate, TenantID: "tenant-b"})
	if err != nil {
		t.Fatalf("Create c: %v", err)
	}
	t.Cleanup(func() {
		_ = m.Destroy(context.Background(), aID)
		_ = m.Destroy(context.Background(), bID)
		_ = m.Destroy(context.Background(), cID)
	})

	gw, err := net.Listen("tcp", net.JoinHostPort(a.GatewayIP, "0"))
	if err != nil {
		t.Fatal(err)
	}
	defer gw.Close()
	port := strconv.Itoa(gw.Addr().(*net.TCPAddr).Port)
	if out, err := nsenterErr(a.NetnsPID, "timeout", "2", "bash", "-c", "exec 3<>/dev/tcp/"+a.GatewayIP+"/"+port+" && echo UNEXPECTED"); err == nil || strings.Contains(out, "UNEXPECTED") {
		t.Fatalf("host gateway TCP service reachable from private sandbox: %q (%v)", out, err)
	}

	// Serve inside b's namespace.
	addr := net.JoinHostPort(b.SandboxIP, "9999")
	srv := exec.Command("nsenter", "-t", strconv.Itoa(b.NetnsPID), "-n", serve, "-addr", addr)
	if err := srv.Start(); err != nil {
		t.Fatalf("start netserve: %v", err)
	}
	t.Cleanup(func() { _ = srv.Process.Kill(); _, _ = srv.Process.Wait() })
	waitForTCP(t, addr)

	// Same tenant: a reaches b.
	out := nsenterRun(t, a.NetnsPID, "bash", "-c", "exec 3<>/dev/tcp/"+b.SandboxIP+"/9999; head -1 <&3")
	if !strings.Contains(out, "NETSERVE") {
		t.Fatalf("same-tenant peer unreachable: %q", out)
	}

	// Different tenant: a must not reach c.
	if out, err := nsenterErr(a.NetnsPID, "timeout", "2", "bash", "-c", "exec 3<>/dev/tcp/"+c.SandboxIP+"/9999 && echo UNEXPECTED"); err == nil || strings.Contains(out, "UNEXPECTED") {
		t.Fatalf("cross-tenant peer reachable: %q (%v)", out, err)
	}

	// Public egress is denied.
	if out, err := nsenterErr(a.NetnsPID, "timeout", "2", "bash", "-c", "exec 3<>/dev/tcp/1.1.1.1/80 && echo UNEXPECTED"); err == nil || strings.Contains(out, "UNEXPECTED") {
		t.Fatalf("public egress allowed in NetworkPrivate: %q (%v)", out, err)
	}
}

func hostNonLoopbackIPv4(t *testing.T) string {
	t.Helper()
	ifaces, err := net.Interfaces()
	if err != nil {
		t.Fatal(err)
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			t.Fatal(err)
		}
		for _, addr := range addrs {
			if network, ok := addr.(*net.IPNet); ok && network.IP.To4() != nil {
				return network.IP.String()
			}
		}
	}
	return ""
}

func buildNetServe(t *testing.T) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "netserve")
	cmd := exec.Command("go", "build", "-o", out, "./tests/integration/testdata/netserve")
	cmd.Dir = filepath.Join("..", "..")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build netserve: %v\n%s", err, b)
	}
	return out
}

func waitForTCP(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond); err == nil {
			_ = c.Close()
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("listener %s not ready", addr)
}
