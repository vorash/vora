//go:build integration

package integration

import (
	"context"
	"errors"
	"io"
	"log/slog"
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
	"github.com/vorash/vora/internal/ulid"
	"github.com/vorash/vora/internal/workspace"
)

// echoHosts are public TLS endpoints that echo request headers, so the test can
// prove the broker attached the credential.
var echoHosts = []string{"postman-echo.com", "httpbin.org"}

// TestBrokerEndToEnd proves the broker route from inside a real
// NetworkRestricted sandbox: the workload calls
// http://<gateway>:<port>/creds/<alias>/<path>, the broker re-originates to the
// bound host over TLS with the credential attached, and the sandbox never
// holds the value. It needs uid 0 in a user namespace (run under rootlesskit,
// like `make test-broker`) plus external egress.
func TestBrokerEndToEnd(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("needs uid 0 in a user namespace (run under rootlesskit)")
	}
	for _, bin := range []string{"unshare", "ip", "nft", "nsenter", "bwrap"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not found", bin)
		}
	}
	host, err := pickEchoHost()
	if err != nil {
		t.Skipf("no external egress to an echo endpoint: %v", err)
	}
	t.Logf("brokering to %s", host)

	const secret = "vora-brokered-token-123"
	const sandboxAuth = "sandbox-supplied-token"

	initBin := buildBinary(t, "vora-init")
	t.Setenv("VORA_INIT_BINARY", initBin)
	subtree, err := cgroup.NewSubtree()
	if err != nil {
		t.Skipf("cgroup subtree unavailable: %v", err)
	}
	rootfs := imageRootfs(t, "base")

	stateDir := stateTempDir(t)
	cfg := config.Config{
		StateDir:             stateDir,
		SystemdMode:          config.SystemdUser,
		EgressBrokerPort:     network.DefaultBrokerPort,
		EgressMaxConnections: 32,
		EgressIdleTimeout:    time.Minute,
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

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
	manager := network.NewManager(stateDir, network.ManagerConfig{
		ProxyPort:  network.DefaultProxyPort,
		BrokerPort: cfg.EgressBrokerPort,
	}, logger)

	backend, err := bwrap.New(cfg, nil, logger)
	if err != nil {
		t.Fatalf("bwrap backend: %v", err)
	}
	backend.EnableNetns(subtree, manager, sink)

	id := "sbx_broker_" + strings.ReplaceAll(time.Now().UTC().Format("20060102150405.000000000"), ".", "")
	wsManager := workspace.NewManager(cfg.SandboxesDir())
	ws := wsManager.Paths(id)
	ws.RootfsDir = rootfs
	if _, err := wsManager.Create(ws); err != nil {
		t.Fatalf("workspace: %v", err)
	}
	t.Cleanup(func() { _ = wsManager.Destroy(id) })

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	spec := runtime.SandboxSpec{
		ID:        id,
		Image:     "base",
		Resources: runtime.ResourceLimits{CPUMillis: 1000, MemoryBytes: 256 << 20, Pids: 64},
		Network: runtime.NetworkPolicy{
			Mode:           runtime.NetworkRestricted,
			AllowedDomains: []string{host},
			BrokerAliases:  []string{"echo"},
		},
		TTL:       time.Minute,
		Workspace: ws,
	}
	h, err := backend.Create(ctx, spec)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = backend.Destroy(context.Background(), h) })

	creds := []egress.Credential{{Alias: "echo", Host: host, Scheme: egress.SchemeBearer, Value: secret}}
	if err := backend.StartWithCredentials(ctx, h, creds); err != nil {
		t.Fatalf("StartWithCredentials: %v", err)
	}

	script := `
import json, os, urllib.error, urllib.request

base = os.environ["VORA_BROKER_URL"]

def get(path, auth=None):
    # A curl-like User-Agent keeps the public echo endpoint from rejecting the
    # probe as a bot; the broker forwards it unchanged.
    headers = {"User-Agent": "curl/8.22.0"}
    if auth:
        headers["Authorization"] = auth
    req = urllib.request.Request(base + path, headers=headers)
    try:
        with urllib.request.urlopen(req, timeout=30) as r:
            return r.status, r.read(4096).decode("utf-8", "replace")
    except urllib.error.HTTPError as e:
        return e.code, ""
    except Exception as e:
        return -1, str(e)

status, body = get("/creds/echo/get", "Bearer ` + sandboxAuth + `")
print("ALLOWED", status, "` + secret + `" in body, "` + sandboxAuth + `" in body)
status, _ = get("/creds/unknown/get")
print("UNKNOWN", status)
print("ENV", "` + secret + `" in json.dumps(dict(os.environ)))
`
	out, errOut, code := execIn(t, backend, h, "/usr/bin/python3", "-c", script)
	if code != 0 {
		t.Fatalf("sandbox probe failed (code %d):\n%s\n%s", code, out, errOut)
	}
	fields := strings.Fields(out)
	if len(fields) < 6 {
		t.Fatalf("unexpected probe output: %q", out)
	}
	// ALLOWED <status> <saw-brokered-token> <saw-sandbox-token>
	if fields[0] != "ALLOWED" || fields[1] != "200" {
		t.Fatalf("brokered call = %q, want ALLOWED 200", out)
	}
	if fields[2] != "True" {
		t.Fatalf("the upstream did not receive the brokered credential: %q", out)
	}
	if fields[3] != "False" {
		t.Fatalf("the sandbox's own Authorization header was forwarded: %q", out)
	}
	// UNKNOWN <status>
	if !strings.Contains(out, "UNKNOWN 404") {
		t.Fatalf("unknown alias was not denied: %q", out)
	}
	if !strings.Contains(out, "ENV False") {
		t.Fatalf("the credential is visible in the sandbox environment: %q", out)
	}

	mu.Lock()
	defer mu.Unlock()
	var allowed bool
	for _, e := range events {
		if e.Alias == "echo" && e.Allowed && e.Host == host {
			allowed = true
		}
	}
	if !allowed {
		t.Fatalf("no alias-bearing allow decision was persisted: %+v", events)
	}
}

// TestBrokerThroughNetns proves the broker enforcement boundary without a full
// sandbox: from inside a prepared sandbox netns, the gateway broker route works
// and the credential reaches the upstream, while an unserveable alias is denied
// and a direct connection to the proxy's port never touches the broker. This is
// the layer `make test-broker` exercises when the host cannot delegate a cgroup
// subtree for a real bubblewrap sandbox.
func TestBrokerThroughNetns(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("needs uid 0 in a user namespace (run under rootlesskit)")
	}
	for _, bin := range []string{"unshare", "ip", "nft", "nsenter", "python3"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not found", bin)
		}
	}
	host, err := pickEchoHost()
	if err != nil {
		t.Skipf("no external egress to an echo endpoint: %v", err)
	}

	const secret = "vora-netns-brokered-token-456"
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

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	id := ulid.NewSandbox()
	n, err := manager.Create(ctx, id, network.Policy{Mode: runtime.NetworkRestricted})
	if err != nil {
		t.Fatalf("network Create: %v", err)
	}
	t.Cleanup(func() { _ = manager.Destroy(context.Background(), id) })

	broker := egress.NewBroker(id,
		egress.Policy{AllowedDomains: []string{host}},
		[]egress.Credential{{Alias: "echo", Host: host, Scheme: egress.SchemeBearer, Value: secret}},
		logger, sink)
	brokerCtx, stopBroker := context.WithCancel(context.Background())
	t.Cleanup(stopBroker)
	if err := broker.Listen(brokerCtx, n.ServiceAddr(network.DefaultBrokerPort)); err != nil {
		t.Fatalf("broker Listen: %v", err)
	}
	t.Cleanup(func() { _ = broker.Close() })

	script := `
import os, urllib.error, urllib.request
base = "http://` + n.GatewayIP + `:` + network.DefaultBrokerPort + `"
def get(path):
    req = urllib.request.Request(base + path, headers={"User-Agent": "curl/8.22.0"})
    try:
        with urllib.request.urlopen(req, timeout=30) as r:
            return r.status, r.read(4096).decode("utf-8", "replace")
    except urllib.error.HTTPError as e:
        return e.code, ""
    except Exception as e:
        return -1, str(e)
status, body = get("/creds/echo/get")
print("ALLOWED", status, "` + secret + `" in body)
status, _ = get("/creds/unknown/get")
print("UNKNOWN", status)
`
	out := nsenterRun(t, n.NetnsPID, "python3", "-c", script)
	fields := strings.Fields(out)
	if len(fields) < 4 {
		t.Fatalf("unexpected probe output: %q", out)
	}
	if fields[0] != "ALLOWED" || fields[1] != "200" || fields[2] != "True" {
		mu.Lock()
		defer mu.Unlock()
		t.Fatalf("brokered call = %q, want the credential attached upstream; events=%+v host=%q", out, events, host)
	}
	if !strings.Contains(out, "UNKNOWN 404") {
		t.Fatalf("unknown alias was not denied: %q", out)
	}

	mu.Lock()
	defer mu.Unlock()
	for _, e := range events {
		if e.Alias == "echo" && e.Allowed && e.Host == host {
			return
		}
	}
	t.Fatalf("no alias-bearing allow decision was persisted: %+v", events)
}

// pickEchoHost returns the first echo endpoint reachable from the runner, or an
// error when none is (the test then skips).
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
	return "", lastErr
}

// execIn runs a command in a restricted sandbox and returns stdout, stderr and
// the exit code.
func execIn(t *testing.T, backend *bwrap.Backend, h runtime.Handle, command string, args ...string) (string, string, int32) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	stream, err := backend.Exec(ctx, h, runtime.ExecRequest{
		ExecutionID: "exe_broker",
		Command:     command,
		Args:        args,
		Cwd:         "/workspace",
	})
	if err != nil {
		t.Fatalf("Exec %s: %v", command, err)
	}
	defer stream.Close()

	var stdout, stderr strings.Builder
	var result int32
	for {
		chunk, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Recv %s: %v", command, err)
		}
		stdout.Write(chunk.Stdout)
		stderr.Write(chunk.Stderr)
		if chunk.Exit != nil {
			result = int32(chunk.Exit.ExitCode)
		}
	}
	return stdout.String(), stderr.String(), result
}
