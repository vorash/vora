//go:build security

package security

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/vorash/vora/internal/runtime"
	"github.com/vorash/vora/internal/runtime/bwrap"
)

// setManagerEnv puts a variable in the user service manager's environment, the
// environment a NetworkNone sandbox's unit starts with. Callers pass a test-only
// name and never an ambient one like SSH_AUTH_SOCK: the manager outlives the test
// and serves the rest of the user's session.
func setManagerEnv(t *testing.T, name, value string) {
	t.Helper()
	if out, err := exec.Command("systemctl", "--user", "set-environment", name+"="+value).CombinedOutput(); err != nil {
		t.Fatalf("systemctl --user set-environment: %v: %s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("systemctl", "--user", "unset-environment", name).Run() })
}

// procEnviron reads a process's environment from inside the sandbox. A file the
// workload cannot open holds nothing it can steal, so it reads as empty: that is
// a non-dumpable process (EACCES) or a pid the backend does not have (ENOENT).
func (f *fixture) procEnviron(h runtime.Handle, pid string) string {
	f.t.Helper()
	out, errOut, code := f.exec(h, "cat", "/proc/"+pid+"/environ")
	if code != 0 {
		if strings.Contains(errOut, "Permission denied") || strings.Contains(errOut, "No such file") {
			return ""
		}
		f.t.Fatalf("read /proc/%s/environ: exit %d: %s", pid, code, errOut)
	}
	return out
}

// envNames returns the variable names in a /proc/<pid>/environ dump, so a failure
// can name what leaked without logging a value.
func envNames(environ string) []string {
	var names []string
	for _, entry := range strings.Split(environ, "\x00") {
		if name, _, _ := strings.Cut(entry, "="); name != "" {
			names = append(names, name)
		}
	}
	return names
}

// TestEnvIsAllowlisted asserts what the sandbox's first processes and a workload
// receive is an allowlist, not "the host's environment minus what we thought of".
//
// Why it matters: the user manager's environment reached every process of a
// NetworkNone sandbox (vorash/vora#107), and anything added to it later, by
// `systemctl --user set-environment` or environment.d, would have followed. bwrap
// stays PID 1 and a workload of the same uid can read its /proc/1/environ, so
// bwrap itself has to start clean: --clearenv would only clean its child.
func TestEnvIsAllowlisted(t *testing.T) {
	forEachBackend(t, func(t *testing.T, f *fixture) {
		if _, ok := f.backend.(*bwrap.Backend); !ok {
			t.Skip("gVisor takes its process environment from the OCI spec, not the user manager")
		}
		setManagerEnv(t, "VORA_SECURITY_MANAGER_MARKER", "fake")
		h := f.create("sbx_env", limits(128, 64))

		// Reading the wrong process, or none, would pass the checks below.
		for pid, want := range map[string]string{"1": "bwrap", "2": "vora-init"} {
			if cmdline, _, _ := f.exec(h, "cat", "/proc/"+pid+"/cmdline"); !strings.Contains(cmdline, want) {
				t.Fatalf("/proc/%s is %q, want %s: this test reads the wrong process", pid, cmdline, want)
			}
		}
		if _, errOut, code := f.exec(h, "cat", "/proc/1/environ"); code != 0 {
			t.Fatalf("cannot read /proc/1/environ, so this test proves nothing: %s", errOut)
		}

		// bwrap adds PWD for --chdir, and HOME and PATH come from its --setenv.
		// /proc/1/environ keeps what bwrap was started with, not those.
		workload := map[string]bool{"HOME": true, "PATH": true, "PWD": true}
		for pid, allowed := range map[string]map[string]bool{"1": {}, "2": workload, "self": workload} {
			for _, name := range envNames(f.procEnviron(h, pid)) {
				if !allowed[name] {
					t.Errorf("/proc/%s/environ holds %s, which is not on its allowlist", pid, name)
				}
			}
		}
	})
}
