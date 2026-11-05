package main

import (
	"fmt"
	"os"
	"os/exec"
	goruntime "runtime"
	"strings"
	"sync"
)

// workloadOOMScoreAdj makes every workload process the OOM killer's first
// choice in the sandbox cgroup. The kernel kills the process with the highest
// badness, roughly its RSS plus adj/1000 of the cgroup limit, so at 1000 any
// workload process outranks the agent however small it is. Killing the agent
// would end the sandbox; killing a workload process only ends its exec.
const workloadOOMScoreAdj = "1000"

// oomScore raises the agent's own OOM score across a fork, so the child inherits
// the raised score before any user code runs, and so does everything it forks.
//
// Nothing else does this without a cost: os/exec has no pre-exec hook, writing
// /proc/<pid>/oom_score_adj after Start is too late because the child has already
// exec'd, and a re-exec shim needs OS threads of its own, which count against the
// sandbox's pids limit and crash a Go process when refused.
//
// An unprivileged process may lower its score again as long as it stays at or
// above oom_score_adj_min, which its baseline is. The agent is the likeliest
// victim only for the length of the fork, and a kill then ends this sandbox alone.
type oomScore struct {
	// file stays open from startup because a write needs no further permission
	// check, which keeps this working if the agent later stops being dumpable.
	file     *os.File
	baseline string
}

func openOOMScore() (*oomScore, error) {
	f, err := os.OpenFile("/proc/self/oom_score_adj", os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	buf := make([]byte, 16)
	n, err := f.ReadAt(buf, 0)
	if n == 0 {
		_ = f.Close()
		return nil, fmt.Errorf("read oom_score_adj: %w", err)
	}
	return &oomScore{file: f, baseline: strings.TrimSpace(string(buf[:n]))}, nil
}

// startWorkload starts cmd with the raised score. A failure to raise or restore
// is logged by the caller and does not stop the exec: it only changes who the OOM
// killer picks, and the command the caller asked for should still run.
func (o *oomScore) startWorkload(cmd *exec.Cmd) (startErr, scoreErr error) {
	if o == nil {
		return cmd.Start(), nil
	}
	if _, err := o.file.WriteAt([]byte(workloadOOMScoreAdj), 0); err != nil {
		return cmd.Start(), fmt.Errorf("raise oom_score_adj: %w", err)
	}
	startErr = cmd.Start()
	if _, err := o.file.WriteAt([]byte(o.baseline), 0); err != nil {
		scoreErr = fmt.Errorf("restore oom_score_adj: %w", err)
	}
	return startErr, scoreErr
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
