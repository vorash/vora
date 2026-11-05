package bwrap

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// launcherStub writes an executable that stands in for a program of the start
// chain and records exactly what it was given: /proc/$$/environ is the environ
// the kernel handed it, unaffected by what the shell adds to its own.
func launcherStub(t *testing.T, dir, name string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	script := "#!/bin/sh\n/bin/cat /proc/$$/environ > " + dir + "/environ\nprintf '%s\\n' \"$@\" > " + dir + "/args\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func readStubFile(t *testing.T, dir, name string) string {
	t.Helper()
	got, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatalf("the stub did not run: %v", err)
	}
	return string(got)
}

// stubEnvNames returns the names the stub was started with. Names only: a failing
// test must not print the values of whoever's environment it just caught.
func stubEnvNames(t *testing.T, dir string) []string {
	t.Helper()
	var names []string
	for _, entry := range strings.Split(readStubFile(t, dir, "environ"), "\x00") {
		if name, _, _ := strings.Cut(entry, "="); name != "" {
			names = append(names, name)
		}
	}
	return names
}

// The netns path starts bwrap from the runner, whose environment holds the audit
// DSN and the join token, and bwrap stays the sandbox's PID 1 with a readable
// /proc/1/environ (vorash/vora#126). This runs what startInNetns runs, up to a
// stub nsenter, because the isolation job has no CAP_NET_ADMIN to start the real
// thing; scripts/test-system-unit.sh does that on main.
func TestNetnsCommandEnv(t *testing.T) {
	t.Setenv("VORA_AUDIT_DSN", "fake-not-a-secret")
	dir := t.TempDir()
	launcherStub(t, dir, "nsenter")
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	b := &Backend{setprivBin: "/usr/bin/setpriv", bwrapBin: "/usr/bin/bwrap"}
	if err := b.netnsCommand(context.Background(), 4242, []string{"--unshare-user"}).Run(); err != nil {
		t.Fatalf("run: %v", err)
	}
	if names := stubEnvNames(t, dir); len(names) != 0 {
		t.Fatalf("the sandbox's start chain inherited an environment: %v", names)
	}
}

// Every process of a transient unit starts with the service manager's environment
// (vorash/vora#107), so the wrapper has to empty it before bwrap runs. It must also
// leave the tenant's --setenv values alone: TestBwrapEnvNotExpanded relies on them
// arriving byte for byte, and env would take a leading NAME=VALUE as an assignment.
func TestUnitArgvEnv(t *testing.T) {
	dir := t.TempDir()
	stub := launcherStub(t, dir, "bwrap")

	argv := unitArgv(stub, []string{"--setenv", "VORA_LEAK", "${HOME}", "--", "A=1"})
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "SSH_AUTH_SOCK=/fake/agent"}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("run: %v: %s", err, out)
	}

	if names := stubEnvNames(t, dir); len(names) != 0 {
		t.Fatalf("bwrap inherited the manager's environment: %v", names)
	}
	want := "--setenv\nVORA_LEAK\n${HOME}\n--\nA=1\n"
	if got := readStubFile(t, dir, "args"); got != want {
		t.Fatalf("args = %q, want %q", got, want)
	}
}
