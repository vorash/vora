package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/vorash/vora/internal/config"
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

// At boot the system runner can start before the vora user's rootless
// containerd. The resolver would then run without an image store until a
// restart, so containerd mode must refuse to start instead and let systemd
// retry; auto and cache modes keep their fallback.
func TestRequireImageStore(t *testing.T) {
	up := func(string) bool { return true }
	down := func(string) bool { return false }
	for _, tc := range []struct {
		name      string
		mode      string
		available func(string) bool
		wantErr   bool
	}{
		{"containerd mode, socket missing", config.ImagesContainerd, down, true},
		{"containerd mode, socket present", config.ImagesContainerd, up, false},
		{"auto mode falls back", config.ImagesAuto, down, false},
		{"cache mode needs no socket", config.ImagesCache, down, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := requireImageStore(tc.mode, "/run/user/997/containerd/containerd.sock", tc.available)
			if (err != nil) != tc.wantErr {
				t.Fatalf("error = %v, want error = %v", err, tc.wantErr)
			}
		})
	}
}
