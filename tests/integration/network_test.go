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
	for _, bin := range []string{"unshare", "ip", "nft", "nsenter", "bash"} {
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

	// The gateway's non-proxy port must stay refused (no re-DNAT loop).
	out, err := nsenterErr(n.NetnsPID, "bash", "-c", "exec 3<>/dev/tcp/"+n.GatewayIP+"/9999 && echo UNEXPECTED")
	if err == nil && strings.Contains(out, "UNEXPECTED") {
		t.Fatalf("gateway port 9999 should be refused, got %q", out)
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
