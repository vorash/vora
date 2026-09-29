package main

import (
	"os"
	"path/filepath"
	"testing"
)

// A state directory created by an older release (or the default umask) is
// 0755; starting the runner must tighten it, not just create new ones private.
func TestPrivateDirTightensExistingDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "sandboxes")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := ensurePrivateDir(dir); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o700 {
		t.Fatalf("mode = %04o, want 0700", got)
	}
}

// A non-root runner started by the system unit has no XDG_RUNTIME_DIR, and
// the unit cannot supply the right one. Without it systemd-run --user and the
// rootless containerd socket are unreachable and no sandbox starts.
func TestDefaultRuntimeDirForTheSystemUnit(t *testing.T) {
	for _, tc := range []struct {
		name    string
		current string
		euid    int
		want    string
	}{
		{"vora user under the system unit", "", 997, "/run/user/997"},
		{"user session keeps its own", "/run/user/1000", 1000, ""},
		{"root runner has none", "", 0, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := defaultRuntimeDir(tc.current, tc.euid); got != tc.want {
				t.Fatalf("defaultRuntimeDir(%q, %d) = %q, want %q", tc.current, tc.euid, got, tc.want)
			}
		})
	}
}
