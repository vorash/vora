package cgroup

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/vorash/vora/internal/runtime"
)

// fakeCgroup writes the files a window reads into a fresh directory. Regular
// files stand in for cgroup files; the per-fd reset is simulated by rewriting
// the inode the window holds and replacing the path with another inode.
func fakeCgroup(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		writeFile(t, filepath.Join(dir, name), content)
	}
	return dir
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func baseCgroup(t *testing.T) string {
	return fakeCgroup(t, map[string]string{
		"cpu.stat":      "usage_usec 5000000\nuser_usec 3000000\nsystem_usec 2000000\n",
		"memory.events": "low 0\nhigh 0\nmax 4\noom 2\noom_kill 2\noom_group_kill 0\n",
		"pids.events":   "max 3\n",
		"memory.peak":   "8388608\n",
	})
}

// An exec record must describe the exec, not the sandbox's history: counters
// are differences from the window's start, and a counter that went backwards
// (a recreated cgroup) must not report negative usage.
func TestWindowCountsDeltasClampedAtZero(t *testing.T) {
	dir := baseCgroup(t)
	w, err := OpenWindow(dir)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "cpu.stat"), "usage_usec 7500000\n")
	writeFile(t, filepath.Join(dir, "memory.events"), "oom_kill 3\n")
	writeFile(t, filepath.Join(dir, "pids.events"), "max 1\n")

	u, err := w.Stop()
	if err != nil {
		t.Fatal(err)
	}
	if u.CPUMillis != 2500 || u.OOMKills != 1 || u.PidsLimitHits != 0 {
		t.Fatalf("usage = %+v, want cpu 2500 ms, 1 oom kill, 0 pids hits (clamped from -2)", u)
	}
}

// On a kernel that accepts the reset, the peak must come from the fd that was
// reset. Any other read of memory.peak (a reopen by path) returns the
// lifetime peak, which would pin every later exec to the sandbox's worst one.
func TestWindowReadsPeakThroughItsOwnFD(t *testing.T) {
	dir := baseCgroup(t)
	peakPath := filepath.Join(dir, "memory.peak")
	w, err := OpenWindow(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(peakPath); string(got[:5]) != "reset" {
		t.Fatalf("memory.peak = %q, want the window to have written a reset", got)
	}
	// The held fd now sees the window's peak (same inode)...
	writeFile(t, peakPath, "4096\n")
	// ...and the path a reopen would find holds the lifetime peak.
	if err := os.Remove(peakPath); err != nil {
		t.Fatal(err)
	}
	writeFile(t, peakPath, "999999999\n")

	u, err := w.Stop()
	if err != nil {
		t.Fatal(err)
	}
	if u.PeakScope != runtime.PeakWindow || u.MemoryPeak != 4096 {
		t.Fatalf("peak = %d scope %d, want 4096 from the window's fd with scope WINDOW", u.MemoryPeak, u.PeakScope)
	}
}

// Before kernel 6.12 memory.peak is 0444 and cannot be reset. The window must
// say its peak is the lifetime one instead of passing it off as the exec's.
func TestWindowReadOnlyPeakIsLifetime(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root opens a 0444 file for writing")
	}
	dir := baseCgroup(t)
	peakPath := filepath.Join(dir, "memory.peak")
	if err := os.Chmod(peakPath, 0o444); err != nil {
		t.Fatal(err)
	}
	w, err := OpenWindow(dir)
	if err != nil {
		t.Fatal(err)
	}
	u, err := w.Stop()
	if err != nil {
		t.Fatal(err)
	}
	if u.PeakScope != runtime.PeakLifetime || u.MemoryPeak != 8388608 {
		t.Fatalf("peak = %d scope %d, want the lifetime 8388608 with scope LIFETIME", u.MemoryPeak, u.PeakScope)
	}
}

// Without memory.peak there is no peak to report, and the scope must not
// claim one.
func TestWindowWithoutPeakFileIsUnspecified(t *testing.T) {
	dir := baseCgroup(t)
	if err := os.Remove(filepath.Join(dir, "memory.peak")); err != nil {
		t.Fatal(err)
	}
	w, err := OpenWindow(dir)
	if err != nil {
		t.Fatal(err)
	}
	u, err := w.Stop()
	if err != nil {
		t.Fatal(err)
	}
	if u.PeakScope != runtime.PeakUnspecified || u.MemoryPeak != 0 {
		t.Fatalf("usage = %+v, want no peak and scope UNSPECIFIED", u)
	}
}

// A sandbox that died during the window takes its cgroup with it. Stop must
// say so with ErrNotFound, so the backend can fall back to what systemd
// retained about the death instead of reporting zero usage as fact.
func TestWindowStopAfterCgroupRemoved(t *testing.T) {
	dir := baseCgroup(t)
	w, err := OpenWindow(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Stop(); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Stop after removal = %v, want ErrNotFound", err)
	}
}

func TestOpenWindowMissingCgroup(t *testing.T) {
	if _, err := OpenWindow(filepath.Join(t.TempDir(), "gone")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("OpenWindow = %v, want ErrNotFound", err)
	}
}
