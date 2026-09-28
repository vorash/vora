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
