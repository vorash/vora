//go:build linux

package sandboxreset

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

const (
	fakeBwrap = 1
	fakeAgent = 2
)

// fakeKernel is a sandbox of two processes, which reads as the same thing until a test
// changes it.
type fakeKernel struct {
	threads   map[int][]int
	attrs     map[int]ThreadAttrs
	threadErr map[int]error
	listErr   error
	rlimits   map[int][]Rlimit
	rlimitErr error
	files     map[int]map[string]string
	fileErr   map[string]error
	slack     error
	sysctls   map[string]string
	sysctlErr error
}

var plainThread = ThreadAttrs{Affinity: "ffff", Sched: unix.SchedAttr{Size: 56, Util_max: 1024}}

func newFakeKernel() *fakeKernel {
	k := &fakeKernel{
		threads:   map[int][]int{fakeBwrap: {1}, fakeAgent: {2, 3, 4}},
		attrs:     map[int]ThreadAttrs{},
		threadErr: map[int]error{},
		rlimits:   map[int][]Rlimit{},
		files:     map[int]map[string]string{},
		fileErr:   map[string]error{},
		slack:     unix.EPERM,
		sysctls:   map[string]string{"kernel/msgmax": "8192\n", "kernel/sem": "32000\t1024000000\t500\t32000\n"},
	}
	for _, tid := range []int{1, 2, 3, 4} {
		k.attrs[tid] = plainThread
	}
	for _, pid := range []int{fakeBwrap, fakeAgent} {
		k.rlimits[pid] = []Rlimit{{Soft: 1024, Hard: 4096}, {Soft: math.MaxUint64, Hard: math.MaxUint64}}
		k.files[pid] = map[string]string{"oom_score_adj": "200\n", "coredump_filter": "00000033\n"}
	}
	return k
}

func (k *fakeKernel) ThreadIDs(pid int) ([]int, error) {
	return k.threads[pid], k.listErr
}

func (k *fakeKernel) Thread(tid int) (ThreadAttrs, error) {
	if err := k.threadErr[tid]; err != nil {
		return ThreadAttrs{}, err
	}
	return k.attrs[tid], nil
}

func (k *fakeKernel) Rlimits(pid int) ([]Rlimit, error) {
	return k.rlimits[pid], k.rlimitErr
}

func (k *fakeKernel) ProcFile(pid int, name string) (string, error) {
	if name == timerSlackFile {
		if k.slack != nil {
			return "", k.slack
		}
		return "50000\n", nil
	}
	if err := k.fileErr[name]; err != nil {
		return "", err
	}
	content, ok := k.files[pid][name]
	if !ok {
		return "", &fs.PathError{Op: "open", Path: name, Err: unix.ENOENT}
	}
	return content, nil
}

func (k *fakeKernel) Sysctls(context.Context) (map[string]string, error) {
	return maps.Clone(k.sysctls), k.sysctlErr
}

var bothPIDs = []int{fakeBwrap, fakeAgent}

func captureFake(t *testing.T, k *fakeKernel) Manifest {
	t.Helper()
	processes, sysctls, err := CaptureKernel(context.Background(), k, bothPIDs)
	if err != nil {
		t.Fatalf("CaptureKernel: %v", err)
	}
	return Manifest{Processes: processes, Sysctls: sysctls}
}

func TestUntouchedSandboxPassesTheCheck(t *testing.T) {
	k := newFakeKernel()
	m := captureFake(t, k)
	if err := CheckKernel(context.Background(), k, bothPIDs, m); err != nil {
		t.Fatalf("CheckKernel: %v", err)
	}
}

// The manifest crosses the host as JSON, so a value it can't carry exactly would fail every
// reset or, worse, compare equal to something else. RLIM_INFINITY is the largest there is.
func TestKernelStateSurvivesTheManifestEncoding(t *testing.T) {
	k := newFakeKernel()
	m := captureFake(t, k)
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	var back Manifest
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(back, m) {
		t.Fatalf("round trip changed the manifest\nbefore %+v\nafter  %+v", m, back)
	}
	if err := CheckKernel(context.Background(), k, bothPIDs, back); err != nil {
		t.Fatalf("CheckKernel with the decoded manifest: %v", err)
	}
}

// Every attribute is here because a workload can set it on a process that outlives it and
// a later run sees the result. Each one has to fail the check by itself, on any thread.
func TestAnyChangedThreadAttributeFailsTheCheck(t *testing.T) {
	cases := map[string]func(*ThreadAttrs){
		"nice":      func(a *ThreadAttrs) { a.Sched.Nice = 13 },
		"ioprio":    func(a *ThreadAttrs) { a.Ioprio = 16387 },
		"affinity":  func(a *ThreadAttrs) { a.Affinity = "3" },
		"policy":    func(a *ThreadAttrs) { a.Sched.Policy = 5 },
		"utilclamp": func(a *ThreadAttrs) { a.Sched.Util_max = 512 },
	}
	for name, change := range cases {
		for _, tid := range []int{1, 2, 4} {
			t.Run(fmt.Sprintf("%s on thread %d", name, tid), func(t *testing.T) {
				k := newFakeKernel()
				m := captureFake(t, k)
				changed := k.attrs[tid]
				change(&changed)
				k.attrs[tid] = changed
				err := CheckKernel(context.Background(), k, bothPIDs, m)
				if !errors.Is(err, ErrProcessChanged) {
					t.Fatalf("err = %v, want ErrProcessChanged", err)
				}
				if !strings.Contains(err.Error(), fmt.Sprintf("thread %d:", tid)) {
					t.Fatalf("err = %v, want it to name thread %d", err, tid)
				}
			})
		}
	}
}

// A Go program starts threads whenever it likes, so a different count alone must pass or
// every warm member would be retired at random.
func TestThreadCountMovingAloneDoesNotFailTheCheck(t *testing.T) {
	k := newFakeKernel()
	m := captureFake(t, k)

	k.threads[fakeAgent] = []int{2, 3, 4, 9}
	k.attrs[9] = plainThread
	if err := CheckKernel(context.Background(), k, bothPIDs, m); err != nil {
		t.Fatalf("with one more thread: %v", err)
	}
	k.threads[fakeAgent] = []int{2, 3}
	if err := CheckKernel(context.Background(), k, bothPIDs, m); err != nil {
		t.Fatalf("with one fewer thread: %v", err)
	}
}

// A thread clones its parent's attributes. If one a workload changed starts another, the
// new thread has no entry of its own to be compared against, but must still be held to
// the baseline.
func TestThreadBornAfterCaptureIsHeldToTheBaseline(t *testing.T) {
	k := newFakeKernel()
	m := captureFake(t, k)
	k.threads[fakeAgent] = []int{2, 3, 4, 9}
	tainted := plainThread
	tainted.Sched.Nice = 13
	k.attrs[9] = tainted
	if err := CheckKernel(context.Background(), k, bothPIDs, m); !errors.Is(err, ErrProcessChanged) {
		t.Fatalf("err = %v, want ErrProcessChanged", err)
	}
}

// A thread can exit between the listing and its read, and takes its attributes with it.
func TestThreadThatExitedDuringTheCheckIsNoFailure(t *testing.T) {
	k := newFakeKernel()
	m := captureFake(t, k)
	k.threadErr[4] = fmt.Errorf("sched_getattr: %w", unix.ESRCH)
	if err := CheckKernel(context.Background(), k, bothPIDs, m); err != nil {
		t.Fatalf("CheckKernel: %v", err)
	}
}

// Skipping a process that has gone would call a sandbox clean with its agent dead.
func TestProcessThatIsGoneFailsTheCheck(t *testing.T) {
	k := newFakeKernel()
	m := captureFake(t, k)
	k.threadErr[fakeAgent] = fmt.Errorf("sched_getattr: %w", unix.ESRCH)
	if err := CheckKernel(context.Background(), k, bothPIDs, m); !errors.Is(err, ErrProcessChanged) {
		t.Fatalf("err = %v, want ErrProcessChanged", err)
	}

	k = newFakeKernel()
	m = captureFake(t, k)
	k.threads[fakeAgent] = []int{3, 4}
	if err := CheckKernel(context.Background(), k, bothPIDs, m); !errors.Is(err, ErrProcessChanged) {
		t.Fatalf("listing without the process: err = %v, want ErrProcessChanged", err)
	}
}

func TestChangedRlimitFailsTheCheck(t *testing.T) {
	for name, change := range map[string]func([]Rlimit){
		"soft": func(r []Rlimit) { r[0].Soft = 777 },
		"hard": func(r []Rlimit) { r[1].Hard = 777 },
	} {
		t.Run(name, func(t *testing.T) {
			k := newFakeKernel()
			m := captureFake(t, k)
			changed := slices.Clone(k.rlimits[fakeAgent])
			change(changed)
			k.rlimits[fakeAgent] = changed
			if err := CheckKernel(context.Background(), k, bothPIDs, m); !errors.Is(err, ErrProcessChanged) {
				t.Fatalf("err = %v, want ErrProcessChanged", err)
			}
		})
	}
}

func TestChangedProcessFileFailsTheCheck(t *testing.T) {
	k := newFakeKernel()
	m := captureFake(t, k)
	k.files[fakeBwrap]["coredump_filter"] = "0000007f\n"
	if err := CheckKernel(context.Background(), k, bothPIDs, m); !errors.Is(err, ErrProcessChanged) {
		t.Fatalf("err = %v, want ErrProcessChanged", err)
	}
}

// gVisor has no autogroup. That has to be recorded as such, or a file that appears, or one
// that vanishes, would go unnoticed.
func TestProcessFileMayBeMissingButOnlyAsCaptured(t *testing.T) {
	k := newFakeKernel()
	m := captureFake(t, k)
	if _, ok := m.Processes[0].Files["autogroup"]; ok {
		t.Fatalf("files = %v, want no autogroup", m.Processes[0].Files)
	}
	if err := CheckKernel(context.Background(), k, bothPIDs, m); err != nil {
		t.Fatalf("a file missing at both ends: %v", err)
	}

	k.files[fakeBwrap]["autogroup"] = "/autogroup-1 nice 0\n"
	if err := CheckKernel(context.Background(), k, bothPIDs, m); !errors.Is(err, ErrProcessChanged) {
		t.Fatalf("a file that appeared: err = %v, want ErrProcessChanged", err)
	}

	delete(k.files[fakeBwrap], "autogroup")
	delete(k.files[fakeBwrap], "oom_score_adj")
	if err := CheckKernel(context.Background(), k, bothPIDs, m); !errors.Is(err, ErrProcessChanged) {
		t.Fatalf("a file that vanished: err = %v, want ErrProcessChanged", err)
	}
}

// A hostile limit can make any of these reads fail, so a failed read can never be clean.
func TestUnreadableStateFailsTheCheck(t *testing.T) {
	boom := errors.New("boom")
	cases := map[string]struct {
		breakIt func(*fakeKernel)
		want    error
	}{
		"thread list":  {func(k *fakeKernel) { k.listErr = boom }, ErrProcessChanged},
		"thread":       {func(k *fakeKernel) { k.threadErr[3] = boom }, ErrProcessChanged},
		"rlimits":      {func(k *fakeKernel) { k.rlimitErr = boom }, ErrProcessChanged},
		"process file": {func(k *fakeKernel) { k.fileErr["oom_score_adj"] = unix.EACCES }, ErrProcessChanged},
		"sysctl walk":  {func(k *fakeKernel) { k.sysctlErr = boom }, ErrSysctlChanged},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			k := newFakeKernel()
			m := captureFake(t, k)
			tc.breakIt(k)
			err := CheckKernel(context.Background(), k, bothPIDs, m)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

// If capture left a value out, a later change to it would pass. So capture fails instead.
func TestUnreadableStateFailsTheCapture(t *testing.T) {
	boom := errors.New("boom")
	cases := map[string]func(*fakeKernel){
		"thread list":  func(k *fakeKernel) { k.listErr = boom },
		"thread":       func(k *fakeKernel) { k.threadErr[3] = boom },
		"the process":  func(k *fakeKernel) { k.threadErr[fakeAgent] = unix.ESRCH },
		"rlimits":      func(k *fakeKernel) { k.rlimitErr = boom },
		"process file": func(k *fakeKernel) { k.fileErr["oom_score_adj"] = unix.EACCES },
		"sysctl walk":  func(k *fakeKernel) { k.sysctlErr = boom },
		"sysctl value": func(k *fakeKernel) { k.sysctls["kernel/hostname"] = "\xff" },
	}
	for name, breakIt := range cases {
		t.Run(name, func(t *testing.T) {
			k := newFakeKernel()
			breakIt(k)
			if _, _, err := CaptureKernel(context.Background(), k, bothPIDs); err == nil {
				t.Fatal("CaptureKernel succeeded")
			}
		})
	}
}

// One baseline can only stand for threads that agree. Disagreeing threads mean something
// has already been at them, and that is no start state to keep.
func TestCaptureRefusesThreadsThatDisagree(t *testing.T) {
	k := newFakeKernel()
	changed := k.attrs[3]
	changed.Sched.Nice = 13
	k.attrs[3] = changed
	if _, _, err := CaptureKernel(context.Background(), k, bothPIDs); err == nil || !strings.Contains(err.Error(), "thread 3") {
		t.Fatalf("err = %v, want it to name thread 3", err)
	}
}

// The timer slack isn't compared, on the ground that nothing here can reach it. If a read
// ever works, that ground has gone, and a sandbox that could change a value this package
// doesn't look at must not be admitted or reused.
func TestTimerSlackHasToBeOutOfReach(t *testing.T) {
	for name, slack := range map[string]error{
		"refused, as under bwrap": &fs.PathError{Op: "read", Path: timerSlackFile, Err: unix.EPERM},
		"absent, as under gVisor": &fs.PathError{Op: "open", Path: timerSlackFile, Err: unix.ENOENT},
	} {
		t.Run(name, func(t *testing.T) {
			k := newFakeKernel()
			k.slack = slack
			m := captureFake(t, k)
			if err := CheckKernel(context.Background(), k, bothPIDs, m); err != nil {
				t.Fatalf("CheckKernel: %v", err)
			}
		})
	}
	for name, slack := range map[string]error{
		"readable":                nil,
		"failing some other way":  &fs.PathError{Op: "read", Path: timerSlackFile, Err: unix.EIO},
		"refused for wrong cause": &fs.PathError{Op: "open", Path: timerSlackFile, Err: unix.EACCES},
	} {
		t.Run(name, func(t *testing.T) {
			k := newFakeKernel()
			m := captureFake(t, k)
			k.slack = slack
			if err := CheckKernel(context.Background(), k, bothPIDs, m); !errors.Is(err, ErrProcessChanged) {
				t.Fatalf("CheckKernel: err = %v, want ErrProcessChanged", err)
			}
			if _, _, err := CaptureKernel(context.Background(), k, bothPIDs); err == nil {
				t.Fatal("CaptureKernel succeeded")
			}
		})
	}
}

// The manifest is what stops a run. One that holds none of this, or that leaves the agent
// out, must not pass for having nothing to compare.
func TestManifestThatDoesNotCoverTheSandboxFailsTheCheck(t *testing.T) {
	k := newFakeKernel()
	whole := captureFake(t, k)
	cases := map[string]struct {
		m    Manifest
		want error
	}{
		"no processes":   {Manifest{Sysctls: whole.Sysctls}, ErrProcessChanged},
		"no agent":       {Manifest{Processes: whole.Processes[:1], Sysctls: whole.Sysctls}, ErrProcessChanged},
		"no init":        {Manifest{Processes: whole.Processes[1:], Sysctls: whole.Sysctls}, ErrProcessChanged},
		"an extra PID":   {Manifest{Processes: append(slices.Clone(whole.Processes), Process{PID: 7}), Sysctls: whole.Sysctls}, ErrProcessChanged},
		"no sysctls":     {Manifest{Processes: whole.Processes}, ErrSysctlChanged},
		"an old version": {Manifest{}, ErrProcessChanged},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if err := CheckKernel(context.Background(), k, bothPIDs, tc.m); !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestChangedSysctlsFailTheCheck(t *testing.T) {
	cases := map[string]struct {
		change func(map[string]string)
		named  string
	}{
		"a changed value": {func(s map[string]string) { s["kernel/msgmax"] = "4096\n" }, "kernel/msgmax"},
		"a file gone":     {func(s map[string]string) { delete(s, "kernel/sem") }, "kernel/sem"},
		"a new file":      {func(s map[string]string) { s["net/ipv4/conf/eth0/forwarding"] = "0\n" }, "net/ipv4/conf/eth0/forwarding"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			k := newFakeKernel()
			m := captureFake(t, k)
			tc.change(k.sysctls)
			err := CheckKernel(context.Background(), k, bothPIDs, m)
			if !errors.Is(err, ErrSysctlChanged) {
				t.Fatalf("err = %v, want ErrSysctlChanged", err)
			}
			if !strings.Contains(err.Error(), tc.named) {
				t.Fatalf("err = %v, want it to name %s", err, tc.named)
			}
		})
	}
}

// This goes to an operator through gRPC, however many files a run changed.
func TestSysctlErrorStaysShort(t *testing.T) {
	want, got := map[string]string{}, map[string]string{}
	for i := range 40 {
		want[fmt.Sprintf("kernel/s%02d", i)] = "1\n"
		got[fmt.Sprintf("kernel/s%02d", i)] = "2\n"
	}
	changed := changedSysctls(want, got)
	if !strings.Contains(changed, "and 35 more") || strings.Contains(changed, "s39") {
		t.Fatalf("changed = %q", changed)
	}
}

// Under bwrap nothing is writable, so this is what every manifest there holds. The manifest
// crosses the host as JSON, and an empty set must not come back as no set at all, which
// fails the check.
func TestNothingWritableIsAResultAndNotAGap(t *testing.T) {
	k := newFakeKernel()
	k.sysctls = map[string]string{}
	m := captureFake(t, k)
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	var back Manifest
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if err := CheckKernel(context.Background(), k, bothPIDs, back); err != nil {
		t.Fatalf("CheckKernel: %v", err)
	}
}

// The walk is skipped only where capture found nothing writable. A sandbox with even one
// writable sysctl gets compared at every reset, or gVisor's would go unchecked.
func TestCheckWalksUnlessCaptureFoundNothingWritable(t *testing.T) {
	boom := errors.New("boom")

	k := newFakeKernel()
	m := captureFake(t, k)
	k.sysctlErr = boom
	if err := CheckKernel(context.Background(), k, bothPIDs, m); !errors.Is(err, ErrSysctlChanged) {
		t.Fatalf("with sysctls captured: err = %v, want ErrSysctlChanged", err)
	}

	k = newFakeKernel()
	k.sysctls = map[string]string{}
	m = captureFake(t, k)
	k.sysctlErr = boom
	if err := CheckKernel(context.Background(), k, bothPIDs, m); err != nil {
		t.Fatalf("with nothing captured: %v, want no walk", err)
	}
}

func writeSysctl(t *testing.T, root, rel, value string, mode os.FileMode) {
	t.Helper()
	path := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(value), mode); err != nil {
		t.Fatal(err)
	}
}

// access(2) answers yes to root for any mode, and that is what the walk relies on.
func skipIfRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root can write every file")
	}
}

// A file this process could not write, a workload of the same uid and capabilities could
// not write either, and recording it would only make the manifest bigger and let a value
// the kernel moves by itself fail every reset.
func TestSysctlWalkRecordsWhatThisProcessCouldWrite(t *testing.T) {
	skipIfRoot(t)
	root := t.TempDir()
	writeSysctl(t, root, "kernel/msgmax", "8192\n", 0o644)
	writeSysctl(t, root, "kernel/sem", "1\t2\n", 0o666)
	writeSysctl(t, root, "fs/mqueue/msg_max", "10\n", 0o644)
	writeSysctl(t, root, "fs/file-nr", "99\n", 0o444)
	if err := os.Symlink("../fs/file-nr", filepath.Join(root, "kernel/link")); err != nil {
		t.Fatal(err)
	}

	got, err := liveKernel{sysctlRoot: root}.Sysctls(context.Background())
	if err != nil {
		t.Fatalf("Sysctls: %v", err)
	}
	want := map[string]string{"kernel/msgmax": "8192\n", "kernel/sem": "1\t2\n", "fs/mqueue/msg_max": "10\n"}
	if !maps.Equal(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// A workload could change this file and nothing could see it. Either it stops the capture
// or the check goes blind to it.
func TestSysctlWalkFailsOnAFileItCanWriteButNotRead(t *testing.T) {
	skipIfRoot(t)
	root := t.TempDir()
	writeSysctl(t, root, "kernel/msgmax", "8192\n", 0o200)
	if _, err := (liveKernel{sysctlRoot: root}).Sysctls(context.Background()); err == nil {
		t.Fatal("Sysctls succeeded")
	}
}

func TestSysctlWalkFailsWithoutATree(t *testing.T) {
	if _, err := (liveKernel{sysctlRoot: filepath.Join(t.TempDir(), "missing")}).Sysctls(context.Background()); err == nil {
		t.Fatal("Sysctls succeeded: a /proc without sys would pass as nothing to compare")
	}
}

func TestSysctlWalkStopsWithTheContext(t *testing.T) {
	root := t.TempDir()
	writeSysctl(t, root, "kernel/msgmax", "8192\n", 0o644)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := (liveKernel{sysctlRoot: root}).Sysctls(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

// The live reader only reads, so it can run on this test's own process. Reading a second
// time has to give the same answer, because that is what a reset does.
func TestLiveReaderReadsThisProcess(t *testing.T) {
	k := liveKernel{sysctlRoot: t.TempDir()}
	pid := os.Getpid()

	tids, err := k.ThreadIDs(pid)
	if err != nil || !slices.Contains(tids, pid) {
		t.Fatalf("ThreadIDs = %v, %v", tids, err)
	}
	attrs, err := k.Thread(pid)
	if err != nil {
		t.Fatalf("Thread: %v", err)
	}
	if attrs.Affinity == "" || attrs.Affinity == "0" {
		t.Fatalf("Thread = %+v: a thread runs on some CPU", attrs)
	}
	if attrs.Sched.Size == 0 {
		t.Fatalf("Thread = %+v: sched_getattr filled nothing in", attrs)
	}
	rlimits, err := k.Rlimits(pid)
	if err != nil || len(rlimits) != rlimitCount {
		t.Fatalf("Rlimits = %v, %v", rlimits, err)
	}
	var nofile unix.Rlimit
	if err := unix.Getrlimit(unix.RLIMIT_NOFILE, &nofile); err != nil {
		t.Fatal(err)
	}
	if got := rlimits[unix.RLIMIT_NOFILE]; got.Soft != nofile.Cur || got.Hard != nofile.Max {
		t.Fatalf("RLIMIT_NOFILE = %+v, getrlimit says %+v", got, nofile)
	}
	if _, err := k.ProcFile(pid, "oom_score_adj"); err != nil {
		t.Fatalf("ProcFile: %v", err)
	}
	if _, err := k.Thread(1 << 30); !errors.Is(err, unix.ESRCH) {
		t.Fatalf("Thread on nothing: err = %v, want ESRCH so that a thread that exited can be told apart", err)
	}
	if _, err := k.ProcFile(1<<30, "oom_score_adj"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("ProcFile on nothing: err = %v, want a missing file", err)
	}
}

func TestAffinityStringDropsOnlyTrailingZeroWords(t *testing.T) {
	var low, high, none unix.CPUSet
	low.Set(0)
	low.Set(1)
	high.Set(64)
	if got := affinityString(&low); got != "3" {
		t.Errorf("low = %q", got)
	}
	if got := affinityString(&high); got != "0,1" {
		t.Errorf("high = %q", got)
	}
	if got := affinityString(&none); got != "0" {
		t.Errorf("none = %q", got)
	}
}
