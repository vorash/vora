package cgroup

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/vorash/vora/internal/runtime"
)

// Window is an open usage window on one cgroup directory. The counters are
// cumulative for the cgroup's lifetime, so a window keeps their values at open
// and reports the difference.
type Window struct {
	dir   string
	peak  *os.File // memory.peak, held open so Stop reads this fd's watermark
	scope runtime.PeakScope
	cpu0  int64 // cpu.stat usage_usec
	oom0  int64 // memory.events oom_kill
	pids0 int64 // pids.events max
}

// OpenWindow snapshots the cgroup at dir (an absolute path) and starts a
// window. It returns ErrNotFound when dir does not exist.
func OpenWindow(dir string) (*Window, error) {
	if err := statCgroup(dir); err != nil {
		return nil, err
	}
	w := &Window{
		dir:   dir,
		cpu0:  readKeyed(dir, "cpu.stat", "usage_usec"),
		oom0:  readKeyed(dir, "memory.events", "oom_kill"),
		pids0: readKeyed(dir, "pids.events", "max"),
	}
	w.peak, w.scope = openPeak(filepath.Join(dir, "memory.peak"))
	return w, nil
}

// openPeak opens memory.peak and resets its watermark. Since kernel 6.12 a
// write of any non-empty string resets the peak to current usage for reads
// through that fd only, so other readers keep the lifetime value. Older
// kernels make the file read-only; the fd then reports the lifetime peak and
// the scope says so.
func openPeak(path string) (*os.File, runtime.PeakScope) {
	if f, err := os.OpenFile(path, os.O_RDWR, 0); err == nil {
		if _, err := f.Write([]byte("reset")); err == nil {
			return f, runtime.PeakWindow
		}
		_ = f.Close()
	}
	f, err := os.Open(path)
	if err != nil {
		// No memory.peak at all (kernel before 5.19, or no memory controller).
		return nil, runtime.PeakUnspecified
	}
	return f, runtime.PeakLifetime
}

// Stop ends the window: the counter deltas, clamped at zero, and the peak read
// through the window's own fd. It returns ErrNotFound when the cgroup is gone,
// which means the sandbox died during the window.
func (w *Window) Stop() (runtime.Usage, error) {
	u, err := w.read()
	if w.peak != nil {
		_ = w.peak.Close()
	}
	return u, err
}

func (w *Window) read() (runtime.Usage, error) {
	// A removed cgroup's open files fail with ENODEV; check the directory
	// first so the caller can tell a dead sandbox from a read error.
	if err := statCgroup(w.dir); err != nil {
		return runtime.Usage{}, err
	}
	u := runtime.Usage{
		CPUMillis:     delta(readKeyed(w.dir, "cpu.stat", "usage_usec"), w.cpu0) / 1000,
		OOMKills:      delta(readKeyed(w.dir, "memory.events", "oom_kill"), w.oom0),
		PidsLimitHits: delta(readKeyed(w.dir, "pids.events", "max"), w.pids0),
	}
	if w.peak != nil {
		if peak, ok := readPeak(w.peak); ok {
			u.MemoryPeak, u.PeakScope = peak, w.scope
		}
	}
	return u, nil
}

// readPeak reads from offset 0: the reset write moved the fd's offset, and
// reopening by path would read the lifetime peak instead of this fd's.
func readPeak(f *os.File) (int64, bool) {
	buf := make([]byte, 32)
	n, err := f.ReadAt(buf, 0)
	if err != nil && err != io.EOF {
		return 0, false
	}
	return parseInt(strings.TrimSpace(string(buf[:n]))), true
}

// delta clamps at zero: a counter that went down means the cgroup was
// recreated, not that the window used negative resources.
func delta(now, then int64) int64 {
	if now < then {
		return 0
	}
	return now - then
}

func statCgroup(dir string) error {
	if _, err := os.Stat(dir); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("%w: %s", ErrNotFound, dir)
		}
		return err
	}
	return nil
}
