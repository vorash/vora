package workspace

import (
	"os"
	"path/filepath"
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

// The workload can destroy its own sandbox, so Clean must leave the audit
// trail in place: dropping it would let the code under audit erase the record.
func TestCleanKeepsAuditLogs(t *testing.T) {
	m := NewManager(t.TempDir())
	spec, err := m.Create(m.Paths(testID))
	if err != nil {
		t.Fatal(err)
	}
	execRecord := m.ExecLogDir(testID) + "/exe_1.json"
	if err := os.WriteFile(execRecord, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := m.Clean(testID); err != nil {
		t.Fatalf("Clean: %v", err)
	}
	if _, err := os.Stat(execRecord); err != nil {
		t.Fatalf("audit record removed by Clean: %v", err)
	}
	for _, d := range []string{spec.WorkspaceDir, spec.HomeDir, spec.IPCDir} {
		if _, err := os.Stat(d); !os.IsNotExist(err) {
			t.Fatalf("%s should be removed by Clean: %v", d, err)
		}
	}
}

// A tombstone is the record plus its audit trail and nothing else: backend
// leftovers (a gVisor bundle/, a bound resolv.conf) are not audit data and
// must not accumulate for every sandbox ever destroyed.
func TestCleanLeavesOnlyTombstone(t *testing.T) {
	m := NewManager(t.TempDir())
	spec, err := m.Create(m.Paths(testID))
	if err != nil {
		t.Fatal(err)
	}
	keep := []string{"config.json", ".config.json.tmp-123"}
	drop := []string{"resolv.conf"}
	for _, f := range append(keep, drop...) {
		if err := os.WriteFile(filepath.Join(spec.RootDir, f), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(spec.RootDir, "bundle", "rootfs"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := m.Clean(testID); err != nil {
		t.Fatalf("Clean: %v", err)
	}
	for _, f := range append(keep, "logs") {
		if _, err := os.Stat(filepath.Join(spec.RootDir, f)); err != nil {
			t.Fatalf("tombstone lost %s: %v", f, err)
		}
	}
	for _, f := range append(drop, "bundle") {
		if _, err := os.Stat(filepath.Join(spec.RootDir, f)); !os.IsNotExist(err) {
			t.Fatalf("%s should be removed by Clean: %v", f, err)
		}
	}
}
