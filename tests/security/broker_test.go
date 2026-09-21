//go:build security

package security

import (
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vorash/vora/internal/cgroup"
	"github.com/vorash/vora/internal/config"
	"github.com/vorash/vora/internal/egress"
	"github.com/vorash/vora/internal/network"
	"github.com/vorash/vora/internal/runtime"
	"github.com/vorash/vora/internal/runtime/bwrap"
	"github.com/vorash/vora/internal/workspace"
)

// echoHosts are public TLS endpoints that echo request headers, so the test can
// prove the brokered call really carried the credential.
var echoHosts = []string{"postman-echo.com", "httpbin.org"}

// TestNoSecretsLeakBroker asserts a brokered sandbox reaches an allowlisted API
// while the credential exists only outside it: no value and no alias-bearing
// configuration inside the sandbox environment, /proc, or its writable
// filesystems.
//
// Why it matters: the whole point of brokering (ADR 013) is that untrusted code
// cannot exfiltrate a credential it never holds. If the value reaches env,
// /proc/self/environ, or the workspace, the broker buys nothing.
//
// Needs uid 0 in a user namespace (run under rootlesskit) and external egress.
func TestNoSecretsLeakBroker(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("needs uid 0 in a user namespace (run under rootlesskit)")
	}
	for _, bin := range []string{"unshare", "ip", "nft", "nsenter", "bwrap", "python3"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not found", bin)
		}
	}
	host, err := pickEchoHost()
	if err != nil {
		t.Skipf("no external egress to an echo endpoint: %v", err)
	}

	secret := "hunter2-" + strings.Repeat("y", 16)
	f, _ := newBrokerFixture(t)
	h := f.createRestricted(t, "sbx_broker_secret", []string{host}, []string{"echo"},
		[]egress.Credential{{Alias: "echo", Host: host, Scheme: egress.SchemeBearer, Value: secret}})

	// The brokered call succeeds, so the credential does exist somewhere.
	probe := `
import os, urllib.error, urllib.request
base = os.environ.get("VORA_BROKER_URL", "")
if not base:
    print("NO_BROKER_URL")
    raise SystemExit(1)
req = urllib.request.Request(base + "/creds/echo/get", headers={"User-Agent": "curl/8.22.0"})
with urllib.request.urlopen(req, timeout=30) as r:
    print("BROKERED", r.status, "` + secret + `" in r.read(4096).decode("utf-8", "replace"))
`
	out, stderr, code := f.exec(h, "/usr/bin/python3", "-c", probe)
	if code != 0 || !strings.Contains(out, "BROKERED 200 True") {
		t.Fatalf("brokered call failed (code %d): stdout=%q stderr=%q", code, out, stderr)
	}

	// The sandbox does not hold it.
	out, _, _ = f.exec(h, "env")
	if strings.Contains(out, secret) {
		t.Fatal("credential value leaked into the sandbox environment")
	}
	out, _, _ = f.exec(h, "cat", "/proc/self/environ")
	if strings.Contains(out, secret) {
		t.Fatal("credential value leaked into /proc/self/environ")
	}
	out, _, _ = f.exec(h, "/bin/sh", "-c",
		"grep -r --binary-files=text -e "+secret+" /workspace /root /tmp /vora /etc 2>/dev/null | head -3")
	if strings.Contains(out, secret) {
		t.Fatalf("credential value leaked into the sandbox filesystem: %q", out)
	}
}

// newBrokerFixture builds a backend with the restricted-network and broker
// services wired, using the security suite's image and agent helpers.
func newBrokerFixture(t *testing.T) (*fixture, *network.Manager) {
	t.Helper()
	if os.Getenv("VORA_INIT_BINARY") == "" {
		t.Setenv("VORA_INIT_BINARY", mustBuildInit(t))
	}
	cfg := config.Config{
		StateDir:             t.TempDir(),
		SystemdMode:          config.SystemdUser,
		EgressBrokerPort:     network.DefaultBrokerPort,
		EgressMaxConnections: 32,
		EgressIdleTimeout:    time.Minute,
	}
	subtree, err := cgroup.NewSubtree()
	if err != nil {
		t.Skipf("cgroup subtree unavailable: %v", err)
	}
	logger := discardLogger()
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
	nets := network.NewManager(cfg.StateDir, network.ManagerConfig{
		ProxyPort:  network.DefaultProxyPort,
		BrokerPort: cfg.EgressBrokerPort,
	}, logger)
	backend, err := bwrap.New(cfg, nil, logger)
	if err != nil {
		t.Skipf("bwrap backend unavailable: %v", err)
	}
	backend.EnableNetns(subtree, nets, sink)
	return &fixture{t: t, backend: backend, ws: workspace.NewManager(cfg.SandboxesDir()), cfg: cfg}, nets
}

// createRestricted creates and starts a NetworkRestricted sandbox whose broker
// serves creds.
func (f *fixture) createRestricted(t *testing.T, id string, allowed, aliases []string, creds []egress.Credential) runtime.Handle {
	t.Helper()
	ws := f.ws.Paths(id)
	ws.RootfsDir = imageRootfs(t, "base")
	if _, err := f.ws.Create(ws); err != nil {
		t.Fatalf("workspace: %v", err)
	}
	spec := runtime.SandboxSpec{
		ID:        id,
		Image:     "base",
		Resources: limits(256, 64),
		Network: runtime.NetworkPolicy{
			Mode:           runtime.NetworkRestricted,
			AllowedDomains: allowed,
			BrokerAliases:  aliases,
		},
		TTL:       time.Minute,
		Workspace: ws,
	}
	ctx := context.Background()
	h, err := f.backend.Create(ctx, spec)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	ba, ok := f.backend.(runtime.BrokerAware)
	if !ok {
		t.Fatalf("backend %T does not implement runtime.BrokerAware", f.backend)
	}
	if err := ba.StartWithCredentials(ctx, h, creds); err != nil {
		t.Fatalf("StartWithCredentials: %v", err)
	}
	t.Cleanup(func() {
		_ = f.backend.Destroy(context.Background(), h)
		_ = f.ws.Destroy(id)
	})
	return h
}

// pickEchoHost returns the first echo endpoint reachable from the runner.
func pickEchoHost() (string, error) {
	var lastErr error
	for _, host := range echoHosts {
		conn, err := net.DialTimeout("tcp", net.JoinHostPort(host, "443"), 5*time.Second)
		if err != nil {
			lastErr = err
			continue
		}
		_ = conn.Close()
		return host, nil
	}
	if lastErr == nil {
		lastErr = errors.New("no echo endpoint configured")
	}
	return "", lastErr
}
