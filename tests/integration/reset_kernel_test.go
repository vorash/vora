//go:build integration

package integration

import (
	"encoding/json"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/vorash/vora/internal/runtime"
	"github.com/vorash/vora/internal/sandboxreset"
)

// kernelTools is what each change below is written with. Every helper reads back what it
// set and raises if the kernel did not take it, so a change that did not happen cannot
// pass as a reset that noticed something else. A kernel without the feature, such as
// autogroup, or whose pid_max is not per namespace yet, gives a run nothing to change, so
// the helper exits with kernelUnsupported and the test says so by skipping.
const kernelTools = `
import ctypes, errno, json, os, resource, struct, sys

UNSUPPORTED = 77
libc = ctypes.CDLL(None, use_errno=True)
SYS_IOPRIO_SET, SYS_IOPRIO_GET, SYS_SCHED_SETATTR, SYS_SCHED_GETATTR = json.loads(sys.argv[1])
SCHED_ATTR = "IIQiIQQQII"


def syscall(nr, *args):
    ctypes.set_errno(0)
    ret = libc.syscall(ctypes.c_long(nr), *args)
    if ret == -1:
        raise OSError(ctypes.get_errno(), "syscall %d" % nr)
    return ret


def threads(pid):
    return sorted(int(t) for t in os.listdir("/proc/%d/task" % pid))


def nice(tid):
    os.setpriority(os.PRIO_PROCESS, tid, 13)
    assert os.getpriority(os.PRIO_PROCESS, tid) == 13


def affinity(tid):
    if len(os.sched_getaffinity(tid)) < 2:
        sys.exit(UNSUPPORTED)
    cpus = set(sorted(os.sched_getaffinity(tid))[:1])
    os.sched_setaffinity(tid, cpus)
    assert os.sched_getaffinity(tid) == cpus


def ioprio(tid):
    syscall(SYS_IOPRIO_SET, ctypes.c_long(1), ctypes.c_long(tid), ctypes.c_long(16387))
    assert syscall(SYS_IOPRIO_GET, ctypes.c_long(1), ctypes.c_long(tid)) == 16387


def policy(tid):
    os.sched_setscheduler(tid, os.SCHED_IDLE, os.sched_param(0))
    assert os.sched_getscheduler(tid) == os.SCHED_IDLE


def uclamp(tid):
    keep_all, util_min, util_max = 0x18, 0x20, 0x40
    attr = struct.pack(SCHED_ATTR, 56, 0, keep_all | util_min | util_max, 0, 0, 0, 0, 0, 0, 512)
    try:
        syscall(SYS_SCHED_SETATTR, ctypes.c_long(tid), ctypes.c_char_p(attr), ctypes.c_long(0))
    except OSError as e:
        if e.errno == errno.EOPNOTSUPP:
            sys.exit(UNSUPPORTED)
        raise
    buf = ctypes.create_string_buffer(56)
    syscall(SYS_SCHED_GETATTR, ctypes.c_long(tid), buf, ctypes.c_long(56), ctypes.c_long(0))
    assert struct.unpack(SCHED_ATTR, buf.raw)[9] == 512


def rlimit(pid):
    soft, hard = resource.prlimit(pid, resource.RLIMIT_NOFILE)
    resource.prlimit(pid, resource.RLIMIT_NOFILE, (777, hard))
    assert resource.prlimit(pid, resource.RLIMIT_NOFILE) == (777, hard)


def put(path, value):
    try:
        before = open(path).read()
        with open(path, "w") as f:
            f.write(value)
    except (FileNotFoundError, PermissionError):
        sys.exit(UNSUPPORTED)
    after = open(path).read()
    assert after != before, "%s stayed %r" % (path, after)


def oom_score_adj(pid):
    put("/proc/%d/oom_score_adj" % pid, "123")


def coredump_filter(pid):
    put("/proc/%d/coredump_filter" % pid, "0x7f")


def autogroup(pid):
    put("/proc/%d/autogroup" % pid, "5")


`

// kernelSandbox makes a fresh sandbox for one change, because a reset that notices one
// leaves a member that must be destroyed.
type kernelSandbox func(t *testing.T) ipcSandbox

const (
	processChanged, sysctlChanged = "process attribute changed", "sysctl changed"
	kernelUnsupported             = 77
)

// wantKernelResetFails checks that a real run's change fails the reset, and that the reason
// given is the change. The same sandbox's untouched reset passed first, so nothing else
// could be what fails it.
func wantKernelResetFails(t *testing.T, sandbox kernelSandbox, change, wantErr, wantNamed string) {
	t.Helper()
	s := sandbox(t)
	manifest := s.capture(t)
	s.reset(t, manifest)
	numbers := fmt.Sprintf("[%d, %d, %d, %d]", unix.SYS_IOPRIO_SET, unix.SYS_IOPRIO_GET, unix.SYS_SCHED_SETATTR, unix.SYS_SCHED_GETATTR)
	stdout, stderr, code := execSandbox(t, s.backend, s.h, runtime.ExecRequest{Command: "/usr/bin/python3", Args: []string{"-c", kernelTools + change, numbers}})
	if code == kernelUnsupported {
		t.Skipf("this kernel does not offer %s", change)
	}
	if code != 0 {
		t.Fatalf("%s: exit %d\nstdout: %s\nstderr: %s", change, code, stdout, stderr)
	}
	err := s.tryReset(manifest)
	if err == nil {
		t.Fatalf("reset succeeded after %s", change)
	}
	if !strings.Contains(err.Error(), wantErr) || !strings.Contains(err.Error(), wantNamed) {
		t.Fatalf("err = %v, want %q naming %q", err, wantErr, wantNamed)
	}
}

// Later execs inherit these from the agent or read them from PID 1, so a run that changes
// one has reached the next run. A target is an expression for a thread id: a process's
// first thread, or one other thread alone, which has to fail the same way.
func testThreadAttributes(t *testing.T, sandbox kernelSandbox, targets map[string]string, attrs []string) {
	for targetName, tid := range targets {
		for _, attr := range attrs {
			t.Run(attr+" of "+targetName, func(t *testing.T) {
				wantKernelResetFails(t, sandbox, fmt.Sprintf("%s(%s)", attr, tid), processChanged, "thread ")
			})
		}
	}
}

func testProcessAttributes(t *testing.T, sandbox kernelSandbox, pid int, attrs []string) {
	for _, attr := range attrs {
		t.Run(fmt.Sprintf("%s of PID %d", attr, pid), func(t *testing.T) {
			wantKernelResetFails(t, sandbox, fmt.Sprintf("%s(%d)", attr, pid), processChanged, fmt.Sprintf("PID %d", pid))
		})
	}
}

// testSysctls takes each sysctl a run can really change on a backend. They are listed by
// hand, not from the manifest, so one that falls out of the walk fails here.
func testSysctls(t *testing.T, sandbox kernelSandbox, sysctls map[string]string) {
	for _, path := range slices.Sorted(maps.Keys(sysctls)) {
		t.Run(path, func(t *testing.T) {
			change := fmt.Sprintf("put(%q, %q)", "/proc/sys/"+path, sysctls[path])
			wantKernelResetFails(t, sandbox, change, sysctlChanged, path)
		})
	}
}

func bwrapKernelSandbox(t *testing.T) ipcSandbox { return newBwrapIPCSandbox(t, true) }

func gvisorKernelSandbox(t *testing.T) ipcSandbox { return newGvisorIPCSandbox(t, true) }

func TestResetFailsOnKernelStateBwrap(t *testing.T) {
	testThreadAttributes(t, bwrapKernelSandbox, map[string]string{
		"the agent":             "2",
		"one agent thread only": "threads(2)[-1]",
		"PID 1":                 "1",
	}, []string{"nice", "affinity", "ioprio", "policy", "uclamp"})
	testProcessAttributes(t, bwrapKernelSandbox, 2, []string{"rlimit"})
	testProcessAttributes(t, bwrapKernelSandbox, 1, []string{"oom_score_adj", "coredump_filter", "autogroup"})
}

// openSysctlsForWriting names what each open did. It stops at the open and never writes:
// without the read-only bind, kernel/cad_pid would set the host's value.
const openSysctlsForWriting = `
import errno, json, os, sys
got = {}
for name in json.loads(sys.argv[1]):
    try:
        os.close(os.open("/proc/sys/" + name, os.O_WRONLY))
        got[name] = "opened"
    except OSError as e:
        got[name] = errno.errorcode[e.errno]
print(json.dumps(got))
`

// These were writable before /proc/sys became a read-only bind: the IPC and PID namespace
// sysctls a user namespace owns, and kernel/cad_pid, which acts on the host. A manifest with
// nothing in it is what lets a reset skip the walk, so both have to hold.
func TestSysctlsAreReadOnlyBwrap(t *testing.T) {
	s := bwrapKernelSandbox(t)
	manifest := s.capture(t)
	if m := decodeManifest(t, manifest); m.Sysctls == nil || len(m.Sysctls) != 0 {
		t.Errorf("manifest sysctls = %q, want an empty set", m.Sysctls)
	}
	names := []string{
		"fs/mqueue/msg_default", "fs/mqueue/msg_max", "fs/mqueue/msgsize_default", "fs/mqueue/msgsize_max",
		"fs/mqueue/queues_max", "kernel/cad_pid", "kernel/msgmax", "kernel/msgmnb", "kernel/msgmni",
		"kernel/ns_last_pid", "kernel/pid_max", "kernel/sem", "kernel/shm_rmid_forced", "kernel/shmall",
		"kernel/shmmax", "kernel/shmmni",
	}
	list, err := json.Marshal(names)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]string
	out := mustExec(t, s.backend, s.h, "/usr/bin/python3", "-c", openSysctlsForWriting, string(list))
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("printed %q: %v", out, err)
	}
	for _, name := range names {
		if got[name] != "EROFS" {
			t.Errorf("%s: open for writing gave %s, want EROFS", name, got[name])
		}
	}
	s.reset(t, manifest)
}

// plantUnderBinfmt tries what a run could leave in a writable mount. Only a regular file it
// can still write would show in the sysctl walk, so the others would pass a reset unseen.
const plantUnderBinfmt = `
import errno, json, os
dir = "/proc/sys/fs/binfmt_misc"
got = set()
def attempt(call, *args):
    try:
        call(*args)
        got.add("planted")
    except OSError as e:
        got.add(errno.errorcode[e.errno])
attempt(os.mkdir, dir + "/dir")
attempt(os.symlink, "anything", dir + "/link")
attempt(os.open, dir + "/file", os.O_CREAT | os.O_WRONLY, 0o444)
print(json.dumps(sorted(got)))
`

// The host's binfmt_misc is hidden with a tmpfs, and a reset does not look under /proc.
func TestBinfmtMaskKeepsNothingBwrap(t *testing.T) {
	s := bwrapKernelSandbox(t)
	manifest := s.capture(t)
	if got := strings.TrimSpace(mustExec(t, s.backend, s.h, "/usr/bin/python3", "-c", plantUnderBinfmt)); strings.Contains(got, "planted") {
		t.Fatalf("a run could write under binfmt_misc: %s", got)
	}
	s.reset(t, manifest)
}

// PID 1 is a workload's uid and outlives it, so without a mask a write here clears its
// Referenced and soft-dirty bits or its schedstats, and a later run reads the difference
// from smaps_rollup, pagemap or sched. Nothing compares those, so this has to stay refused.
func TestPID1StatisticsAreOutOfReachBwrap(t *testing.T) {
	s := bwrapKernelSandbox(t)
	manifest := s.capture(t)
	const write = `
import errno, json, os
got = set()
for path in ("/proc/1/clear_refs", "/proc/1/task/1/clear_refs", "/proc/1/sched", "/proc/1/task/1/sched"):
    try:
        fd = os.open(path, os.O_WRONLY)
        try:
            os.write(fd, b"4")
        finally:
            os.close(fd)
        got.add("wrote")
    except OSError as e:
        got.add(errno.errorcode[e.errno])
print(json.dumps(sorted(got)))
`
	if got := strings.TrimSpace(mustExec(t, s.backend, s.h, "/usr/bin/python3", "-c", write)); got != `["EACCES"]` {
		t.Fatalf("writing PID 1's statistics gave %s, want refusal", got)
	}
	s.reset(t, manifest)
}

// refusedWrite is a write to a long-lived process's file that the kernel has to refuse. The
// comparison does not look at these because a run cannot change them, and this is what
// holds that up: the file modes say otherwise.
type refusedWrite struct {
	path, value, want string
}

const tryWrites = `
import errno, json, os, sys
got = []
for path, value in json.loads(sys.argv[1]):
    try:
        fd = os.open(path, os.O_WRONLY)
        try:
            os.write(fd, value.encode())
        finally:
            os.close(fd)
        got.append("wrote")
    except OSError as e:
        got.append(errno.errorcode[e.errno])
print(json.dumps(got))
`

func testRefusedWrites(t *testing.T, s ipcSandbox, writes []refusedWrite) {
	t.Helper()
	manifest := s.capture(t)
	pairs := make([][2]string, len(writes))
	for i, w := range writes {
		pairs[i] = [2]string{w.path, w.value}
	}
	list, err := json.Marshal(pairs)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	out := mustExec(t, s.backend, s.h, "/usr/bin/python3", "-c", tryWrites, string(list))
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("printed %q: %v", out, err)
	}
	for i, w := range writes {
		if got[i] != w.want {
			t.Errorf("%s <- %q: %s, want %s", w.path, w.value, got[i], w.want)
		}
	}
	s.reset(t, manifest)
}

func TestLongLivedFilesRefuseWritesBwrap(t *testing.T) {
	writes := []refusedWrite{
		{"/proc/1/loginuid", "4294967295", "EPERM"},
		{"/proc/1/uid_map", "0 0 1", "EPERM"},
		{"/proc/1/gid_map", "0 0 1", "EPERM"},
		{"/proc/1/projid_map", "0 0 1", "EPERM"},
		{"/proc/1/timens_offsets", "monotonic 0 0", "EPERM"},
		{"/proc/1/comm", "zz", "EINVAL"},
		{"/proc/1/setgroups", "deny", "EACCES"},
		{"/proc/1/mem", "x", "EACCES"},
	}
	for _, pid := range []string{"1", "2"} {
		for _, name := range []string{"current", "exec", "fscreate", "keycreate", "sockcreate", "smack/current", "apparmor/current", "apparmor/exec"} {
			writes = append(writes, refusedWrite{"/proc/" + pid + "/attr/" + name, "x", "EPERM"})
		}
	}
	testRefusedWrites(t, bwrapKernelSandbox(t), writes)
}

func TestLongLivedFilesRefuseWritesGvisor(t *testing.T) {
	testRefusedWrites(t, gvisorKernelSandbox(t), []refusedWrite{
		{"/proc/1/setgroups", "deny", "EPERM"},
		{"/proc/self/setgroups", "deny", "EPERM"},
		{"/proc/1/uid_map", "0 0 1", "EPERM"},
		{"/proc/self/uid_map", "0 0 1", "EPERM"},
		{"/proc/1/gid_map", "0 0 1", "EPERM"},
		{"/proc/self/gid_map", "0 0 1", "EPERM"},
		{"/proc/1/task/1/children", "x", "EIO"},
		{"/proc/1/comm", "zz", "EINVAL"},
		{"/proc/1/mem", "x", "EACCES"},
		{"/proc/sys/kernel/pid_max", "65536", "EIO"},
		{"/proc/sys/kernel/randomize_va_space", "0", "EIO"},
		{"/proc/sys/kernel/keys/maxkeys", "100", "EPERM"},
		{"/proc/sys/fs/pipe-max-size", "65536", "EIO"},
	})
}

// listWritable names every file under /proc and /sys that opens for writing, and every
// directory that takes a new entry, leaving out the caller's own process, whose files go
// with it. It only opens: under /proc a write could reach the host.
const listWritable = `
import json, os, re, stat
me = str(os.getpid())
found = []
def visit(root, e):
    if e.is_symlink():
        return
    if root == "/proc" and e.name.isdigit() and (e.name == me or e.name not in ("1", "2")):
        return
    if root == "/proc" and e.name in ("self", "thread-self", "net"):
        return
    # Out of scope: these reach what a process holds open, not state a run can leave
    # behind. The workload can open bwrap's fd 2, the runner's stderr pipe, but a pipe's
    # data does not outlive the sandbox and the runner bounds what it reads.
    if re.fullmatch(r"/proc/\d+(/task/\d+)?", root) and e.name in ("fd", "fdinfo", "map_files", "ns", "net"):
        return
    if e.is_dir(follow_symlinks=False):
        if os.access(e.path, os.W_OK):
            found.append(e.path + "/")
        walk(e.path)
    elif stat.S_ISREG(e.stat(follow_symlinks=False).st_mode):
        try:
            os.close(os.open(e.path, os.O_WRONLY | os.O_NONBLOCK))
            found.append(e.path)
        except OSError:
            pass
def walk(root):
    try:
        entries = list(os.scandir(root))
    except OSError:
        return
    for e in entries:
        try:
            visit(root, e)
        except FileNotFoundError:
            pass
walk("/proc")
walk("/sys")
print(json.dumps(sorted(found)))
`

// accountedFor is why each writable path may exist. A new one fails the test until someone
// finds out what it does, which is how kernel.cad_pid would have been caught. A task's
// directory repeats its process's files.
var accountedForBwrap = map[string]string{
	`/proc/1(/task/1)?/(oom_adj|oom_score_adj)`:      "compared",
	`/proc/1(/task/1)?/coredump_filter`:              "compared",
	`/proc/1/autogroup`:                              "compared",
	`/proc/[12]/timerslack_ns`:                       "refused, and sealTimerSlack proves it",
	`/proc/[12](/task/[0-9]+)?/attr/.*`:              "refused: only a task's own, TestLongLivedFilesRefuseWritesBwrap",
	`/proc/1(/task/1)?/(comm|loginuid)`:              "refused, TestLongLivedFilesRefuseWritesBwrap",
	`/proc/1(/task/1)?/(uid_map|gid_map|projid_map)`: "refused, TestLongLivedFilesRefuseWritesBwrap",
	`/proc/1/timens_offsets`:                         "refused, TestLongLivedFilesRefuseWritesBwrap",
}

var accountedForGvisor = map[string]string{
	`/proc/sys/.*`:        "the Sentry's: each is compared, or a static file that refuses writes",
	`/proc/sysrq-trigger`: "the Sentry's static file, which does nothing",
	`/proc/1(/task/[0-9]+)?/(oom_score_adj|coredump_filter)`:           "compared",
	`/proc/1(/task/[0-9]+)?/(comm|setgroups|uid_map|gid_map|children)`: "refused, TestLongLivedFilesRefuseWritesGvisor",
}

func testEveryWritablePathIsAccountedFor(t *testing.T, s ipcSandbox, accounted map[string]string) {
	t.Helper()
	patterns := make([]*regexp.Regexp, 0, len(accounted))
	for pattern := range accounted {
		patterns = append(patterns, regexp.MustCompile("^"+pattern+"$"))
	}
	var found []string
	out := mustExec(t, s.backend, s.h, "/usr/bin/python3", "-c", listWritable)
	if err := json.Unmarshal([]byte(out), &found); err != nil {
		t.Fatalf("printed %q: %v", out, err)
	}
	if len(found) == 0 {
		t.Fatal("found nothing writable, not even PID 1's oom_score_adj, so this proved nothing")
	}
	t.Logf("%d writable paths: %s", len(found), strings.Join(found, " "))
	for _, path := range found {
		if !slices.ContainsFunc(patterns, func(p *regexp.Regexp) bool { return p.MatchString(path) }) {
			t.Errorf("%s is writable and nothing says what it does", path)
		}
	}
}

func TestEveryWritablePathIsAccountedForBwrap(t *testing.T) {
	testEveryWritablePathIsAccountedFor(t, bwrapKernelSandbox(t), accountedForBwrap)
}

func TestEveryWritablePathIsAccountedForGvisor(t *testing.T) {
	testEveryWritablePathIsAccountedFor(t, gvisorKernelSandbox(t), accountedForGvisor)
}

func TestResetFailsOnKernelStateGvisor(t *testing.T) {
	testThreadAttributes(t, gvisorKernelSandbox, map[string]string{
		"PID 1":                 "1",
		"one PID 1 thread only": "threads(1)[-1]",
	}, []string{"nice", "affinity", "ioprio", "policy"})
	testProcessAttributes(t, gvisorKernelSandbox, 1, []string{"oom_score_adj", "coredump_filter"})
	testSysctls(t, gvisorKernelSandbox, map[string]string{
		"fs/mount-max":                 "5000",
		"fs/nr_open":                   "1048576",
		"kernel/yama/ptrace_scope":     "0",
		"net/ipv4/ip_forward":          "1",
		"net/ipv4/ip_local_port_range": "20000 30000",
		"net/ipv4/tcp_recovery":        "0",
		"net/ipv4/tcp_rmem":            "4096 65536 131072",
		"net/ipv4/tcp_sack":            "0",
		"net/ipv4/tcp_wmem":            "4096 65536 131072",
	})
}

// rlimitOfPID1 prints what a workload's attempt on PID 1's limits did.
const rlimitOfPID1 = `
try:
    rlimit(1)
    print("changed")
except PermissionError:
    print("refused")
`

// PID 1's limits are compared like the agent's, but whether a workload reaches them is the
// host's call: this is refused with EACCES on vora-mini, where AppArmor is enabled, and the
// agent's are not. So the test holds either way: refused, or caught.
func TestRlimitOfPID1Bwrap(t *testing.T) {
	s := bwrapKernelSandbox(t)
	manifest := s.capture(t)
	numbers := fmt.Sprintf("[%d, %d, %d, %d]", unix.SYS_IOPRIO_SET, unix.SYS_IOPRIO_GET, unix.SYS_SCHED_SETATTR, unix.SYS_SCHED_GETATTR)
	got := strings.TrimSpace(mustExec(t, s.backend, s.h, "/usr/bin/python3", "-c", kernelTools+rlimitOfPID1, numbers))
	t.Logf("the workload's change to PID 1's limits was %s", got)
	err := s.tryReset(manifest)
	switch {
	case got == "refused" && err != nil:
		t.Fatalf("reset failed after a change that was refused: %v", err)
	case got == "changed" && (err == nil || !strings.Contains(err.Error(), processChanged)):
		t.Fatalf("err = %v, want %q", err, processChanged)
	}
}

// gVisor's prlimit ignores the PID it is given: it sets and reads the caller's own limits,
// even through /proc/1/limits, and the next exec is back at the start. So a workload cannot
// reach PID 1's limits at all, and what the helper compares is its own, which every exec
// gets from the bundle. If gVisor starts honouring the PID, this fails with the reset
// noticing, which is the sign to add rlimit to the changes tested above.
func TestRlimitOfPID1StaysWithTheCallerGvisor(t *testing.T) {
	s := gvisorKernelSandbox(t)
	manifest := s.capture(t)
	const readNofile = `
import resource
print(resource.prlimit(1, resource.RLIMIT_NOFILE)[0], resource.getrlimit(resource.RLIMIT_NOFILE)[0])
`
	before := mustExec(t, s.backend, s.h, "/usr/bin/python3", "-c", readNofile)
	mustExec(t, s.backend, s.h, "/usr/bin/python3", "-c", `
import resource
soft, hard = resource.prlimit(1, resource.RLIMIT_NOFILE)
resource.prlimit(1, resource.RLIMIT_NOFILE, (777, hard))
assert resource.getrlimit(resource.RLIMIT_NOFILE)[0] == 777
`)
	if after := mustExec(t, s.backend, s.h, "/usr/bin/python3", "-c", readNofile); after != before {
		t.Fatalf("limits went from %q to %q: prlimit now reaches another process", before, after)
	}
	s.reset(t, manifest)
}

// The timer slack is not in the manifest, because nothing in a sandbox can change another
// task's. If that stops being true, this goes red with the check still blind to it, so
// the writes are tried on every thread rather than judged from the file's 0666 mode.
const writeTimerSlack = `
import errno, json, os
got = set()
for pid in [p for p in (1, 2) if os.path.isdir("/proc/%d/task" % p)]:
    for tid in os.listdir("/proc/%d/task" % pid):
        try:
            fd = os.open("/proc/%s/timerslack_ns" % tid, os.O_WRONLY)
            try:
                os.write(fd, b"123456")
            finally:
                os.close(fd)
            got.add("wrote")
        except OSError as e:
            got.add(errno.errorcode[e.errno])
print(json.dumps(sorted(got)))
`

func testTimerSlackOutOfReach(t *testing.T, s ipcSandbox, want string) {
	t.Helper()
	manifest := s.capture(t)
	got := strings.TrimSpace(mustExec(t, s.backend, s.h, "/usr/bin/python3", "-c", writeTimerSlack))
	if got != want {
		t.Fatalf("writing every long-lived task's timer slack gave %s, want %s", got, want)
	}
	s.reset(t, manifest)
}

func TestTimerSlackIsOutOfReachBwrap(t *testing.T) {
	testTimerSlackOutOfReach(t, bwrapKernelSandbox(t), `["EPERM"]`)
}

func TestTimerSlackIsOutOfReachGvisor(t *testing.T) {
	testTimerSlackOutOfReach(t, gvisorKernelSandbox(t), `["ENOENT"]`)
}

// ordinaryRun forks, starts threads and changes its own nice, limits, policy and affinity,
// all of which are its own business and go with it.
const ordinaryRun = `
import os, resource, subprocess, threading
os.nice(5)
resource.setrlimit(resource.RLIMIT_NOFILE, (100, resource.getrlimit(resource.RLIMIT_NOFILE)[1]))
os.sched_setscheduler(0, os.SCHED_BATCH, os.sched_param(0))
os.sched_setaffinity(0, set(sorted(os.sched_getaffinity(0))[:1]))
def work():
    subprocess.run(["/bin/sh", "-c", "for i in 1 2 3 4 5; do /bin/true; done"], check=True)
ts = [threading.Thread(target=work) for _ in range(4)]
[t.start() for t in ts]
[t.join() for t in ts]
print(len(os.listdir("/proc/%d/task" % (2 if os.path.isdir("/proc/2/task") else 1))))
`

// One clean run must never lose a member, however many there are. A value that drifts by
// itself, or that the agent's own runtime moves, fails here before it fails a warm pool in
// production.
// A capture is a second reading of everything by the same code, so comparing them says
// what moved where a reset would only say that something did. leftover runs at the end of
// each run, for a backend whose reset kills what it starts.
func testCleanResetsRepeat(t *testing.T, s ipcSandbox, leftover string) {
	const resets = 30
	var captures, resetTimes []time.Duration

	start := time.Now()
	manifest := s.capture(t)
	captures = append(captures, time.Since(start))

	threads := map[string]int{}
	for i := range resets {
		out := mustExec(t, s.backend, s.h, "/usr/bin/python3", "-c", ordinaryRun+leftover)
		threads[strings.TrimSpace(out)]++
		if i%3 == 0 {
			start := time.Now()
			again := s.capture(t)
			captures = append(captures, time.Since(start))
			if diff := manifestKernelDiff(t, manifest, again); diff != "" {
				t.Errorf("capture after %d runs differs: %s", i+1, diff)
			}
		}
		start := time.Now()
		if err := s.tryReset(manifest); err != nil {
			t.Fatalf("clean reset %d of %d: %v", i+1, resets, err)
		}
		resetTimes = append(resetTimes, time.Since(start))
	}
	t.Logf("%d clean resets passed. Threads of the long-lived process, as runs saw them: %v", resets, threads)
	t.Logf("capture %s", describeLatency(captures))
	t.Logf("reset %s", describeLatency(resetTimes))
	reportManifest(t, manifest)
}

func manifestKernelDiff(t *testing.T, a, b []byte) string {
	t.Helper()
	before, after := decodeManifest(t, a), decodeManifest(t, b)
	var out []string
	if !maps.Equal(before.Sysctls, after.Sysctls) {
		out = append(out, fmt.Sprintf("sysctls %q vs %q", before.Sysctls, after.Sysctls))
	}
	if !slices.EqualFunc(before.Processes, after.Processes, func(x, y sandboxreset.Process) bool {
		return x.PID == y.PID && x.Threads == y.Threads && slices.Equal(x.Rlimits, y.Rlimits) && maps.Equal(x.Files, y.Files)
	}) {
		out = append(out, fmt.Sprintf("processes %+v vs %+v", before.Processes, after.Processes))
	}
	return strings.Join(out, "; ")
}

// reportManifest prints what a real walk found and what it added to the manifest, which
// travels within agentproto.MaxManifestBytes.
func reportManifest(t *testing.T, raw []byte) {
	t.Helper()
	m := decodeManifest(t, raw)
	paths := slices.Sorted(maps.Keys(m.Sysctls))
	for _, p := range m.Processes {
		t.Logf("PID %d: %+v", p.PID, p)
	}
	m.Processes, m.Sysctls = nil, nil
	without, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("manifest %d bytes, %d of them this check's. %d sysctls: %s", len(raw), len(raw)-len(without), len(paths), strings.Join(paths, " "))
}

func TestCleanResetsRepeatBwrap(t *testing.T) {
	testCleanResetsRepeat(t, bwrapKernelSandbox(t), `
subprocess.Popen(["/bin/sleep", "300"], start_new_session=True, stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
`)
}

func TestCleanResetsRepeatGvisor(t *testing.T) {
	testCleanResetsRepeat(t, gvisorKernelSandbox(t), "")
}
