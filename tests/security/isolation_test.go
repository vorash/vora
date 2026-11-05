//go:build security

package security

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/vorash/vora/internal/runtime/bwrap"
)

func TestCloneCannotCreateUserNamespace(t *testing.T) {
	forEachBackend(t, func(t *testing.T, f *fixture) {
		h := f.create("sbx_clone_userns", limits(128, 64))
		helper := filepath.Join(f.ws.Paths(h.ID()).WorkspaceDir, "clone-newuser")
		cmd := exec.Command("go", "build", "-o", helper, "./testdata/clone-newuser")
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("build clone helper: %v: %s", err, out)
		}
		if _, _, code := f.exec(h, "/workspace/clone-newuser"); code != 0 {
			t.Fatalf("clone(CLONE_NEWUSER) was not refused: exit %d", code)
		}
	})
}

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
		if _, _, code := f.exec(a, "sh", "-c", "test -e "+f.ws.Paths(fixtureID("sbx_b")).WorkspaceDir); code == 0 {
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
// The workload runs as the same uid as PID 1 and PID 2, so their /proc environ
// is as readable to it as its own (vorash/vora#126).
//
// A NetworkNone sandbox starts through the user manager, which never sees this
// process's environment, so on bwrap the secret goes in the manager too: without
// that this test cannot fail on that path. Start paths that run from the runner
// (full, restricted, private) need CAP_NET_ADMIN, which the isolation job lacks:
// scripts/test-system-unit.sh checks them end to end, and TestNetnsCommandEnv
// checks what they pass.
func TestNoSecretsLeak(t *testing.T) {
	secret := "hunter2-" + strings.Repeat("x", 16)
	t.Setenv(testSecret, secret)
	forEachBackend(t, func(t *testing.T, f *fixture) {
		if _, ok := f.backend.(*bwrap.Backend); ok {
			setManagerEnv(t, testSecret, secret)
		}
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
		for _, pid := range []string{"1", "2"} {
			environ := f.procEnviron(h, pid)
			if strings.Contains(environ, secret) || strings.Contains(environ, testSecret) {
				t.Fatalf("runner secret leaked into /proc/%s/environ", pid)
			}
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

		// Bubblewrap keeps its agent and reports the exec's exit. A backend
		// whose OOM takes the agent down drops the exec stream with
		// Unavailable instead of a process exit code. Either is the limit working.
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

func TestReplacedControlSocketFailsClosed(t *testing.T) {
	forEachBackend(t, func(t *testing.T, f *fixture) {
		if _, ok := f.backend.(*bwrap.Backend); !ok {
			t.Skip("bwrap control socket only")
		}
		h := f.create("sbx_control_swap", limits(128, 64))
		_, _, _ = f.exec(h, "sh", "-c", "rm -f /vora/control.sock; ln -s /tmp/forged.sock /vora/control.sock")
		out, _, code, err := f.execStream(h, "echo", "trusted")
		if err == nil && (code != 0 || strings.TrimSpace(out) != "trusted") {
			t.Fatalf("exec returned a forged response: output %q, code %d", out, code)
		}
	})
}

// TestProcSysIsReadOnly asserts a workload cannot open a sysctl for writing.
//
// Why it matters: kernel.cad_pid is listed with the PID namespace's sysctls and so looks
// owned by the sandbox, but it acts on the host's value. On kernel 7.0 a workload with no
// capabilities redirected the host's Ctrl-Alt-Del to itself. The IPC and PID namespace
// sysctls are writable the same way, and a warm pool's next run could read them back.
//
// It only opens files and never writes, so a failure here leaves the host unchanged.
func TestProcSysIsReadOnly(t *testing.T) {
	forEachBackend(t, func(t *testing.T, f *fixture) {
		if _, ok := f.backend.(*bwrap.Backend); !ok {
			t.Skip("gVisor's /proc/sys is the Sentry's own, and writes never reach the host")
		}
		h := f.create("sbx_proc_sys", limits(128, 64))
		out, errOut, code := f.exec(h, "python3", "-c", `
import errno, os, stat
checked, writable = 0, []
for d, _, names in os.walk("/proc/sys"):
    for name in names:
        path = os.path.join(d, name)
        if not stat.S_ISREG(os.lstat(path).st_mode):
            continue
        checked += 1
        try:
            os.close(os.open(path, os.O_WRONLY))
            writable.append(path)
        except OSError as e:
            if e.errno not in (errno.EROFS, errno.EACCES, errno.EPERM):
                raise
print("checked", checked)
print("writable", writable)
print("msgmax", open("/proc/sys/kernel/msgmax").read().strip())
print("binfmt", os.listdir("/proc/sys/fs/binfmt_misc") if os.path.isdir("/proc/sys/fs/binfmt_misc") else [])
`)
		if code != 0 {
			t.Fatalf("exit %d:\n%s%s", code, out, errOut)
		}
		t.Log(out)
		if !strings.Contains(out, "writable []") {
			t.Fatalf("a workload can open sysctls for writing:\n%s", out)
		}
		if strings.Contains(out, "checked 0") {
			t.Fatal("/proc/sys is empty, so this proved nothing")
		}
		// The bind that makes /proc/sys read-only comes from the host's /proc.
		if !strings.Contains(out, "binfmt []") {
			t.Fatalf("the host's binfmt_misc entries are visible:\n%s", out)
		}
		// Hiding /proc/sys would pass the check above and break programs that read it.
		if !strings.Contains(out, "msgmax ") {
			t.Fatalf("a sysctl cannot be read:\n%s", out)
		}
	})
}

// TestVoraInitTamper asserts a workload cannot replace or change the agent
// binary, from inside or as seen from the host.
//
// Why it matters: a warm pool reuses a sandbox across untrusted runs and runs
// /vora/vora-init inside it to reset it. A run that replaced that binary would
// own every later run. /vora itself stays writable under bwrap for the control
// socket, so the read-only file bind is the whole defence.
func TestVoraInitTamper(t *testing.T) {
	forEachBackend(t, func(t *testing.T, f *fixture) {
		if _, ok := f.backend.(*bwrap.Backend); !ok {
			t.Skip("gVisor binds all of /vora read-only: TestGvisorVoraReadOnly")
		}
		h := f.create("sbx_init_tamper", limits(128, 64))
		want := fileSHA256(t, os.Getenv("VORA_INIT_BINARY"))

		script, err := os.ReadFile(filepath.Join("..", "testdata", "tamper_vora_init.py"))
		if err != nil {
			t.Fatal(err)
		}
		out, errOut, code := f.exec(h, "python3", "-c", string(script))
		t.Logf("tamper attempts:\n%s", out)
		if code != 0 {
			t.Fatalf("a tamper attempt was not stopped (exit %d):\n%s%s", code, out, errOut)
		}
		if !strings.Contains(out, "sha256 "+want) {
			t.Fatalf("binary inside the sandbox changed, want sha256 %s:\n%s", want, out)
		}
		host := filepath.Join(f.ws.Paths(h.ID()).IPCDir, "vora-init")
		if got := fileSHA256(t, host); got != want {
			t.Fatalf("binary on the host changed: sha256 %s, want %s", got, want)
		}
		if got, _, code := f.exec(h, "echo", "alive"); code != 0 || strings.TrimSpace(got) != "alive" {
			t.Fatalf("sandbox stopped serving after the attempts: exit %d, output %q", code, got)
		}
	})
}

// TestInitReinstall asserts a start over a used ipc directory installs the real
// binary, whatever the workload left there.
//
// Why it matters: the install runs on the host with the runner's privileges and
// bwrap follows a symlink at the file bind's source, so a link the workload
// planted could overwrite a host file or become the sandbox's init.
func TestInitReinstall(t *testing.T) {
	forEachBackend(t, func(t *testing.T, f *fixture) {
		if _, ok := f.backend.(*bwrap.Backend); !ok {
			t.Skip("the workload cannot write /vora under gVisor")
		}
		hostFile := filepath.Join(f.cfg.StateDir, "host-file")
		if err := os.WriteFile(hostFile, []byte("host data"), 0o600); err != nil {
			t.Fatal(err)
		}
		h := f.create("sbx_reinstall", limits(128, 64))
		if _, errOut, code := f.exec(h, "ln", "-s", hostFile, "/vora/vora-init.tmp"); code != 0 {
			t.Fatalf("plant symlink: %s", errOut)
		}
		if err := f.backend.Destroy(context.Background(), h); err != nil {
			t.Fatalf("Destroy: %v", err)
		}

		h = f.create("sbx_reinstall", limits(128, 64))
		if got, err := os.ReadFile(hostFile); err != nil || string(got) != "host data" {
			t.Fatalf("host file = %q, %v: the install wrote through the planted symlink", got, err)
		}
		host := filepath.Join(f.ws.Paths(h.ID()).IPCDir, "vora-init")
		info, err := os.Lstat(host)
		if err != nil {
			t.Fatal(err)
		}
		if !info.Mode().IsRegular() {
			t.Fatalf("installed binary mode = %v, want a regular file", info.Mode())
		}
		if got, want := fileSHA256(t, host), fileSHA256(t, os.Getenv("VORA_INIT_BINARY")); got != want {
			t.Fatalf("installed binary sha256 %s, want %s", got, want)
		}
		if got, _, code := f.exec(h, "echo", "alive"); code != 0 || strings.TrimSpace(got) != "alive" {
			t.Fatalf("restarted sandbox does not serve: exit %d, output %q", code, got)
		}
	})
}

// TestAgentProcDenied asserts a workload cannot open the agent's memory or
// environment through /proc.
//
// Why it matters: the agent runs as the workload's uid and outlives every run in
// a reused sandbox, so writing its memory would carry code into the next run.
//
// environ is what tells PR_SET_DUMPABLE apart on any host. mem is opened with
// PTRACE_MODE_ATTACH, which Yama already refuses for a child reading its ancestor
// when ptrace_scope is 1 or more, so it only depends on dumpable at ptrace_scope 0.
// The check is on open: reading mem at offset 0 fails with EIO on any process.
func TestAgentProcDenied(t *testing.T) {
	forEachBackend(t, func(t *testing.T, f *fixture) {
		if _, ok := f.backend.(*bwrap.Backend); !ok {
			t.Skip("gVisor runs no agent: TestGvisorVoraReadOnly covers its PID 1")
		}
		h := f.create("sbx_agent_proc", limits(128, 64))
		out, errOut, code := f.exec(h, "python3", "-c", `
import errno, os, sys
agent = None
for pid in filter(str.isdigit, os.listdir("/proc")):
    try:
        with open(f"/proc/{pid}/comm") as c:
            if c.read().strip() == "vora-init":
                agent = pid
                break
    except OSError:
        pass
if agent is None:
    sys.exit("agent not found in /proc")
# The same opens on this process must work, or a denial below proves nothing.
for name in ("mem", "environ"):
    os.close(os.open(f"/proc/self/{name}", os.O_RDONLY))
exposed = False
for name in ("mem", "environ"):
    try:
        os.close(os.open(f"/proc/{agent}/{name}", os.O_RDONLY))
        print(name, "OK")
        exposed = True
    except OSError as e:
        print(name, errno.errorcode[e.errno])
        exposed = exposed or e.errno not in (errno.EACCES, errno.EPERM)
print("agent", agent)
sys.exit(1 if exposed else 0)
`)
		scope, _ := os.ReadFile("/proc/sys/kernel/yama/ptrace_scope")
		t.Logf("yama ptrace_scope %s\n%s", strings.TrimSpace(string(scope)), out)
		if code != 0 {
			t.Fatalf("agent /proc files exposed (exit %d, yama ptrace_scope %s):\n%s%s", code, strings.TrimSpace(string(scope)), out, errOut)
		}
		if got, _, code := f.exec(h, "echo", "alive"); code != 0 || strings.TrimSpace(got) != "alive" {
			t.Fatalf("sandbox stopped serving: exit %d, output %q", code, got)
		}
	})
}

func fileSHA256(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("%x", sha256.Sum256(data))
}

// TestControlSocketIsolation asserts sandbox A cannot see B's control socket.
//
// Why it matters: the control socket is the exec/signal API; reaching another
// sandbox's socket is equivalent to executing code as that tenant.
func TestControlSocketIsolation(t *testing.T) {
	forEachBackend(t, func(t *testing.T, f *fixture) {
		a := f.create("sbx_sock_a", limits(128, 64))
		_ = f.create("sbx_sock_b", limits(128, 64))
		bIPC := f.ws.Paths(fixtureID("sbx_sock_b")).IPCDir
		if _, _, code := f.exec(a, "sh", "-c", "test -e "+bIPC); code == 0 {
			t.Fatal("sandbox A can see sandbox B's ipc directory")
		}
		if _, _, code := f.exec(a, "sh", "-c", "test -e "+bIPC+"/control.sock"); code == 0 {
			t.Fatal("sandbox A can see sandbox B's control socket")
		}
	})
}
