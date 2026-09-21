package quota

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

type fakeRunner struct {
	calls  [][]string
	failOn string
}

func (f *fakeRunner) Run(_ context.Context, name string, args ...string) error {
	f.calls = append(f.calls, append([]string{name}, args...))
	if f.failOn != "" && name == f.failOn {
		return errors.New("boom")
	}
	return nil
}

func (f *fakeRunner) LookPath(name string) (string, error) { return "/usr/bin/" + name, nil }

func (f *fakeRunner) called(name string) bool {
	for _, c := range f.calls {
		if c[0] == name {
			return true
		}
	}
	return false
}

func hasCall(calls [][]string, want ...string) bool {
	for _, c := range calls {
		if len(c) != len(want) {
			continue
		}
		ok := true
		for i := range want {
			if c[i] != want[i] {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

func TestProjectIDStableAndNonZero(t *testing.T) {
	a := projectID("sbx_01H")
	b := projectID("sbx_01H")
	if a != b {
		t.Fatalf("project id is not stable: %d != %d", a, b)
	}
	if a == 0 {
		t.Fatal("project id must not be zero")
	}
	if projectID("sbx_02H") == a {
		t.Fatal("distinct ids produced the same project id")
	}
	if a > 0x7fffffff {
		t.Fatalf("project id %d exceeds 31 bits", a)
	}
}

func TestUsageSumsRegularFiles(t *testing.T) {
	root := t.TempDir()
	ws := filepath.Join(root, "workspace")
	home := filepath.Join(root, "home")
	if err := os.MkdirAll(filepath.Join(ws, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "a"), []byte("12345"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "sub", "b"), []byte("123"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "c"), []byte("12"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A symlink is not a regular file and must not be counted.
	if err := os.Symlink(filepath.Join(ws, "a"), filepath.Join(ws, "link")); err != nil {
		t.Fatal(err)
	}

	m := &Manager{root: root, run: &fakeRunner{}, mode: ModeAccounting}
	got, err := m.Usage([]string{ws, home})
	if err != nil {
		t.Fatal(err)
	}
	if got != 10 {
		t.Fatalf("Usage = %d, want 10", got)
	}
}

func TestEnforceInAccountingModeIsNoop(t *testing.T) {
	fr := &fakeRunner{}
	m := &Manager{root: t.TempDir(), run: fr, mode: ModeAccounting}
	if err := m.Enforce(context.Background(), "sbx_q", []string{"/tmp/x"}, 1<<20); err != nil {
		t.Fatal(err)
	}
	if len(fr.calls) != 0 {
		t.Fatalf("accounting mode ran commands: %v", fr.calls)
	}
}

func TestEnforceProjectModeIssuesCommands(t *testing.T) {
	root := t.TempDir()
	ws := filepath.Join(root, "workspace")
	home := filepath.Join(root, "home")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	fr := &fakeRunner{}
	m := &Manager{root: root, run: fr, mode: ModeProject}
	limit := int64(8 << 20)
	if err := m.Enforce(context.Background(), "sbx_q", []string{ws, home}, limit); err != nil {
		t.Fatalf("Enforce: %v", err)
	}
	proj := strconv.FormatUint(uint64(projectID("sbx_q")), 10)
	if !hasCall(fr.calls, "chattr", "-R", "-p", proj, ws) {
		t.Fatalf("missing chattr for workspace: %v", fr.calls)
	}
	if !hasCall(fr.calls, "chattr", "-R", "-p", proj, home) {
		t.Fatalf("missing chattr for home: %v", fr.calls)
	}
	blocks := strconv.FormatInt(limit/1024, 10)
	if !fr.called("setquota") {
		t.Fatalf("missing setquota call: %v", fr.calls)
	}
	found := false
	for _, c := range fr.calls {
		if c[0] == "setquota" && len(c) == 8 && c[2] == proj && c[3] == blocks && c[4] == blocks {
			found = true
		}
	}
	if !found {
		t.Fatalf("setquota call not as expected: %v", fr.calls)
	}
}

func TestReleaseProjectModeClears(t *testing.T) {
	root := t.TempDir()
	ws := filepath.Join(root, "workspace")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	fr := &fakeRunner{}
	m := &Manager{root: root, run: fr, mode: ModeProject}
	if err := m.Release(context.Background(), "sbx_q", []string{ws}); err != nil {
		t.Fatal(err)
	}
	if !hasCall(fr.calls, "chattr", "-R", "-p", "0", ws) {
		t.Fatalf("missing chattr clear: %v", fr.calls)
	}
	if !fr.called("setquota") {
		t.Fatalf("missing setquota clear: %v", fr.calls)
	}
}

func TestEnforceSurfacesCommandFailure(t *testing.T) {
	root := t.TempDir()
	ws := filepath.Join(root, "workspace")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	fr := &fakeRunner{failOn: "chattr"}
	m := &Manager{root: root, run: fr, mode: ModeProject}
	err := m.Enforce(context.Background(), "sbx_q", []string{ws}, 1<<20)
	if err == nil || !strings.Contains(err.Error(), "assign project") {
		t.Fatalf("want assign error, got %v", err)
	}
}

func TestDetectRequiresRoot(t *testing.T) {
	if got := detect(t.TempDir(), &fakeRunner{}, false); got != ModeAccounting {
		t.Fatalf("detect(non-root) = %q, want accounting", got)
	}
}
