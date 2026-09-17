//go:build integration

package integration

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vorash/vora/internal/egress"
	"github.com/vorash/vora/internal/network"
)

// TestDNSThroughNetns proves the gateway resolver from inside a real sandbox
// netns: an allowlisted name resolves, a non-allowlisted name does not, a
// hardcoded resolver is redirected to the gateway (fail closed, not dropped),
// and a name that resolves to a loopback address is refused. It needs uid 0 in
// a user namespace (run under rootlesskit, like `make test-dns`) and a working
// host resolver.
func TestDNSThroughNetns(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("needs uid 0 in a user namespace (run under rootlesskit)")
	}
	for _, bin := range []string{"unshare", "ip", "nft", "nsenter", "python3"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not found", bin)
		}
	}
	const (
		allowHost  = "example.com"
		deniedHost = "iana.org"
		rebindHost = "localtest.me" // resolves to 127.0.0.1
	)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if _, err := net.DefaultResolver.LookupIPAddr(ctx, allowHost); err != nil {
		t.Skipf("host resolver cannot resolve %s: %v", allowHost, err)
	}
	rebindResolves := true
	if _, err := net.DefaultResolver.LookupIPAddr(ctx, rebindHost); err != nil {
		rebindResolves = false
		t.Logf("host resolver cannot resolve %s; skipping the rebinding assertion: %v", rebindHost, err)
	}

	var (
		mu     sync.Mutex
		events []egress.Event
	)
	sink := func(e egress.Event) error {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, e)
		return nil
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	manager := network.NewManager(t.TempDir(), network.ManagerConfig{
		ProxyPort:  network.DefaultProxyPort,
		BrokerPort: network.DefaultBrokerPort,
	}, logger)

	n, err := manager.Create(ctx, "sbx_dns_netns")
	if err != nil {
		t.Fatalf("network Create: %v", err)
	}
	t.Cleanup(func() { _ = manager.Destroy(context.Background(), "sbx_dns_netns") })

	resolver := egress.NewDNS("sbx_dns_netns",
		egress.Policy{AllowedDomains: []string{allowHost, rebindHost}}, logger, sink)
	dnsCtx, stopDNS := context.WithCancel(context.Background())
	t.Cleanup(stopDNS)
	if err := resolver.Listen(dnsCtx, n.ServiceAddr(network.DefaultDNSPort)); err != nil {
		t.Fatalf("dns Listen: %v", err)
	}
	t.Cleanup(func() { _ = resolver.Close() })

	rebindCheck := ""
	if rebindResolves {
		rebindCheck = fmt.Sprintf(`
rcode, answers = query(%q, gw)
print("REBIND", rcode, answers)
`, rebindHost)
	}
	script := fmt.Sprintf(`
import random, socket, struct

gw = %q

def qname(name):
    out = b""
    for label in name.split("."):
        out += bytes([len(label)]) + label.encode()
    return out + b"\x00"

def query(name, server):
    tid = random.randint(0, 65535)
    pkt = struct.pack(">HHHHHH", tid, 0x0100, 1, 0, 0, 0) + qname(name) + struct.pack(">HH", 1, 1)
    s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
    s.settimeout(5)
    s.sendto(pkt, (server, 53))
    data, _ = s.recvfrom(2048)
    _, flags, _, answers, _, _ = struct.unpack(">HHHHHH", data[:12])
    return flags & 0xF, answers

rcode, answers = query(%q, gw)
print("ALLOWED", rcode, answers)
rcode, answers = query(%q, gw)
print("DENIED", rcode, answers)
rcode, answers = query(%q, "8.8.8.8")
print("DNAT", rcode, answers)
%s`, n.GatewayIP, allowHost, deniedHost, allowHost, rebindCheck)

	out := nsenterRun(t, n.NetnsPID, "python3", "-c", script)
	t.Logf("resolver probe:\n%s", out)
	lines := map[string][]string{}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 3 {
			lines[fields[0]] = fields[1:]
		}
	}

	allowed, ok := lines["ALLOWED"]
	if !ok || allowed[0] != "0" || allowed[1] == "0" {
		t.Fatalf("allowlisted name did not resolve: %q", out)
	}
	if denied, ok := lines["DENIED"]; !ok || denied[0] != "3" {
		t.Fatalf("non-allowlisted name was not NXDOMAIN: %q", out)
	}
	if dnat, ok := lines["DNAT"]; !ok || dnat[0] != "0" || dnat[1] == "0" {
		t.Fatalf("a hardcoded resolver was not redirected to the gateway: %q", out)
	}
	if rebindResolves {
		if rebind, ok := lines["REBIND"]; !ok || rebind[0] != "3" || rebind[1] != "0" {
			t.Fatalf("a name resolving to loopback was not refused: %q", out)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	var sawAllow, sawDenied, sawRebind bool
	for _, e := range events {
		switch {
		case e.Allowed && e.Host == allowHost && e.Reason == egress.ReasonDNSResolved:
			sawAllow = true
		case !e.Allowed && e.Host == deniedHost && e.Reason == egress.ReasonDNSNotAllowed:
			sawDenied = true
		case !e.Allowed && e.Host == rebindHost && e.Reason == egress.ReasonDNSBlockedAnswer:
			sawRebind = true
		}
	}
	if !sawAllow || !sawDenied {
		t.Fatalf("missing audit decisions: %+v", events)
	}
	if rebindResolves && !sawRebind {
		t.Fatalf("missing the blocked-answer decision: %+v", events)
	}
}
