package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// skipNonUnix skips tests that need a POSIX shell stub as a fake plugin.
func skipNonUnix(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("plugin dispatch tests use a POSIX shell stub")
	}
}

// fakePath writes executable fake plugins into a temp dir and returns a PATH
// value containing only that dir. The stubs use shell builtins only, so the
// restricted PATH does not hide anything they need — and it keeps the test from
// discovering real vora-* binaries.
func fakePath(t *testing.T, scripts map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range scripts {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
			t.Fatalf("write fake plugin %s: %v", name, err)
		}
	}
	return dir
}

func runCLI(t *testing.T, args []string, stdin string) (stdout, stderr string, code int) {
	t.Helper()
	var out, errBuf bytes.Buffer
	code = run(args, strings.NewReader(stdin), &out, &errBuf)
	return out.String(), errBuf.String(), code
}

// silenceStdout redirects os.Stdout for the duration of the test. Built-in
// commands print with fmt directly, so they bypass the writer passed to run.
func silenceStdout(t *testing.T) {
	t.Helper()
	devnull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = devnull
	t.Cleanup(func() {
		os.Stdout = old
		_ = devnull.Close()
	})
}

func TestValidPluginName(t *testing.T) {
	valid := []string{"probe", "probe-worker", "foo-bar", "a", "a1-b2-c3", strings.Repeat("a", 64)}
	for _, name := range valid {
		if !validPluginName(name) {
			t.Errorf("validPluginName(%q) = false, want true", name)
		}
	}
	invalid := []string{
		"", "../evil", "/abs", "-x", "Foo", "a b", "a/", "a_", "a.", "a--b", "a-",
		"1a", "..", strings.Repeat("a", 65),
	}
	for _, name := range invalid {
		if validPluginName(name) {
			t.Errorf("validPluginName(%q) = true, want false", name)
		}
	}
}

func TestDiscoverPlugins(t *testing.T) {
	skipNonUnix(t)
	t.Setenv("PATH", fakePath(t, map[string]string{
		"vora-probe":   "#!/bin/sh\ntrue\n",
		"vora-bbb":     "#!/bin/sh\ntrue\n",
		"vora-aaa":     "#!/bin/sh\ntrue\n",
		"vora-version": "#!/bin/sh\ntrue\n", // collides with a built-in
		"vora-Bad":     "#!/bin/sh\ntrue\n", // invalid name
		"vora-":        "#!/bin/sh\ntrue\n", // invalid name
	}))
	got := discoverPlugins(map[string]bool{"version": true})
	var names []string
	for _, p := range got {
		names = append(names, p.name)
	}
	want := []string{"aaa", "bbb", "probe"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Errorf("discoverPlugins() = %v, want %v", names, want)
	}
}

func TestPluginDispatchForwardsArgsAndStdin(t *testing.T) {
	skipNonUnix(t)
	t.Setenv("PATH", fakePath(t, map[string]string{
		"vora-probe": "#!/bin/sh\nprintf 'args:%s\\n' \"$*\"\nIFS= read -r line\nprintf 'stdin:%s\\n' \"$line\"\n",
	}))
	out, errOut, code := runCLI(t, []string{"probe", "replay", "/x", "--out", "."}, "hello\n")
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %q", code, errOut)
	}
	want := "args:replay /x --out .\nstdin:hello\n"
	if out != want {
		t.Errorf("stdout = %q, want %q", out, want)
	}
}

func TestPluginExitCodesPropagate(t *testing.T) {
	skipNonUnix(t)
	t.Setenv("PATH", fakePath(t, map[string]string{
		"vora-probe": "#!/bin/sh\nexit \"${EXIT_CODE:-0}\"\n",
	}))
	for _, want := range []int{0, 1, 42} {
		t.Setenv("EXIT_CODE", strconv.Itoa(want))
		if _, _, code := runCLI(t, []string{"probe"}, ""); code != want {
			t.Errorf("plugin exit %d: vora exit = %d", want, code)
		}
	}
}

func TestPluginKilledBySignalFails(t *testing.T) {
	skipNonUnix(t)
	t.Setenv("PATH", fakePath(t, map[string]string{
		"vora-probe": "#!/bin/sh\nkill -TERM $$\n",
	}))
	_, _, code := runCLI(t, []string{"probe"}, "")
	if code != 128+15 {
		t.Errorf("vora exit = %d, want %d (128+SIGTERM)", code, 128+15)
	}
}

func TestPluginHelpReachesPlugin(t *testing.T) {
	skipNonUnix(t)
	t.Setenv("PATH", fakePath(t, map[string]string{
		"vora-probe": "#!/bin/sh\nprintf '%s\\n' \"$*\"\n",
	}))
	out, errOut, code := runCLI(t, []string{"probe", "--help"}, "")
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %q", code, errOut)
	}
	if strings.TrimSpace(out) != "--help" {
		t.Errorf("plugin received %q, want %q", strings.TrimSpace(out), "--help")
	}
}

func TestBuiltinWinsOverPluginOfSameName(t *testing.T) {
	skipNonUnix(t)
	marker := filepath.Join(t.TempDir(), "plugin-ran")
	t.Setenv("PATH", fakePath(t, map[string]string{
		"vora-version": "#!/bin/sh\ntouch \"$MARKER\"\n",
	}))
	t.Setenv("MARKER", marker)

	silenceStdout(t) // the version built-in writes to os.Stdout directly
	_, errOut, code := runCLI(t, []string{"version"}, "")
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %q", code, errOut)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("a plugin shadowed the built-in 'version' command")
	}
}

func TestInvalidNameNeverExecs(t *testing.T) {
	skipNonUnix(t)
	marker := filepath.Join(t.TempDir(), "executed")
	stub := "#!/bin/sh\ntouch \"$MARKER\"\n"
	t.Setenv("PATH", fakePath(t, map[string]string{
		"vora-..":  stub,
		"vora-Foo": stub,
		"vora-a_b": stub,
	}))
	t.Setenv("MARKER", marker)

	for _, name := range []string{"..", "Foo", "a_b", "-x"} {
		_, errOut, code := runCLI(t, []string{name}, "")
		if code == 0 {
			t.Errorf("%q: exit = 0, want non-zero", name)
		}
		if !strings.Contains(errOut, "unknown") {
			t.Errorf("%q: stderr = %q, want an unknown-command error", name, errOut)
		}
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("invalid name reached exec")
	}
}

func TestPluginAfterDoubleDash(t *testing.T) {
	skipNonUnix(t)
	t.Setenv("PATH", fakePath(t, map[string]string{
		"vora-probe": "#!/bin/sh\nprintf '%s\\n' \"$*\"\n",
	}))
	out, errOut, code := runCLI(t, []string{"--", "probe", "x", "y"}, "")
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %q", code, errOut)
	}
	if strings.TrimSpace(out) != "x y" {
		t.Errorf("plugin received %q, want %q", strings.TrimSpace(out), "x y")
	}
}

func TestMissingPluginReportsClearError(t *testing.T) {
	skipNonUnix(t)
	t.Setenv("PATH", t.TempDir())
	_, errOut, code := runCLI(t, []string{"nosuchthing"}, "")
	if code != pluginExitNotFound {
		t.Errorf("exit = %d, want %d", code, pluginExitNotFound)
	}
	for _, want := range []string{
		"'nosuchthing' is not a vora command and no 'vora-nosuchthing' was found on PATH",
		"run 'vora --help' for the list of commands",
	} {
		if !strings.Contains(errOut, want) {
			t.Errorf("stderr = %q, want it to contain %q", errOut, want)
		}
	}
}

func TestPluginsDisabled(t *testing.T) {
	skipNonUnix(t)
	t.Setenv("PATH", fakePath(t, map[string]string{
		"vora-probe": "#!/bin/sh\necho RAN\n",
	}))
	t.Setenv(disablePluginsEnv, "1")

	out, errOut, code := runCLI(t, []string{"probe"}, "")
	if code == 0 {
		t.Errorf("exit = 0, want non-zero")
	}
	if strings.Contains(out, "RAN") {
		t.Errorf("plugin ran while disabled: %q", out)
	}
	if !strings.Contains(errOut, "plugins are disabled") {
		t.Errorf("stderr = %q, want a disabled notice", errOut)
	}

	help, _, _ := runCLI(t, []string{"--help"}, "")
	if strings.Contains(help, "External plugins:") {
		t.Errorf("help advertised plugins while disabled:\n%s", help)
	}
}

func TestHelpListsDiscoveredPlugins(t *testing.T) {
	skipNonUnix(t)
	t.Setenv("PATH", fakePath(t, map[string]string{
		"vora-probe": "#!/bin/sh\ntrue\n",
	}))
	out, errOut, code := runCLI(t, []string{"--help"}, "")
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %q", code, errOut)
	}
	if !strings.Contains(out, "External plugins:") {
		t.Errorf("help has no plugin section:\n%s", out)
	}
	if !strings.Contains(out, "probe") {
		t.Errorf("help does not list probe:\n%s", out)
	}
	if !strings.Contains(out, "Available Commands:") {
		t.Errorf("help lost the built-in section:\n%s", out)
	}
}

func TestHelpWithoutPlugins(t *testing.T) {
	skipNonUnix(t)
	t.Setenv("PATH", t.TempDir())
	out, errOut, code := runCLI(t, []string{"--help"}, "")
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %q", code, errOut)
	}
	if strings.Contains(out, "External plugins:") {
		t.Errorf("help shows an empty plugin section:\n%s", out)
	}
	if !strings.Contains(out, "Available Commands:") {
		t.Errorf("help is missing the built-in section:\n%s", out)
	}
}
