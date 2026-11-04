package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
)

// workloadArg is the hidden subcommand the agent re-executes itself with to
// start an exec.
const workloadArg = "workload"

// workloadOOMScoreAdj makes every workload process the OOM killer's first
// choice in the sandbox cgroup. The kernel kills the process with the highest
// badness, roughly its RSS plus adj/1000 of the cgroup limit, so at 1000 any
// workload process outranks the agent however small it is. Killing the agent
// would end the sandbox; killing a workload process only ends its exec.
const workloadOOMScoreAdj = "1000"

// workloadCommand returns the command for one exec, started through
// `vora-init workload` so the OOM score is raised before any user code runs
// and every process the workload forks inherits it. os/exec has no pre-exec
// hook, and writing /proc/<pid>/oom_score_adj after Start is too late: Start
// returns once the child has already exec'd, so the workload can fork or
// allocate first. Only the child raises its score; the agent keeps its own.
//
// The command is resolved here rather than in the shim, so a missing or
// non-executable command still fails Start and is reported as a runtime
// failure, not as an exit code from the shim.
func workloadCommand(command string, args []string, dir string) *exec.Cmd {
	cmd := exec.Command(command, args...)
	cmd.Dir = dir
	if cmd.Err == nil {
		// exec.Command looks up bare names only; check paths too, relative to
		// the directory the command will run in.
		path := cmd.Path
		if !filepath.IsAbs(path) {
			path = filepath.Join(dir, path)
		}
		_, cmd.Err = exec.LookPath(path)
	}
	if cmd.Err != nil {
		return cmd
	}
	shim := exec.Command("/proc/self/exe", append([]string{workloadArg, cmd.Path, command}, args...)...)
	shim.Dir = dir
	return shim
}

// runWorkload is the `vora-init workload` shim: it raises its own OOM score
// and replaces itself with the command. It never returns.
func runWorkload(path string, argv []string) {
	if err := os.WriteFile("/proc/self/oom_score_adj", []byte(workloadOOMScoreAdj), 0); err != nil {
		fmt.Fprintf(os.Stderr, "vora-init: raise oom_score_adj: %v\n", err)
		os.Exit(127)
	}
	err := syscall.Exec(path, argv, os.Environ())
	fmt.Fprintf(os.Stderr, "vora-init: exec %s: %v\n", path, err)
	os.Exit(127)
}
