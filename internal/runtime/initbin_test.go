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
	entries, err := os.ReadDir(filepath.Dir(dst))
	if err != nil {
		t.Fatalf("read ipc dir: %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("ipc dir holds %d entries, want only the binary: temp file left behind", len(entries))
	}
}

// Under bwrap the workload can write the ipc directory, and a reinstall runs on
// the host with the runner's privileges. A symlink the workload left at a
// predictable temp name must not make the install overwrite a host file, or put
// a link in the binary's place that the read-only bind would then follow.
func TestInstallInitBinaryIgnoresPlantedTemp(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src-init")
	if err := os.WriteFile(src, []byte("real init"), 0o755); err != nil {
		t.Fatalf("write source: %v", err)
	}
	hostFile := filepath.Join(dir, "host-file")
	if err := os.WriteFile(hostFile, []byte("host data"), 0o644); err != nil {
		t.Fatalf("write host file: %v", err)
	}
	ipc := filepath.Join(dir, "ipc")
	if err := os.MkdirAll(ipc, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	dst := filepath.Join(ipc, "vora-init")
	if err := os.Symlink(hostFile, dst+".tmp"); err != nil {
		t.Fatalf("plant symlink: %v", err)
	}

	if err := InstallInitBinary(src, dst); err != nil {
		t.Fatalf("InstallInitBinary: %v", err)
	}

	got, err := os.ReadFile(hostFile)
	if err != nil {
		t.Fatalf("read host file: %v", err)
	}
	if string(got) != "host data" {
		t.Errorf("host file = %q: the install wrote through the planted symlink", got)
	}
	info, err := os.Lstat(dst)
	if err != nil {
		t.Fatalf("lstat dst: %v", err)
	}
	if !info.Mode().IsRegular() {
		t.Errorf("dst mode = %v, want a regular file", info.Mode())
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
