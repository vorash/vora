//go:build integration

package integration

import (
	"context"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/vorash/vora/internal/network"
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

	m := network.NewManager(t.TempDir(), network.DefaultProxyPort, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	n, err := m.Create(ctx, "sbx_net_test")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = m.Destroy(context.Background(), "sbx_net_test") })

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
	if err := m.Destroy(context.Background(), "sbx_net_test"); err != nil {
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
