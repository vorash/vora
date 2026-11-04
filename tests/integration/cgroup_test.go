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

func cleanupUnit(t *testing.T, _ cgroup.Manager, name string) {
	t.Helper()
	// A unit can become failed only after the first reset-failed, so retry
	// stop + reset-failed until systemd no longer knows the unit.
	deadline := time.Now().Add(12 * time.Second)
	for {
		_ = exec.Command("systemctl", "--user", "stop", name).Run()
		_ = exec.Command("systemctl", "--user", "reset-failed", name).Run()
		if !unitPresent(name) || time.Now().After(deadline) {
			return
		}
		time.Sleep(300 * time.Millisecond)
	}
}

// unitPresent reports whether systemd still lists the unit.
func unitPresent(name string) bool {
	out, err := exec.Command("systemctl", "--user", "list-units", "--type=service",
		"--all", "--no-legend", "--plain", "--no-pager", name).Output()
	return err == nil && strings.TrimSpace(string(out)) != ""
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

// An OOM kill must take only the process the kernel chose, not the unit. With
// systemd's default OOMPolicy=stop the unit was stopped and its cgroup
// removed, so a sandbox died with its exec and lost its counters
// (vorash/vora#108). systemd records no Result=oom-kill under
// OOMPolicy=continue; memory.events is where the kill shows.
func TestSystemdOOMKeepsUnit(t *testing.T) {
	m := newCgroupManager(t)
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not installed")
	}
	name := uniqueUnit(t)
	defer cleanupUnit(t, m, name)
	ctx := context.Background()

	const limit = 64 << 20
	u, err := m.Start(ctx, cgroup.StartRequest{
		Name:   name,
		Limits: runtime.ResourceLimits{CPUMillis: 1000, MemoryBytes: limit, Pids: 64},
		// The shell stands in for the sandbox's init: it outlives the hog.
		Argv: []string{"/bin/sh", "-c", python + ` -c "x = bytearray(400 * 1024 * 1024)"; exec sleep 30`},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	var st *runtime.Stats
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if st, err = u.Stats(ctx); err == nil && st.OOMKills > 0 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if st == nil || st.OOMKills == 0 {
		t.Fatalf("no OOM kill counted (stats %+v, err %v)", st, err)
	}
	// systemd acts on the OOM event asynchronously; give a stop time to land.
	time.Sleep(time.Second)
	if active, _ := u.Active(ctx); !active {
		result, _ := u.Result(ctx)
		t.Fatalf("unit stopped after the OOM kill (result %q); only the hog should have died", result)
	}
	if st.MemoryPeak < limit*8/10 {
		t.Fatalf("memory peak = %d, want near the %d limit", st.MemoryPeak, limit)
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
