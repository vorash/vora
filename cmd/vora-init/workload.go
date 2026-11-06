package main

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	goruntime "runtime"
	"sync"
	"syscall"
	"time"

	v1 "github.com/vorash/vora/api/proto/v1"
)

// workloadOOMScoreAdj makes every workload process the OOM killer's first
// choice in the sandbox cgroup. The kernel kills the process with the highest
// badness, roughly its RSS plus adj/1000 of the cgroup limit, so at 1000 any
// workload process outranks the agent however small it is. Killing the agent
// would end the sandbox; killing a workload process only ends its exec.
const workloadOOMScoreAdj = "1000"

// startWorkload starts cmd and raises the score of the process it became, which
// everything the workload forks then inherits. A failure to raise is logged by
// the caller and does not stop the exec: it only changes who the OOM killer
// picks, and the command the caller asked for should still run.
//
// The agent's own score never changes. Raising it across the fork so the child
// inherits the value looked tighter, but a fork at the memory limit stalls in
// reclaim for seconds, and an OOM in that window picks the agent: at the same
// score it is the largest process in the sandbox. The window left here, from the
// exec to the write, is microseconds unless the agent itself is stalled, and
// while the limit is being hit the processes hitting it already carry the
// raised score.
//
// Nothing else raises the child without a cost: os/exec has no pre-exec hook,
// ptrace is denied by seccomp, and a re-exec shim needs OS threads of its own,
// which count against the sandbox's pids limit and crash a Go process when
// refused.
func startWorkload(cmd *exec.Cmd) (startErr, scoreErr error) {
	if err := cmd.Start(); err != nil {
		return err, nil
	}
	return nil, raiseOOMScore(cmd.Process.Pid)
}

// startFailureReason says who a command that never started is down to. A path or a file
// the request got wrong is the command's own failure, as gVisor reports it, and says
// nothing of the sandbox. Anything else, such as a fork the sandbox could not do, does.
func startFailureReason(err error) v1.ExitReason {
	switch {
	case errors.Is(err, exec.ErrNotFound), errors.Is(err, fs.ErrNotExist), errors.Is(err, fs.ErrPermission),
		errors.Is(err, syscall.ENOTDIR), errors.Is(err, syscall.ENOEXEC):
		return v1.ExitReason_EXITED
	default:
		return v1.ExitReason_RUNTIME_FAILURE
	}
}

// raiseOOMScoreWait bounds the wait for a child's exec to finish. A child that stays
// unwritable, such as one running a binary it cannot read, costs its exec this much.
const raiseOOMScoreWait = time.Second

// raiseOOMScore writes the child's score, waiting out the end of its exec. Start
// returns when the exec closes the close-on-exec pipe, and the kernel does that before
// it makes the new image dumpable. Until then the child is non-dumpable like this
// agent, so its /proc files belong to root and the write fails with EACCES. A child
// that is short of CPU can stay there for milliseconds.
func raiseOOMScore(pid int) error {
	deadline := time.Now().Add(raiseOOMScoreWait)
	for {
		err := os.WriteFile(fmt.Sprintf("/proc/%d/oom_score_adj", pid), []byte(workloadOOMScoreAdj), 0)
		if err == nil || exited(pid) {
			return nil
		}
		if !errors.Is(err, fs.ErrPermission) || time.Now().After(deadline) {
			return fmt.Errorf("raise oom_score_adj: %w", err)
		}
		time.Sleep(time.Millisecond)
	}
}

// exited reports whether pid is gone or a zombie. A command that has already
// exited has nothing left to score, and a zombie's /proc files belong to root,
// so the write fails with EACCES rather than ESRCH.
func exited(pid int) bool {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return true
	}
	// The state follows the parenthesised comm, which may itself hold spaces.
	i := bytes.LastIndexByte(b, ')')
	return i < 0 || i+2 >= len(b) || b[i+2] == 'Z'
}

// spareThreads is how many threads fixThreads starts. The agent needs one running
// and one each for what can block at once: the signal loop, an exec's wait or fork,
// and a write to its log. The rest is margin, because running out is fatal.
const spareThreads = 5

// fixThreads gives the agent every thread it will need before any workload exists.
// Each thread counts against the sandbox's pids limit, and once a workload has used
// that limit up, a Go program that needs another one crashes with "fatal error:
// newosproc", which ends the sandbox. Go keeps idle threads, so starting them now
// means the agent has no reason to ask again.
//
// One P keeps the number needed small and independent of the host's core count. The
// agent only moves bytes and runs one exec at a time, so it costs nothing.
//
// A goroutine that locks its thread and then blocks holds that thread, so the next
// goroutine needs a new one. This counts threads without waiting on the scheduler's
// timing.
func fixThreads() {
	goruntime.GOMAXPROCS(1)

	var locked sync.WaitGroup
	release := make(chan struct{})
	locked.Add(spareThreads)
	for range spareThreads {
		go func() {
			goruntime.LockOSThread()
			defer goruntime.UnlockOSThread()
			locked.Done()
			<-release
		}()
	}
	locked.Wait()
	close(release)
}
