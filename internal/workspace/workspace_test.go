package workspace

import (
	"os"
	"testing"

	"github.com/vorash/vora/internal/runtime"
)

const testID = "sbx_01ARZ3NDEKTSV4RRFFQ69G5FAV"

func TestCreateIsIdempotentAndDestroy(t *testing.T) {
	m := NewManager(t.TempDir())
	spec := m.Paths(testID)
	spec.RootfsDir = "/cache/python/rootfs"

	got, err := m.Create(spec)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if got.RootfsDir != spec.RootfsDir {
		t.Fatalf("RootfsDir not preserved: %q", got.RootfsDir)
	}

	// Idempotent: creating twice must not error.
	if _, err := m.Create(spec); err != nil {
		t.Fatalf("second Create: %v", err)
	}

	for _, d := range []string{got.WorkspaceDir, got.HomeDir, got.IPCDir, m.ExecLogDir(testID)} {
		info, err := os.Stat(d)
		if err != nil || !info.IsDir() {
			t.Fatalf("expected dir %s: %v", d, err)
		}
	}

	if err := m.Destroy(testID); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if _, err := os.Stat(got.RootDir); !os.IsNotExist(err) {
		t.Fatalf("root dir still present after Destroy: %v", err)
	}
	// Destroy is idempotent.
	if err := m.Destroy(testID); err != nil {
		t.Fatalf("second Destroy: %v", err)
	}
}

func TestCreateRejectsEmptyRoot(t *testing.T) {
	m := NewManager(t.TempDir())
	if _, err := m.Create(runtime.WorkspaceSpec{}); err == nil {
		t.Fatal("Create with empty root should fail")
	}
}

// Sandbox directories hold workload data and the agent's control socket. They
// are only ever used by the runner's own UID, so other local users must not be
// able to traverse them, whatever the parent directory allows.
func TestCreateMakesPrivateDirectories(t *testing.T) {
	m := NewManager(t.TempDir())
	spec, err := m.Create(m.Paths(testID))
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{spec.RootDir, spec.WorkspaceDir, spec.HomeDir, spec.IPCDir, m.LogsDir(testID), m.ExecLogDir(testID)} {
		info, err := os.Stat(d)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != 0o700 {
			t.Errorf("%s mode = %04o, want 0700", d, got)
		}
	}
}
