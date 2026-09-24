package cgroup

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/vorash/vora/internal/runtime"
)

func TestWriteLimitsFailsIfSwapCannotBeDisabled(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "memory.swap.max"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeLimits(dir, runtime.ResourceLimits{MemoryBytes: 64 << 20, Pids: 4, CPUMillis: 1000}); err == nil {
		t.Fatal("cgroup started without a swap cap after memory.swap.max failed")
	}
}

func TestCPUMax(t *testing.T) {
	if got := cpuMax(1000); got != "100000 100000" {
		t.Fatalf("cpuMax(1000) = %q", got)
	}
	if got := cpuMax(500); got != "50000 100000" {
		t.Fatalf("cpuMax(500) = %q", got)
	}
}

func TestSubtreeCreateWritesLimits(t *testing.T) {
	root := t.TempDir()
	s := OpenSubtree(root, "")
	g, err := s.Create(context.Background(), "sbx_lim", runtime.ResourceLimits{
		CPUMillis:   1000,
		MemoryBytes: 64 << 20,
		Pids:        16,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	dir := g.Path()
	read := func(name string) string {
		t.Helper()
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		return string(b)
	}
	if got := read("memory.max"); got != "67108864\n" {
		t.Errorf("memory.max = %q", got)
	}
	if got := read("memory.swap.max"); got != "0\n" {
		t.Errorf("memory.swap.max = %q", got)
	}
	if got := read("pids.max"); got != "16\n" {
		t.Errorf("pids.max = %q", got)
	}
	if got := read("cpu.max"); got != "100000 100000\n" {
		t.Errorf("cpu.max = %q", got)
	}

	ids, err := s.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(ids) != 1 || ids[0] != "sbx_lim" {
		t.Fatalf("List = %v", ids)
	}

	if err := os.WriteFile(filepath.Join(dir, "memory.current"), []byte("123\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "memory.peak"), []byte("456\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pids.current"), []byte("2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "memory.events"), []byte("oom_kill 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pids.events"), []byte("max 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cpu.stat"), []byte("usage_usec 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := g.Stats()
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if st.MemoryBytes != 123 || st.MemoryPeak != 456 || st.Pids != 2 {
		t.Fatalf("stats = %+v", st)
	}

	if err := g.Destroy(); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("cgroup dir still present: %v", err)
	}
}

func TestSubtreeAttachMissing(t *testing.T) {
	s := OpenSubtree(t.TempDir(), "")
	if _, err := s.Attach(context.Background(), "sbx_nope"); err == nil {
		t.Fatal("expected ErrNotFound")
	}
}
