package runtime

import (
	"os"
	"path/filepath"
	"testing"
)

func TestInstallInitBinary(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src-init")
	if err := os.WriteFile(src, []byte("#!/bin/sh\ntrue\n"), 0o755); err != nil {
		t.Fatalf("write source: %v", err)
	}
	dst := filepath.Join(dir, "ipc", "vora-init")
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// A stale binary must be replaced, not appended to.
	if err := os.WriteFile(dst, []byte("stale"), 0o644); err != nil {
		t.Fatalf("write stale: %v", err)
	}

	if err := InstallInitBinary(src, dst); err != nil {
		t.Fatalf("InstallInitBinary: %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read dst: %v", err)
	}
	if string(got) != "#!/bin/sh\ntrue\n" {
		t.Errorf("dst = %q, want the source contents", got)
	}
	info, err := os.Stat(dst)
	if err != nil {
		t.Fatalf("stat dst: %v", err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Errorf("dst mode = %v, want executable", info.Mode())
	}
	if _, err := os.Stat(dst + ".tmp"); !os.IsNotExist(err) {
		t.Error("temp file left behind")
	}
}

func TestResolveInitBinaryPrefersEnv(t *testing.T) {
	t.Setenv("VORA_INIT_BINARY", "/custom/vora-init")
	got, err := ResolveInitBinary()
	if err != nil {
		t.Fatalf("ResolveInitBinary: %v", err)
	}
	if got != "/custom/vora-init" {
		t.Errorf("ResolveInitBinary() = %q, want the env override", got)
	}
}
