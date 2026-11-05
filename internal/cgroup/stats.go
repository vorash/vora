package cgroup

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/vorash/vora/internal/runtime"
)

// cgroupRoot is the cgroup v2 mount point.
var cgroupRoot = "/sys/fs/cgroup"

// readStats reads resource usage from the cgroup at cgroupPath under the mount.
func readStats(cgroupPath string) (*runtime.Stats, error) {
	return readStatsDir(filepath.Join(cgroupRoot, cgroupPath))
}

// readStatsDir reads resource usage from a cgroup v2 directory.
func readStatsDir(base string) (*runtime.Stats, error) {
	if _, err := os.Stat(base); err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNotFound
		}
		return nil, err
	}

	st := &runtime.Stats{
		MemoryBytes:   readScalar(base, "memory.current"),
		MemoryPeak:    readScalar(base, "memory.peak"),
		Pids:          readScalar(base, "pids.current"),
		OOMKills:      readKeyed(base, "memory.events", "oom_kill"),
		PidsLimitHits: readKeyed(base, "pids.events", "max"),
	}
	st.CPUMillis = readKeyed(base, "cpu.stat", "usage_usec") / 1000
	return st, nil
}

// readScalar reads a single integer from a cgroup file. Missing files and the
// literal "max" return 0.
func readScalar(dir, name string) int64 {
	data, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return 0
	}
	return parseInt(strings.TrimSpace(string(data)))
}

// readKeyed reads "key value" lines from a cgroup file (e.g. memory.events).
func readKeyed(dir, name, key string) int64 {
	data, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == key {
			return parseInt(fields[1])
		}
	}
	return 0
}

func parseInt(s string) int64 {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0
	}
	return n
}

// Reason maps observed cgroup statistics to a semantic exit reason.
func Reason(st *runtime.Stats) runtime.ExitReason {
	switch {
	case st == nil:
		return runtime.ReasonRuntimeFailure
	case st.OOMKills > 0:
		return runtime.ReasonOOMKilled
	case st.PidsLimitHits > 0:
		return runtime.ReasonPidsLimit
	default:
		return runtime.ReasonExited
	}
}

// ReasonFromSystemd maps a systemd unit Result to a semantic exit reason,
// falling back to cgroup statistics when the result is inconclusive.
//
// systemd Results include: success, exit-code, signal, core-dump, timeout,
// watchdog, oom-kill, start-limit-hit, resources.
func ReasonFromSystemd(result string, st *runtime.Stats) runtime.ExitReason {
	switch result {
	case "oom-kill":
		return runtime.ReasonOOMKilled
	case "timeout":
		return runtime.ReasonTimeout
	case "resources":
		return runtime.ReasonPolicyViolation
	case "watchdog", "core-dump", "start-limit-hit":
		return runtime.ReasonRuntimeFailure
	case "success":
		return runtime.ReasonExited
	case "exit-code", "signal", "":
		if st == nil {
			return runtime.ReasonExited
		}
		return Reason(st)
	default:
		return Reason(st)
	}
}

// ErrIsNotExist reports whether err is a not-exist error.
func ErrIsNotExist(err error) bool { return errors.Is(err, os.ErrNotExist) }
