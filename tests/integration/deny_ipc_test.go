//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"
	"golang.org/x/sys/unix"

	"github.com/vorash/vora/internal/cgroup"
	"github.com/vorash/vora/internal/config"
	"github.com/vorash/vora/internal/runtime"
	"github.com/vorash/vora/internal/runtime/gvisor"
	"github.com/vorash/vora/internal/workspace"
)

// ipcProbe makes the raw syscalls through ctypes, so no libc wrapper or Python module
// can change what the sandbox answers. Each mode prints a JSON object of integers:
// errnos with 0 for success, and in "find" the number of objects found by index.
const ipcProbe = `
import ctypes, json, os, subprocess, sys

nr = json.loads(sys.argv[1])
mode = sys.argv[2]
libc = ctypes.CDLL(None, use_errno=True)
buf = ctypes.create_string_buffer(8192)
KEY, IPC_CREAT, IPC_NOWAIT, IPC_STAT = 0x766f7261, 0o1000, 0o4000, 2
SHM_STAT, SEM_STAT_ANY, MSG_STAT_ANY = 13, 20, 13
QUEUE = b"vora-probe"


def raw(name, *args):
    conv = [a if isinstance(a, ctypes.Array) else ctypes.c_char_p(a) if isinstance(a, bytes) else ctypes.c_long(a) for a in args]
    ctypes.set_errno(0)
    ret = libc.syscall(ctypes.c_long(nr[name]), *conv)
    return ret, ctypes.get_errno()


def errno(name, *args):
    ret, err = raw(name, *args)
    return err if ret == -1 else 0


def every_id(name, *args):
    # An id is small and predictable, so a guess would find an object if one existed.
    got = {errno(name, i, *args) for i in range(8)}
    return got.pop() if len(got) == 1 else -1


if mode == "calls":
    print(json.dumps({
        "shmget": errno("shmget", KEY, 4096, IPC_CREAT | 0o600),
        "semget": errno("semget", KEY, 1, IPC_CREAT | 0o600),
        "msgget": errno("msgget", KEY, IPC_CREAT | 0o600),
        "mq_open": errno("mq_open", QUEUE, os.O_CREAT | os.O_RDWR, 0o600, 0),
        "mq_unlink": errno("mq_unlink", QUEUE),
        "mq_timedsend": errno("mq_timedsend", 0, b"x", 1, 0, 0),
        "mq_timedreceive": errno("mq_timedreceive", 0, buf, 8192, 0, 0),
        "mq_notify": errno("mq_notify", 0, 0),
        "mq_getsetattr": errno("mq_getsetattr", 0, 0, 0),
        "shmat": every_id("shmat", 0, 0),
        "shmctl": every_id("shmctl", IPC_STAT, buf),
        "semop": every_id("semop", buf, 1),
        "semctl": every_id("semctl", 0, IPC_STAT, buf),
        "msgsnd": every_id("msgsnd", buf, 1, IPC_NOWAIT),
        "msgrcv": every_id("msgrcv", buf, 1, 0, IPC_NOWAIT),
        "msgctl": every_id("msgctl", IPC_STAT, buf),
    }))
elif mode == "child":
    # The filter has to reach a process that left the session of the one the backend started.
    done = subprocess.run([sys.executable, "-c", sys.argv[3], sys.argv[1], "calls"], stdout=subprocess.PIPE, start_new_session=True, check=True)
    sys.stdout.write(done.stdout.decode())
elif mode == "plant":
    queue, queue_err = raw("mq_open", QUEUE, os.O_CREAT | os.O_RDWR, 0o600, 0)
    print(json.dumps({
        "shm": errno("shmget", KEY, 4096, IPC_CREAT | 0o600),
        "sem": errno("semget", KEY, 1, IPC_CREAT | 0o600),
        "msg": errno("msgget", KEY, IPC_CREAT | 0o600),
        "mq": queue_err if queue == -1 else 0,
    }))
elif mode == "find":
    # By index, which needs no key and finds what any earlier run left. /proc/sysvipc
    # doesn't exist under gVisor.
    def live(name, *args):
        return sum(1 for i in range(64) if raw(name, i, *args)[0] != -1)
    queue, queue_err = raw("mq_open", QUEUE, os.O_RDWR, 0, 0)
    print(json.dumps({
        "shm": live("shmctl", SHM_STAT, buf),
        "sem": live("semctl", 0, SEM_STAT_ANY, buf),
        "msg": live("msgctl", MSG_STAT_ANY, buf),
        "mq": queue_err if queue == -1 else 0,
    }))
elif mode == "seccomp":
    status = dict(line.split(":", 1) for line in open("/proc/self/status"))
    print(json.dumps({"seccomp": int(status["Seccomp"])}))
`

// ordinaryPython is what a normal program uses for concurrency: POSIX semaphores and
// shared memory in /dev/shm, which the reset cleans, and pipes. numpy is used too if the
// image has it, and the output says which.
const ordinaryPython = `
import array, mmap, multiprocessing as mp
from multiprocessing import shared_memory

ctx = mp.get_context("fork")
sem = ctx.Semaphore(2)
sem.acquire()
sem.release()
with ctx.Lock():
    pass
queue = ctx.Queue()
queue.put("through a queue")
assert queue.get(timeout=10) == "through a queue"
with ctx.Pool(2) as pool:
    assert pool.map(abs, [-1, -2, -3]) == [1, 2, 3]
shm = shared_memory.SharedMemory(create=True, size=4096)
shm.buf[0] = 42
reader = shared_memory.SharedMemory(name=shm.name)
assert reader.buf[0] == 42
reader.close()
shm.close()
shm.unlink()
assert sum(array.array("d", [1.0, 2.0])) == 3.0
with mmap.mmap(-1, 4096) as anon:
    anon[0] = 7
try:
    import numpy
except ImportError:
    print("ok without numpy")
else:
    assert int(numpy.arange(1000).sum()) == 499500
    print("ok with numpy")
`

// ipcSandbox is a real sandbox and how to reset it on its backend.
type ipcSandbox struct {
	backend runtime.Backend
	h       runtime.Handle
	// capture returns what tryReset needs to put the sandbox back.
	capture  func(t *testing.T) []byte
	tryReset func(manifest []byte) error
}

func (s ipcSandbox) reset(t *testing.T, manifest []byte) {
	t.Helper()
	if err := s.tryReset(manifest); err != nil {
		t.Fatalf("reset: %v", err)
	}
}

// ipcSyscallNumbers is ipcProbe's first argument.
func ipcSyscallNumbers(t *testing.T) string {
	t.Helper()
	numbers, err := json.Marshal(map[string]int{
		"shmget": unix.SYS_SHMGET, "semget": unix.SYS_SEMGET, "msgget": unix.SYS_MSGGET,
		"shmat": unix.SYS_SHMAT, "shmctl": unix.SYS_SHMCTL, "semop": unix.SYS_SEMOP, "semctl": unix.SYS_SEMCTL,
		"msgsnd": unix.SYS_MSGSND, "msgrcv": unix.SYS_MSGRCV, "msgctl": unix.SYS_MSGCTL,
		"mq_open": unix.SYS_MQ_OPEN, "mq_unlink": unix.SYS_MQ_UNLINK, "mq_timedsend": unix.SYS_MQ_TIMEDSEND,
		"mq_timedreceive": unix.SYS_MQ_TIMEDRECEIVE, "mq_notify": unix.SYS_MQ_NOTIFY, "mq_getsetattr": unix.SYS_MQ_GETSETATTR,
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(numbers)
}

func (s ipcSandbox) probe(t *testing.T, mode string) map[string]int {
	t.Helper()
	out := mustExec(t, s.backend, s.h, "/usr/bin/python3", "-c", ipcProbe, ipcSyscallNumbers(t), mode, ipcProbe)
	var got map[string]int
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("probe %s printed %q: %v", mode, out, err)
	}
	return got
}

func wantProbe(t *testing.T, got, want map[string]int) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("got %d results, want %d: %v", len(got), len(want), got)
	}
	for name, value := range want {
		if result, ok := got[name]; !ok || result != value {
			t.Errorf("%s = %d, want %d", name, result, value)
		}
	}
}

// ipcDenied is what ipcProbe's "calls" mode prints under the filter. Every create call is
// denied, so nothing can exist for the others to act on.
var ipcDenied = map[string]int{
	"shmget": int(unix.EPERM), "semget": int(unix.EPERM), "msgget": int(unix.EPERM),
	"mq_open": int(unix.EPERM), "mq_unlink": int(unix.EPERM), "mq_timedsend": int(unix.EPERM),
	"mq_timedreceive": int(unix.EPERM), "mq_notify": int(unix.EPERM), "mq_getsetattr": int(unix.EPERM),
	"shmat": int(unix.EINVAL), "shmctl": int(unix.EINVAL), "semop": int(unix.EINVAL), "semctl": int(unix.EINVAL),
	"msgsnd": int(unix.EINVAL), "msgrcv": int(unix.EINVAL), "msgctl": int(unix.EINVAL),
}

// testIPCDenied is what a sandbox that will be reset must guarantee on either backend.
func testIPCDenied(t *testing.T, s ipcSandbox) {
	eperm := int(unix.EPERM)
	wantProbe(t, s.probe(t, "calls"), ipcDenied)
	wantProbe(t, s.probe(t, "child"), ipcDenied)

	// A normal concurrent program must not notice.
	out := mustExec(t, s.backend, s.h, "/usr/bin/python3", "-c", ordinaryPython)
	if !strings.HasPrefix(out, "ok ") {
		t.Errorf("ordinary program printed %q", out)
	}
	t.Log(strings.TrimSpace(out))

	// The next run finds nothing, and the denial still holds after the reset.
	manifest := s.capture(t)
	wantProbe(t, s.probe(t, "plant"), map[string]int{"shm": eperm, "sem": eperm, "msg": eperm, "mq": eperm})
	s.reset(t, manifest)
	wantProbe(t, s.probe(t, "find"), map[string]int{"shm": 0, "sem": 0, "msg": 0, "mq": eperm})
	wantProbe(t, s.probe(t, "calls"), ipcDenied)
}

// testIPCAllowed is an ordinary sandbox, which must keep working as it did, and also the
// control for testIPCDenied: with the denial off, a reset really does leave these behind.
func testIPCAllowed(t *testing.T, s ipcSandbox, wantSeccomp int) {
	if got := s.probe(t, "seccomp")["seccomp"]; got != wantSeccomp {
		t.Errorf("Seccomp = %d, want %d", got, wantSeccomp)
	}
	manifest := s.capture(t)
	wantProbe(t, s.probe(t, "plant"), map[string]int{"shm": 0, "sem": 0, "msg": 0, "mq": 0})
	s.reset(t, manifest)
	wantProbe(t, s.probe(t, "find"), map[string]int{"shm": 1, "sem": 1, "msg": 1, "mq": 0})
}

func newBwrapIPCSandbox(t *testing.T, denyIPC bool) ipcSandbox {
	t.Helper()
	rs := newResetSandboxWith(t, denyIPC)
	return ipcSandbox{
		backend: rs.backend,
		h:       rs.h,
		capture: rs.capture,
		tryReset: func(manifest []byte) error {
			_, err := rs.reset(manifest)
			return err
		},
	}
}

func newGvisorIPCSandbox(t *testing.T, denyIPC bool) ipcSandbox {
	t.Helper()
	if os.Getuid() != 0 {
		t.Skip("gVisor Method 2 requires a rootlesskit userns or root (run under rootlesskit)")
	}
	if _, err := os.Stat("/dev/kvm"); err != nil {
		t.Skipf("/dev/kvm unavailable: %v", err)
	}
	rootfs := imageRootfs(t, "base")

	cfg := config.Config{StateDir: t.TempDir(), GvisorPlatform: "kvm", GvisorRunRoot: filepath.Join(os.Getenv("XDG_RUNTIME_DIR"), "vora-gvisor-itest")}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	subtree, err := cgroup.NewSubtree()
	if err != nil {
		t.Skipf("cgroup subtree unavailable: %v", err)
	}
	backend, err := gvisor.New(cfg, logger, subtree)
	if err != nil {
		t.Skipf("gvisor backend unavailable: %v", err)
	}

	id := "sbx_" + ulid.Make().String()
	ws := workspace.NewManager(cfg.SandboxesDir())
	wsp := ws.Paths(id)
	wsp.RootfsDir = rootfs
	if _, err := ws.Create(wsp); err != nil {
		t.Fatalf("workspace: %v", err)
	}
	t.Cleanup(func() { _ = ws.Destroy(id) })

	ctx := context.Background()
	h, err := backend.Create(ctx, runtime.SandboxSpec{
		ID:        id,
		Image:     "base",
		Resources: runtime.ResourceLimits{CPUMillis: 1000, MemoryBytes: 256 << 20, Pids: 64},
		Network:   runtime.NetworkPolicy{Mode: runtime.NetworkNone},
		TTL:       time.Minute,
		Workspace: wsp,
		DenyIPC:   denyIPC,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := backend.Start(ctx, h); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = backend.Destroy(context.Background(), h) })

	return ipcSandbox{
		backend: backend,
		h:       h,
		capture: func(t *testing.T) []byte {
			t.Helper()
			return []byte(mustExec(t, backend, h, "/vora/vora-init", "capture"))
		},
		tryReset: func(manifest []byte) error {
			stdout, stderr, code := execSandbox(t, backend, h, runtime.ExecRequest{Command: "/vora/vora-init", Args: []string{"reset"}, Stdin: manifest})
			if code != 0 {
				return fmt.Errorf("vora-init reset: exit %d, stdout %q, stderr %q", code, stdout, stderr)
			}
			return nil
		},
	}
}

func TestBwrapDenyIPC(t *testing.T) { testIPCDenied(t, newBwrapIPCSandbox(t, true)) }

// bwrap sandboxes always have a filter, so the agent's own one is the 2 here.
func TestBwrapIPCAllowedByDefault(t *testing.T) { testIPCAllowed(t, newBwrapIPCSandbox(t, false), 2) }

func TestGvisorDenyIPC(t *testing.T) { testIPCDenied(t, newGvisorIPCSandbox(t, true)) }

// An ordinary gVisor sandbox has no filter at all.
func TestGvisorIPCAllowedByDefault(t *testing.T) { testIPCAllowed(t, newGvisorIPCSandbox(t, false), 0) }
