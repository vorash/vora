package snapshot

import (
	"errors"
	"testing"

	"github.com/vorash/vora/internal/runtime"
)

func TestStoreRoundTrip(t *testing.T) {
	s := NewStore(t.TempDir())
	rec := &Record{
		ID:        "snap_01JTEST",
		SandboxID: "sbx_01JTEST",
		TenantID:  "t1",
		Spec:      runtime.SandboxSpec{ID: "sbx_01JTEST", Image: "vora/python"},
		Bytes:     42,
	}
	if err := s.Create(rec); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := s.Create(rec); err == nil {
		t.Fatal("create: accepted a duplicate snapshot id")
	}

	got, err := s.Load(rec.ID)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got.SandboxID != rec.SandboxID || got.TenantID != "t1" || got.Spec.Image != "vora/python" {
		t.Fatalf("load: got %+v", got)
	}

	list, err := s.List()
	if err != nil || len(list) != 1 {
		t.Fatalf("list: %v entries, err %v", len(list), err)
	}

	if err := s.Delete(rec.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := s.Load(rec.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("load after delete: %v, want ErrNotFound", err)
	}
}

func TestStoreRejectsEscapingIDs(t *testing.T) {
	s := NewStore(t.TempDir())
	for _, id := range []string{"", "../evil", `..\evil`, ".", ".."} {
		if err := s.Create(&Record{ID: id}); err == nil {
			t.Fatalf("create(%q): accepted an id that could escape the store root", id)
		}
	}
}
