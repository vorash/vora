//go:build integration

package integration

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/vorash/vora/internal/cgroup"
	"github.com/vorash/vora/internal/config"
	"github.com/vorash/vora/internal/runtime"
)

func newCgroupManager(t *testing.T) cgroup.Manager {
	t.Helper()
	m, err := cgroup.New(config.Config{SystemdMode: config.SystemdUser})
	if err != nil {
		t.Skipf("systemd user manager unavailable: %v", err)
	}
	if _, err := exec.LookPath("systemd-run"); err != nil {
		t.Skip("systemd-run not installed")
	}
	return m
}

func uniqueUnit(t *testing.T) string {
	t.Helper()
	return cgroup.UnitName("sbx_" + strconv.FormatInt(time.Now().UnixNano(), 36))
}

func readCgroupFile(t *testing.T, cgroupPath, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("/sys/fs/cgroup", cgroupPath, name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return strings.TrimSpace(string(data))
}

func cleanupUnit(t *testing.T, m cgroup.Manager, name string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	u, err := m.Attach(ctx, name)
	if err == nil {
		_ = u.Stop(ctx)
	}
	done := make(chan struct{})
	go func() {
		// Remove is what unloads failed units; retry briefly.
		if u != nil {
			_ = u.Remove(ctx)
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
	}
}

func TestSystemdUnitLimitsAndTeardown(t *testing.T) {
	m := newCgroupManager(t)
	name := uniqueUnit(t)
	defer cleanupUnit(t, m, name)
	ctx := context.Background()

	u, err := m.Start(ctx, cgroup.StartRequest{
		Name:   name,
		Limits: runtime.ResourceLimits{CPUMillis: 500, MemoryBytes: 64 << 20, Pids: 32},
		Argv:   []string{"/usr/bin/sleep", "60"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if u.CgroupPath() == "" {
		t.Fatal("no cgroup path resolved")
	}

	if got := readCgroupFile(t, u.CgroupPath(), "memory.max"); got != strconv.Itoa(64<<20) {
		t.Errorf("memory.max = %s, want %d", got, 64<<20)
	}
	if got := readCgroupFile(t, u.CgroupPath(), "pids.max"); got != "32" {
		t.Errorf("pids.max = %s, want 32", got)
	}
	if got := readCgroupFile(t, u.CgroupPath(), "cpu.max"); got != "50000 100000" {
		t.Errorf("cpu.max = %s, want 50000 100000", got)
	}

	active, err := u.Active(ctx)
	if err != nil || !active {
		t.Fatalf("unit not active: active=%v err=%v", active, err)
	}

	if err := u.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		active, _ := u.Active(ctx)
		if !active {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("unit did not stop")
}

func TestSystemdOOMKilled(t *testing.T) {
	m := newCgroupManager(t)
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not installed")
	}
	name := uniqueUnit(t)
	defer cleanupUnit(t, m, name)
	ctx := context.Background()

	u, err := m.Start(ctx, cgroup.StartRequest{
		Name:   name,
		Limits: runtime.ResourceLimits{CPUMillis: 1000, MemoryBytes: 64 << 20, Pids: 64},
		Argv:   []string{python, "-c", "x = bytearray(400 * 1024 * 1024)"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		active, _ := u.Active(ctx)
		if !active {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	result, err := u.Result(ctx)
	if err != nil {
		t.Fatalf("Result: %v", err)
	}
	if got := cgroup.ReasonFromSystemd(result, nil); got != runtime.ReasonOOMKilled {
		t.Fatalf("ReasonFromSystemd(%q) = %v, want OOMKilled", result, got)
	}
}

func TestSystemdPidsLimitEvents(t *testing.T) {
	m := newCgroupManager(t)
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not installed")
	}
	name := uniqueUnit(t)
	defer cleanupUnit(t, m, name)
	ctx := context.Background()

	u, err := m.Start(ctx, cgroup.StartRequest{
		Name:   name,
		Limits: runtime.ResourceLimits{CPUMillis: 1000, MemoryBytes: 256 << 20, Pids: 16},
		Argv:   []string{bash, "-c", "for i in $(seq 1 100); do sleep 30 & done; wait"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		st, err := u.Stats(ctx)
		if err == nil && st.PidsLimitHits > 0 {
			return // the cgroup recorded a pids.max event
		}
		active, _ := u.Active(ctx)
		if !active {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("pids limit was not observed")
}
