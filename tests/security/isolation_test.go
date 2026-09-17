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
	forEachBackend(t, func(t *testing.T, f *fixture) {
		// A host-only sentinel outside every mounted directory.
		sentinel := f.cfg.StateDir + "/host-only-sentinel"
		if err := os.WriteFile(sentinel, []byte("host secret"), 0o600); err != nil {
			t.Fatal(err)
		}
		h := f.create("sbx_fs", limits(128, 64))

		if _, _, code := f.exec(h, "sh", "-c", "test -e "+sentinel); code == 0 {
			t.Fatalf("host path %s is visible inside the sandbox", sentinel)
		}
		if _, _, code := f.exec(h, "sh", "-c", "touch /etc/should-fail"); code == 0 {
			t.Fatal("sandbox wrote to read-only rootfs /etc")
		}
		if _, _, code := f.exec(h, "sh", "-c", "echo ok > /workspace/ok"); code != 0 {
			t.Fatal("sandbox could not write to its workspace")
		}
	})
}

// TestPIDNamespaceIsolation asserts the sandbox has its own PID namespace.
//
// Why it matters: without a PID namespace the workload can inspect, signal and
// ptrace processes owned by other sandboxes or the host.
func TestPIDNamespaceIsolation(t *testing.T) {
	forEachBackend(t, func(t *testing.T, f *fixture) {
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
		if n := strings.TrimSpace(out); n != "" && n != "0" && len(n) > 2 {
			t.Fatalf("sandbox sees too many processes: %s", n)
		}
	})
}

// TestCrossSandboxFilesystemIsolation asserts sandbox A cannot see sandbox B's
// files.
//
// Why it matters: tenant isolation is the core promise; a shared writable
// path would let one workload read or corrupt another's data.
func TestCrossSandboxFilesystemIsolation(t *testing.T) {
	forEachBackend(t, func(t *testing.T, f *fixture) {
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
		if _, _, code := f.exec(a, "sh", "-c", "test -e "+f.ws.Paths("sbx_b").WorkspaceDir); code == 0 {
			t.Fatal("sandbox A can see sandbox B's workspace directory")
		}
	})
}

// TestNetworkNone asserts the sandbox has no network path.
//
// Why it matters: unrestricted egress enables metadata-service attacks,
// private-network scanning, exfiltration and reverse shells.
func TestNetworkNone(t *testing.T) {
	forEachBackend(t, func(t *testing.T, f *fixture) {
		h := f.create("sbx_net", limits(128, 64))

		out, _, _ := f.exec(h, "sh", "-c", "cat /proc/net/route 2>/dev/null || true")
		if strings.Contains(out, "00000000") {
			t.Fatalf("sandbox has a default route: %s", out)
		}

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
	})
}

// TestNoSecretsLeak asserts runner secrets never appear in the sandbox.
//
// Why it matters: credentials in the environment or /proc are trivially
// exfiltrated by untrusted code; Vora brokers credentials outside the sandbox.
func TestNoSecretsLeak(t *testing.T) {
	secret := "hunter2-" + strings.Repeat("x", 16)
	t.Setenv(testSecret, secret)
	forEachBackend(t, func(t *testing.T, f *fixture) {
		h := f.create("sbx_secret", limits(128, 64))

		out, _, _ := f.exec(h, "env")
		if strings.Contains(out, secret) {
			t.Fatal("runner secret value leaked into the sandbox environment")
		}
		out, _, _ = f.exec(h, "cat", "/proc/self/environ")
		if strings.Contains(out, secret) {
			t.Fatal("runner secret value leaked into /proc/self/environ")
		}
		if strings.Contains(out, testSecret) {
			t.Fatal("runner secret name leaked into /proc/self/environ")
		}
	})
}

// TestForkBombContained asserts pids.max contains a fork bomb.
//
// Why it matters: an unbounded fork exhausts host PIDs and can make the whole
// machine unresponsive.
//
// Resource limits are applied by the bwrap cgroup manager; the rootless gVisor
// backend runs with --ignore-cgroups in v0.2, so limits are a follow-up there.
func TestForkBombContained(t *testing.T) {
	forEachBackend(t, func(t *testing.T, f *fixture) {
		if f.backend.Name() != "bwrap" {
			t.Skip("cgroup limits are enforced by the bwrap backend; gVisor rootless runs --ignore-cgroups (follow-up)")
		}
		h := f.create("sbx_fork", limits(128, 16))
		f.exec(h, "sh", "-c", "for i in $(seq 1 100); do sleep 2 & done; wait")

		if !f.waitFor(3*time.Second, func() bool { return f.stats(h).PidsLimitHits > 0 }) {
			t.Fatal("pids limit was never hit; fork bomb was not contained")
		}
	})
}

// TestMemoryBombKilled asserts memory.max kills an over-allocating process.
//
// Why it matters: unconstrained memory growth triggers host OOM and can kill
// unrelated workloads.
func TestMemoryBombKilled(t *testing.T) {
	forEachBackend(t, func(t *testing.T, f *fixture) {
		if f.backend.Name() != "bwrap" {
			t.Skip("cgroup limits are enforced by the bwrap backend; gVisor rootless runs --ignore-cgroups (follow-up)")
		}
		h := f.create("sbx_mem", limits(64, 64))

		_, _, code := f.exec(h, "python3", "-c", "x = bytearray(400 * 1024 * 1024)")
		if code == 0 {
			t.Fatal("memory bomb exited successfully; memory limit not enforced")
		}
		if !f.waitFor(3*time.Second, func() bool { return f.stats(h).OOMKills > 0 }) {
			t.Fatal("no OOM kill recorded; memory limit not enforced")
		}
	})
}
