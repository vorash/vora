//go:build security

package security

import (
	"os"
	"strconv"
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
func TestForkBombContained(t *testing.T) {
	forEachBackend(t, func(t *testing.T, f *fixture) {
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
		h := f.create("sbx_mem", limits(64, 64))

		// OOM often kills vora-init too, which drops the exec stream with
		// Unavailable instead of a process exit code. That is the limit working.
		_, _, code := f.execAllowDisconnect(h, "python3", "-c", "x = bytearray(400 * 1024 * 1024)")
		if code == 0 {
			t.Fatal("memory bomb exited successfully; memory limit not enforced")
		}
		if !f.waitFor(5*time.Second, func() bool { return f.stats(h).OOMKills > 0 }) {
			t.Fatal("no OOM kill recorded; memory limit not enforced")
		}
	})
}

// TestCannotSignalHost asserts the sandbox cannot signal a host process.
//
// Why it matters: without a PID namespace the workload can kill the runner or
// other tenants by guessing host PIDs.
func TestCannotSignalHost(t *testing.T) {
	forEachBackend(t, func(t *testing.T, f *fixture) {
		h := f.create("sbx_kill", limits(128, 64))
		hostPID := strconv.Itoa(os.Getpid())
		_, _, code := f.exec(h, "kill", "-0", hostPID)
		if code == 0 {
			t.Fatalf("sandbox could signal host pid %s", hostPID)
		}
	})
}

// TestIPCNamespaceIsolation asserts SysV shm created in A is invisible to B.
//
// Why it matters: a shared IPC namespace lets one sandbox read or mutate
// another's shared memory and message queues.
func TestIPCNamespaceIsolation(t *testing.T) {
	forEachBackend(t, func(t *testing.T, f *fixture) {
		a := f.create("sbx_ipc_a", limits(128, 64))
		b := f.create("sbx_ipc_b", limits(128, 64))

		create := `
import ctypes, ctypes.util, sys
libc = ctypes.CDLL(ctypes.util.find_library("c"), use_errno=True)
key = 0x564F5241
shmid = libc.shmget(key, 4096, 0o1000 | 0o600)
sys.exit(0 if shmid >= 0 else 1)
`
		if _, errOut, code := f.exec(a, "python3", "-c", create); code != 0 {
			t.Fatalf("A shmget failed: %s", errOut)
		}
		attach := `
import ctypes, ctypes.util, sys
libc = ctypes.CDLL(ctypes.util.find_library("c"), use_errno=True)
key = 0x564F5241
shmid = libc.shmget(key, 4096, 0)
sys.exit(0 if shmid >= 0 else 1)
`
		if _, _, code := f.exec(b, "python3", "-c", attach); code == 0 {
			t.Fatal("sandbox B attached to sandbox A's SysV shm")
		}
	})
}

// TestUTSNamespaceIsolation asserts the sandbox hostname is not the host's.
//
// Why it matters: UTS leakage reveals host identity and lets a workload
// impersonate the host in logs and Kerberos-style name checks.
func TestUTSNamespaceIsolation(t *testing.T) {
	forEachBackend(t, func(t *testing.T, f *fixture) {
		h := f.create("sbx_uts", limits(128, 64))
		host, err := os.Hostname()
		if err != nil {
			t.Fatal(err)
		}
		out, _, _ := f.exec(h, "cat", "/proc/sys/kernel/hostname")
		got := strings.TrimSpace(out)
		if got == "" {
			t.Fatal("sandbox hostname empty")
		}
		if got == host {
			t.Fatalf("sandbox hostname equals host hostname %q", host)
		}
	})
}

// TestUnprivilegedUserNamespace asserts the sandbox is not in the host user ns.
//
// Why it matters: sharing the host user namespace gives the workload a path to
// host uid 0 capabilities. gVisor Method 2 is uid 0 inside its sentry; skip.
func TestUnprivilegedUserNamespace(t *testing.T) {
	forEachBackend(t, func(t *testing.T, f *fixture) {
		if f.backend.Name() != "bwrap" {
			t.Skip("gVisor Method 2 is uid 0 inside the sentry; user-ns isolation is bwrap's")
		}
		hostMap, err := os.ReadFile("/proc/self/uid_map")
		if err != nil {
			t.Fatal(err)
		}
		h := f.create("sbx_userns", limits(128, 64))
		got, _, _ := f.exec(h, "cat", "/proc/self/uid_map")
		if strings.TrimSpace(got) == strings.TrimSpace(string(hostMap)) && strings.Contains(string(hostMap), "4294967295") {
			t.Fatalf("sandbox uid_map is the host's: %s", got)
		}
	})
}

// TestControlSocketIsolation asserts sandbox A cannot see B's control socket.
//
// Why it matters: the control socket is the exec/signal API; reaching another
// sandbox's socket is equivalent to executing code as that tenant.
func TestControlSocketIsolation(t *testing.T) {
	forEachBackend(t, func(t *testing.T, f *fixture) {
		a := f.create("sbx_sock_a", limits(128, 64))
		_ = f.create("sbx_sock_b", limits(128, 64))
		bIPC := f.ws.Paths("sbx_sock_b").IPCDir
		if _, _, code := f.exec(a, "sh", "-c", "test -e "+bIPC); code == 0 {
			t.Fatal("sandbox A can see sandbox B's ipc directory")
		}
		if _, _, code := f.exec(a, "sh", "-c", "test -e "+bIPC+"/control.sock"); code == 0 {
			t.Fatal("sandbox A can see sandbox B's control socket")
		}
	})
}
