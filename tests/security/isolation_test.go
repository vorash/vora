//go:build security

package security

import (
	"os"
	"strings"
	"testing"
	"time"
)

// TestFilesystemIsolation asserts that the sandbox sees only the image root
// fs and cannot read host paths or write outside its workspace.
//
// Why it matters: a workload that can read the host filesystem can steal
// credentials and other tenants' data; if it can write anywhere, it can
// persist across sandboxes.
func TestFilesystemIsolation(t *testing.T) {
	f := newFixture(t)

	// A host-only sentinel outside every mounted directory.
	sentinel := f.cfg.StateDir + "/host-only-sentinel"
	if err := os.WriteFile(sentinel, []byte("host secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	h := f.create("sbx_fs", limits(128, 64))

	// The host sentinel must not be reachable.
	if _, _, code := f.exec(h, "sh", "-c", "test -e "+sentinel); code == 0 {
		t.Fatalf("host path %s is visible inside the sandbox", sentinel)
	}

	// The root filesystem is read-only.
	if _, _, code := f.exec(h, "sh", "-c", "touch /etc/should-fail"); code == 0 {
		t.Fatal("sandbox wrote to read-only rootfs /etc")
	}
	// The workspace is writable.
	if _, _, code := f.exec(h, "sh", "-c", "echo ok > /workspace/ok"); code != 0 {
		t.Fatal("sandbox could not write to its workspace")
	}
}

// TestPIDNamespaceIsolation asserts the sandbox has its own PID namespace.
//
// Why it matters: without a PID namespace the workload can inspect, signal and
// ptrace processes owned by other sandboxes or the host.
func TestPIDNamespaceIsolation(t *testing.T) {
	f := newFixture(t)
	h := f.create("sbx_pid", limits(128, 64))

	hostInit, err := os.ReadFile("/proc/1/cmdline")
	if err != nil {
		t.Skipf("cannot read host /proc/1/cmdline: %v", err)
	}
	sandboxInit, _, _ := f.exec(h, "cat", "/proc/1/cmdline")
	if strings.TrimSpace(sandboxInit) == strings.TrimSpace(string(hostInit)) && strings.TrimSpace(string(hostInit)) != "" {
		t.Fatal("sandbox PID 1 is the host PID 1")
	}

	// Total processes visible must be small (only the sandbox's own).
	out, _, _ := f.exec(h, "sh", "-c", "ls /proc | grep -E '^[0-9]+$' | wc -l")
	if n := strings.TrimSpace(out); n != "" && n != "0" {
		if len(n) > 2 { // >99 processes would indicate host leakage
			t.Fatalf("sandbox sees too many processes: %s", n)
		}
	}
}

// TestCrossSandboxFilesystemIsolation asserts sandbox A cannot see sandbox B's
// files.
//
// Why it matters: tenant isolation is the core promise; a shared writable
// path would let one workload read or corrupt another's data.
func TestCrossSandboxFilesystemIsolation(t *testing.T) {
	f := newFixture(t)
	a := f.create("sbx_a", limits(128, 64))
	b := f.create("sbx_b", limits(128, 64))

	if _, errOut, code := f.exec(b, "sh", "-c", "echo secret-b > /workspace/secret-b.txt"); code != 0 {
		t.Fatalf("B write failed: %s", errOut)
	}
	if _, _, code := f.exec(a, "sh", "-c", "cat /workspace/secret-b.txt 2>/dev/null"); code == 0 {
		t.Fatal("sandbox A can read sandbox B's file")
	}
	out, _, _ := f.exec(a, "ls", "-A", "/workspace")
	if strings.Contains(out, "secret-b.txt") {
		t.Fatalf("sandbox A sees B's file: %s", out)
	}
	// A's workspace host path cannot reach B's.
	if _, _, code := f.exec(a, "sh", "-c", "test -e "+f.ws.Paths("sbx_b").WorkspaceDir); code == 0 {
		t.Fatal("sandbox A can see sandbox B's workspace directory")
	}
}

// TestForkBombContained asserts pids.max contains a fork bomb.
//
// Why it matters: an unbounded fork exhausts host PIDs and can make the whole
// machine unresponsive.
func TestForkBombContained(t *testing.T) {
	f := newFixture(t)
	h := f.create("sbx_fork", limits(128, 16))

	// Fork more processes than the limit; the extra forks are denied by the
	// kernel, which increments pids.events.
	f.exec(h, "sh", "-c", "for i in $(seq 1 100); do sleep 2 & done; wait")

	if !f.waitFor(3*time.Second, func() bool {
		return f.stats(h).PidsLimitHits > 0
	}) {
		t.Fatal("pids limit was never hit; fork bomb was not contained")
	}
}

// TestMemoryBombKilled asserts memory.max kills an over-allocating process.
//
// Why it matters: unconstrained memory growth triggers host OOM and can kill
// unrelated workloads.
func TestMemoryBombKilled(t *testing.T) {
	f := newFixture(t)
	h := f.create("sbx_mem", limits(64, 64))

	// Allocating far beyond the limit must not succeed.
	_, _, code := f.exec(h, "python3", "-c", "x = bytearray(400 * 1024 * 1024)")
	if code == 0 {
		t.Fatal("memory bomb exited successfully; memory limit not enforced")
	}
	if !f.waitFor(3*time.Second, func() bool {
		return f.stats(h).OOMKills > 0
	}) {
		t.Fatal("no OOM kill recorded; memory limit not enforced")
	}
}

// TestNetworkNone asserts the sandbox has no network path.
//
// Why it matters: unrestricted egress enables metadata-service attacks,
// private-network scanning, exfiltration and reverse shells.
func TestNetworkNone(t *testing.T) {
	f := newFixture(t)
	h := f.create("sbx_net", limits(128, 64))

	// No default route.
	out, _, _ := f.exec(h, "sh", "-c", "cat /proc/net/route 2>/dev/null || true")
	if strings.Contains(out, "00000000") {
		t.Fatalf("sandbox has a default route: %s", out)
	}

	// The cloud metadata endpoint is unreachable.
	out, _, _ = f.exec(h, "python3", "-c", `
import socket
socket.setdefaulttimeout(2)
try:
    socket.create_connection(("169.254.169.254", 80), 2)
    print("CONNECTED")
except Exception:
    print("BLOCKED")
`)
	if !strings.Contains(out, "BLOCKED") || strings.Contains(out, "CONNECTED") {
		t.Fatalf("metadata endpoint reachable: %q", out)
	}
}

// TestNoSecretsLeak asserts runner secrets never appear in the sandbox.
//
// Why it matters: credentials in the environment or /proc are trivially
// exfiltrated by untrusted code; Vora brokers credentials outside the sandbox.
func TestNoSecretsLeak(t *testing.T) {
	secret := "hunter2-" + strings.Repeat("x", 16)
	t.Setenv(testSecret, secret)
	f := newFixture(t)
	h := f.create("sbx_secret", limits(128, 64))

	out, _, _ := f.exec(h, "env")
	if strings.Contains(out, secret) {
		t.Fatal("runner secret leaked into the sandbox environment")
	}
	out, _, _ = f.exec(h, "cat", "/proc/self/environ")
	if strings.Contains(out, secret) {
		t.Fatal("runner secret leaked into /proc/self/environ")
	}
	// And the secret name is not even present.
	if strings.Contains(out, testSecret) {
		t.Fatal("secret variable name leaked into /proc/self/environ")
	}
}
