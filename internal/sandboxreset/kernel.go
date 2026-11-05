//go:build linux

package sandboxreset

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

// Process is what one long-lived process of a sandbox looked like at capture. Later
// execs inherit from it, or can read it, so a run that changes it reaches the next run.
type Process struct {
	PID int `json:"pid"`
	// Threads is what every thread had at capture. The set of threads moves on its own
	// but their attributes do not, so a thread born later is held to this as well.
	Threads ThreadAttrs `json:"threads"`
	// Rlimits is indexed by resource. They belong to the process, not to a thread.
	Rlimits []Rlimit `json:"rlimits"`
	// Files maps each of processFiles to its content. A file this kernel does not
	// have is left out, and must still be missing at reset.
	Files map[string]string `json:"files"`
}

// ThreadAttrs is what one thread's owner can change from another process. A workload
// runs as the agent's uid, so it can reach these without any capability.
type ThreadAttrs struct {
	Ioprio   int    `json:"ioprio"`
	Affinity string `json:"affinity"`
	// Sched is sched_getattr's whole answer: the nice value, the policy, the utilization
	// clamps and the time slice.
	Sched unix.SchedAttr `json:"sched"`
}

// Rlimit is one resource limit.
type Rlimit struct {
	Soft uint64 `json:"soft"`
	Hard uint64 `json:"hard"`
}

// rlimitCount is the kernel's RLIM_NLIMITS.
const rlimitCount = 16

// processFiles are the files under /proc/<pid> that a workload can write and a later
// run can read. The agent's refuse a workload, because it is not dumpable, but PID 1's
// do not, on either backend.
var processFiles = []string{"oom_score_adj", "coredump_filter", "autogroup"}

// timerSlackFile is not compared: nothing in a sandbox can read or write another task's,
// because both take CAP_SYS_NICE over its user namespace, and gVisor has no such file.
// sealTimerSlack proves that instead of assuming it.
const timerSlackFile = "timerslack_ns"

// KernelReader is how Capture's and Reset's kernel checks read a sandbox. It only reads.
type KernelReader interface {
	// OpenForWrite opens a file under /proc/<pid> for writing and closes it. It writes
	// nothing.
	OpenForWrite(pid int, name string) error
	// ThreadIDs lists a process's threads.
	ThreadIDs(pid int) ([]int, error)
	// Thread reads one thread. A thread that is gone reads as an error holding ESRCH.
	Thread(tid int) (ThreadAttrs, error)
	// Rlimits returns a process's limits, indexed by resource.
	Rlimits(pid int) ([]Rlimit, error)
	// ProcFile returns the content of /proc/<pid>/<name>.
	ProcFile(pid int, name string) (string, error)
	// Sysctls maps every sysctl this process could write to its value. Keys are relative
	// to /proc/sys.
	Sysctls(ctx context.Context) (map[string]string, error)
}

// CaptureKernel reads what CheckKernel will compare, for the given long-lived processes.
// It fails if a value cannot be recorded: a value left out could change unnoticed. It
// also fails if a process's threads disagree, since nothing in a sandbox does that
// before a workload has run, and no single baseline would then hold.
func CaptureKernel(ctx context.Context, r KernelReader, pids []int) ([]Process, map[string]string, error) {
	if err := sealTimerSlack(r); err != nil {
		return nil, nil, fmt.Errorf("sandboxreset: capture: %w", err)
	}
	if err := sealPID1Memory(r); err != nil {
		return nil, nil, fmt.Errorf("sandboxreset: capture: %w", err)
	}
	processes := make([]Process, 0, len(pids))
	for _, pid := range pids {
		p, err := captureProcess(ctx, r, pid)
		if err != nil {
			return nil, nil, fmt.Errorf("sandboxreset: capture: PID %d: %w", pid, err)
		}
		processes = append(processes, p)
	}
	sysctls, err := r.Sysctls(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("sandboxreset: capture: sysctls: %w", err)
	}
	for path, value := range sysctls {
		// JSON would hand back a different value from the host.
		if !utf8.ValidString(value) {
			return nil, nil, fmt.Errorf("sandboxreset: capture: sysctl %s holds invalid UTF-8", path)
		}
	}
	return processes, sysctls, nil
}

func captureProcess(ctx context.Context, r KernelReader, pid int) (Process, error) {
	threads, err := readThreads(ctx, r, pid)
	if err != nil {
		return Process{}, err
	}
	p := Process{PID: pid, Threads: threads[pid]}
	for _, tid := range slices.Sorted(maps.Keys(threads)) {
		if threads[tid] != p.Threads {
			return Process{}, fmt.Errorf("thread %d has %+v, the process has %+v", tid, threads[tid], p.Threads)
		}
	}
	if p.Rlimits, err = r.Rlimits(pid); err != nil {
		return Process{}, fmt.Errorf("rlimits: %w", err)
	}
	if p.Files, err = readProcessFiles(r, pid); err != nil {
		return Process{}, err
	}
	return p, nil
}

// CheckKernel proves that what m recorded is still so, for exactly the processes in
// pids. The caller must have stopped everything else in the sandbox first. Anything it
// cannot read fails, under the same error as a change, since a hostile limit can make a
// read fail and that must not count as clean.
func CheckKernel(ctx context.Context, r KernelReader, pids []int, m Manifest) error {
	if !slices.EqualFunc(m.Processes, pids, func(p Process, pid int) bool { return p.PID == pid }) {
		return fmt.Errorf("%w: the manifest does not cover PIDs %v", ErrProcessChanged, pids)
	}
	if m.Sysctls == nil {
		return fmt.Errorf("%w: the manifest holds no sysctls", ErrSysctlChanged)
	}
	if err := sealTimerSlack(r); err != nil {
		return fmt.Errorf("%w: %w", ErrProcessChanged, err)
	}
	if err := sealPID1Memory(r); err != nil {
		return fmt.Errorf("%w: %w", ErrProcessChanged, err)
	}
	for _, want := range m.Processes {
		if err := checkProcess(ctx, r, want); err != nil {
			return fmt.Errorf("%w: PID %d: %w", ErrProcessChanged, want.PID, err)
		}
	}
	// Nothing writable at capture means a read-only mount covers the tree, and making any
	// of it writable takes a capability nothing in a sandbox has. Under bwrap the walk
	// was a third of a reset's time, spent on files that cannot have changed.
	if len(m.Sysctls) == 0 {
		return nil
	}
	got, err := r.Sysctls(ctx)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrSysctlChanged, err)
	}
	if changed := changedSysctls(m.Sysctls, got); changed != "" {
		return fmt.Errorf("%w: %s", ErrSysctlChanged, changed)
	}
	return nil
}

func checkProcess(ctx context.Context, r KernelReader, want Process) error {
	threads, err := readThreads(ctx, r, want.PID)
	if err != nil {
		return err
	}
	for _, tid := range slices.Sorted(maps.Keys(threads)) {
		if threads[tid] != want.Threads {
			return fmt.Errorf("thread %d: captured %+v, now %+v", tid, want.Threads, threads[tid])
		}
	}
	rlimits, err := r.Rlimits(want.PID)
	if err != nil {
		return fmt.Errorf("rlimits: %w", err)
	}
	if !slices.Equal(rlimits, want.Rlimits) {
		return fmt.Errorf("rlimits: captured %+v, now %+v", want.Rlimits, rlimits)
	}
	files, err := readProcessFiles(r, want.PID)
	if err != nil {
		return err
	}
	if !maps.Equal(files, want.Files) {
		return fmt.Errorf("files: captured %q, now %q", want.Files, files)
	}
	return nil
}

// readThreads reads every thread of a process. A thread may exit between the listing and
// its read, and takes what it held with it, but the process's first thread stands for
// the process and has to be there.
func readThreads(ctx context.Context, r KernelReader, pid int) (map[int]ThreadAttrs, error) {
	tids, err := r.ThreadIDs(pid)
	if err != nil {
		return nil, fmt.Errorf("list threads: %w", err)
	}
	if !slices.Contains(tids, pid) {
		return nil, fmt.Errorf("list threads: %v does not hold the process itself", tids)
	}
	threads := make(map[int]ThreadAttrs, len(tids))
	for _, tid := range tids {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		attrs, err := r.Thread(tid)
		if errors.Is(err, unix.ESRCH) && tid != pid {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("thread %d: %w", tid, err)
		}
		threads[tid] = attrs
	}
	return threads, nil
}

func readProcessFiles(r KernelReader, pid int) (map[string]string, error) {
	files := make(map[string]string, len(processFiles))
	for _, name := range processFiles {
		content, err := r.ProcFile(pid, name)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		files[name] = content
	}
	return files, nil
}

// sealTimerSlack fails unless this process is refused PID 1's timer slack, or the file
// does not exist. PID 1 is never the caller, and a task may always read its own. Reading
// and writing take the same capability, which the workload holds exactly when this
// process does, so a read that works means a workload could change a value this package
// does not compare.
func sealTimerSlack(r KernelReader) error {
	_, err := r.ProcFile(1, timerSlackFile)
	if errors.Is(err, unix.EPERM) || errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("%s: %w", timerSlackFile, err)
	}
	return fmt.Errorf("%s is readable here, so a workload could change it", timerSlackFile)
}

// sealPID1Memory fails if this process can open PID 1's memory for writing. PID 1 is
// dumpable and shares a workload's uid, and it outlives every run, so a workload that
// could open this could change PID 1 in a way no check here can see. What stops it is the
// host's Yama or AppArmor setting, not anything Vora controls, so a member on a host
// without them must not be reused. Opening it writes nothing.
func sealPID1Memory(r KernelReader) error {
	err := r.OpenForWrite(1, "mem")
	if errors.Is(err, unix.EACCES) || errors.Is(err, unix.EPERM) || errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("/proc/1/mem: %w", err)
	}
	return errors.New("/proc/1/mem opens for writing here, so a workload could rewrite PID 1")
}

// changedSysctls names what differs between two walks, a few entries at most. It is
// empty when they match. A file that appeared counts: a new one means the sandbox gained
// something, such as a network device, that the listing does not describe.
func changedSysctls(want, got map[string]string) string {
	const shown = 5
	var changed []string
	for _, path := range slices.Sorted(maps.Keys(want)) {
		value, ok := got[path]
		switch {
		case !ok:
			changed = append(changed, fmt.Sprintf("%s: captured %q, now gone", path, want[path]))
		case value != want[path]:
			changed = append(changed, fmt.Sprintf("%s: captured %q, now %q", path, want[path], value))
		}
	}
	for _, path := range slices.Sorted(maps.Keys(got)) {
		if _, ok := want[path]; !ok {
			changed = append(changed, fmt.Sprintf("%s: new, %q", path, got[path]))
		}
	}
	if len(changed) > shown {
		return fmt.Sprintf("%s and %d more", strings.Join(changed[:shown], ", "), len(changed)-shown)
	}
	return strings.Join(changed, ", ")
}

// LiveKernel reads the sandbox this process runs in. Nothing it does needs the target to
// be dumpable: the attributes come from syscalls that ask nothing of the caller, or at
// most the same uid, and processFiles are world-readable.
func LiveKernel() KernelReader { return liveKernel{sysctlRoot: "/proc/sys"} }

type liveKernel struct {
	sysctlRoot string
}

func (liveKernel) ThreadIDs(pid int) ([]int, error) {
	entries, err := os.ReadDir(fmt.Sprintf("/proc/%d/task", pid))
	if err != nil {
		return nil, err
	}
	tids := make([]int, 0, len(entries))
	for _, e := range entries {
		tid, err := strconv.Atoi(e.Name())
		if err != nil {
			return nil, fmt.Errorf("task entry %q: %w", e.Name(), err)
		}
		tids = append(tids, tid)
	}
	return tids, nil
}

func (liveKernel) Thread(tid int) (ThreadAttrs, error) {
	const ioprioWhoProcess = 1
	ioprio, _, errno := unix.Syscall(unix.SYS_IOPRIO_GET, ioprioWhoProcess, uintptr(tid), 0)
	if errno != 0 {
		return ThreadAttrs{}, fmt.Errorf("ioprio_get: %w", errno)
	}
	var cpus unix.CPUSet
	if err := unix.SchedGetaffinity(tid, &cpus); err != nil {
		return ThreadAttrs{}, fmt.Errorf("sched_getaffinity: %w", err)
	}
	sched, err := unix.SchedGetAttr(tid, 0)
	if err != nil {
		return ThreadAttrs{}, fmt.Errorf("sched_getattr: %w", err)
	}
	return ThreadAttrs{Ioprio: int(ioprio), Affinity: affinityString(&cpus), Sched: *sched}, nil
}

// affinityString is the mask in hex, words from the lowest CPU up, without the trailing
// zero words that a CPUSet's fixed size would otherwise add to every manifest.
func affinityString(cpus *unix.CPUSet) string {
	var words []string
	for _, word := range cpus {
		words = append(words, strconv.FormatUint(uint64(word), 16))
	}
	for len(words) > 1 && words[len(words)-1] == "0" {
		words = words[:len(words)-1]
	}
	return strings.Join(words, ",")
}

// Rlimits under gVisor gets the caller's own limits, because its prlimit ignores the PID
// for reading and for writing. That leaves nothing to detect there, and a helper's limits
// come from the bundle, so the comparison still holds.
func (liveKernel) Rlimits(pid int) ([]Rlimit, error) {
	rlimits := make([]Rlimit, rlimitCount)
	for resource := range rlimits {
		var got unix.Rlimit
		if err := unix.Prlimit(pid, resource, nil, &got); err != nil {
			return nil, fmt.Errorf("resource %d: %w", resource, err)
		}
		rlimits[resource] = Rlimit{Soft: got.Cur, Hard: got.Max}
	}
	return rlimits, nil
}

func (liveKernel) OpenForWrite(pid int, name string) error {
	f, err := os.OpenFile(fmt.Sprintf("/proc/%d/%s", pid, name), os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	return f.Close()
}

func (liveKernel) ProcFile(pid int, name string) (string, error) {
	content, err := os.ReadFile(fmt.Sprintf("/proc/%d/%s", pid, name))
	return string(content), err
}

// Sysctls also records what the kernel will refuse at write time, because access(2) only
// asks about the mode and the owner. Recording too many costs nothing while they hold
// still. A file a workload could write and this process cannot read fails the walk.
func (k liveKernel) Sysctls(ctx context.Context) (map[string]string, error) {
	sysctls := map[string]string{}
	err := filepath.WalkDir(k.sysctlRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(k.sysctlRoot, path)
		if err != nil {
			return err
		}
		switch err := unix.Access(path, unix.W_OK); {
		case errors.Is(err, unix.EACCES), errors.Is(err, unix.EPERM), errors.Is(err, unix.EROFS):
			return nil
		case err != nil:
			return fmt.Errorf("%s: %w", rel, err)
		}
		value, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		sysctls[rel] = string(value)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return sysctls, nil
}
