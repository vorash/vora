package cgroup

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/vorash/vora/internal/config"
	"github.com/vorash/vora/internal/runtime"
)

func TestUnitName(t *testing.T) {
	if got := UnitName("sbx_abc"); got != "vora-sbx_abc" {
		t.Fatalf("UnitName = %q", got)
	}
}

func TestCPUQuota(t *testing.T) {
	cases := map[int64]string{
		1000: "100%",
		500:  "50%",
		2000: "200%",
		1500: "150%",
		100:  "10%",
	}
	for millis, want := range cases {
		if got := cpuQuota(millis); got != want {
			t.Errorf("cpuQuota(%d) = %q, want %q", millis, got, want)
		}
	}
}

func TestReadStats(t *testing.T) {
	dir := t.TempDir()
	old := cgroupRoot
	cgroupRoot = dir
	defer func() { cgroupRoot = old }()

	cg := filepath.Join(dir, "sandbox")
	if err := os.MkdirAll(cg, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name, content string) {
		if err := os.WriteFile(filepath.Join(cg, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("memory.current", "1048576\n")
	write("memory.peak", "2097152\n")
	write("pids.current", "3\n")
	write("memory.events", "low 0\nhigh 1\nmax 2\noom 5\noom_kill 2\noom_group_kill 1\n")
	write("pids.events", "max 4\n")
	write("cpu.stat", "usage_usec 2500000\nuser_usec 1000000\nsystem_usec 1500000\n")

	st, err := readStats("sandbox")
	if err != nil {
		t.Fatalf("readStats: %v", err)
	}
	if st.MemoryBytes != 1048576 || st.MemoryPeak != 2097152 {
		t.Errorf("memory = %+v", st)
	}
	if st.Pids != 3 {
		t.Errorf("pids = %d, want 3", st.Pids)
	}
	if st.OOMKills != 2 {
		t.Errorf("oom kills = %d, want 2", st.OOMKills)
	}
	if st.PidsLimitHits != 4 {
		t.Errorf("pids limit hits = %d, want 4", st.PidsLimitHits)
	}
	if st.CPUMillis != 2500 {
		t.Errorf("cpu millis = %d, want 2500", st.CPUMillis)
	}
}

func TestReadStatsMissingCgroup(t *testing.T) {
	dir := t.TempDir()
	old := cgroupRoot
	cgroupRoot = dir
	defer func() { cgroupRoot = old }()

	if _, err := readStats("nope"); err != ErrNotFound {
		t.Fatalf("readStats error = %v, want ErrNotFound", err)
	}
}

func TestReason(t *testing.T) {
	cases := []struct {
		name string
		st   *runtime.Stats
		want runtime.ExitReason
	}{
		{"normal", &runtime.Stats{}, runtime.ReasonExited},
		{"oom", &runtime.Stats{OOMKills: 1}, runtime.ReasonOOMKilled},
		{"pids", &runtime.Stats{PidsLimitHits: 1}, runtime.ReasonPidsLimit},
		{"oom wins over pids", &runtime.Stats{OOMKills: 1, PidsLimitHits: 1}, runtime.ReasonOOMKilled},
		{"nil", nil, runtime.ReasonRuntimeFailure},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Reason(c.st); got != c.want {
				t.Fatalf("Reason = %v, want %v", got, c.want)
			}
		})
	}
}

func TestReasonFromSystemd(t *testing.T) {
	cases := []struct {
		result string
		st     *runtime.Stats
		want   runtime.ExitReason
	}{
		{"oom-kill", nil, runtime.ReasonOOMKilled},
		{"timeout", nil, runtime.ReasonTimeout},
		{"resources", nil, runtime.ReasonPolicyViolation},
		{"watchdog", nil, runtime.ReasonRuntimeFailure},
		{"success", nil, runtime.ReasonExited},
		{"exit-code", &runtime.Stats{}, runtime.ReasonExited},
		{"exit-code", &runtime.Stats{PidsLimitHits: 1}, runtime.ReasonPidsLimit},
		{"signal", &runtime.Stats{OOMKills: 1}, runtime.ReasonOOMKilled},
	}
	for _, c := range cases {
		if got := ReasonFromSystemd(c.result, c.st); got != c.want {
			t.Errorf("ReasonFromSystemd(%q) = %v, want %v", c.result, got, c.want)
		}
	}
}

// systemd expands ${VAR} in the arguments of a transient service, and the
// sandbox's tenant-controlled env values travel as arguments. Without
// --expand-environment=no a tenant can read the service manager's environment.
func TestSystemdRunArgsDisableEnvironmentExpansion(t *testing.T) {
	s := &systemd{mode: config.SystemdUser}
	args := s.runArgs(StartRequest{Name: "vora-sbx_x", Argv: []string{"bwrap", "--setenv", "X", "${HOME}"}})

	sep := slices.Index(args, "--")
	if sep < 0 {
		t.Fatalf("no -- separator in %v", args)
	}
	if !slices.Contains(args[:sep], "--expand-environment=no") {
		t.Fatalf("systemd-run flags %v lack --expand-environment=no", args[:sep])
	}
	if got := args[sep+1:]; !slices.Equal(got, []string{"bwrap", "--setenv", "X", "${HOME}"}) {
		t.Fatalf("argv was altered: %v", got)
	}
}
