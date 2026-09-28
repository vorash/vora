package sandbox

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/vorash/vora/internal/runtime"
	"github.com/vorash/vora/internal/ulid"
)

func testRecord(id string) *Record {
	return &Record{
		ID:    id,
		State: StateRequested,
		Spec: runtime.SandboxSpec{
			ID:    id,
			Image: "vora/base",
			Resources: runtime.ResourceLimits{
				CPUMillis:   1000,
				MemoryBytes: 512 << 20,
				Pids:        128,
			},
			Network: runtime.NetworkPolicy{Mode: runtime.NetworkNone},
			TTL:     time.Minute,
		},
	}
}

func TestStoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)

	rec := testRecord("sbx_01ARZ3NDEKTSV4RRFFQ69G5FAV")
	if err := s.Create(rec); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := s.Load(rec.ID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.State != StateRequested || got.Spec.Image != "vora/base" {
		t.Fatalf("unexpected record: %+v", got)
	}
	if got.CreatedAt.IsZero() || got.UpdatedAt.IsZero() {
		t.Fatal("timestamps not set on create")
	}

	list, err := s.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 || list[0].ID != rec.ID {
		t.Fatalf("List = %+v", list)
	}

	if err := s.Delete(rec.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := s.Load(rec.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Load after delete = %v, want ErrNotFound", err)
	}
}

func TestStoreRejectsDuplicateAndBadID(t *testing.T) {
	s := NewStore(t.TempDir())
	id := ulid.NewSandbox()
	if err := s.Create(testRecord(id)); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := s.Create(testRecord(id)); err == nil {
		t.Fatal("duplicate Create should fail")
	}
	for _, id := range []string{"", "../escape", "a/b", ".hidden", "-flag", "a b", "x.slice", "sbx_a"} {
		if err := s.Create(testRecord(id)); err == nil {
			t.Errorf("Create(%q) should fail", id)
		}
	}
}

func TestSaveIsAtomicAndLeavesNoTempFiles(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	rec := testRecord(ulid.NewSandbox())
	if err := s.Create(rec); err != nil {
		t.Fatalf("Create: %v", err)
	}
	rec.State = StateCreating
	if err := s.Save(rec); err != nil {
		t.Fatalf("Save: %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(dir, rec.ID))
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if e.Name() != configFile {
			t.Fatalf("unexpected file left in sandbox dir: %s", e.Name())
		}
	}
	got, _ := s.Load(rec.ID)
	if got.State != StateCreating {
		t.Fatalf("state = %s, want CREATING", got.State)
	}
}

func TestCanTransition(t *testing.T) {
	allowed := [][2]State{
		{StateRequested, StateCreating},
		{StateCreating, StateStarting},
		{StateStarting, StateRunning},
		{StateRunning, StateStopping},
		{StateRunning, StateExpired},
		{StateStopping, StateStopped},
		{StateStopped, StateDestroyed},
		{StateRunning, StateDestroyed},
	}
	for _, a := range allowed {
		if !CanTransition(a[0], a[1]) {
			t.Errorf("expected %s → %s allowed", a[0], a[1])
		}
	}
	denied := [][2]State{
		{StateRequested, StateRunning},
		{StateRunning, StateCreating},
		{StateDestroyed, StateRunning},
		{StateStopped, StateRunning},
		{StateExpired, StateRunning},
	}
	for _, d := range denied {
		if CanTransition(d[0], d[1]) {
			t.Errorf("expected %s → %s denied", d[0], d[1])
		}
	}
}

func TestTransitionAppliesReason(t *testing.T) {
	rec := testRecord("sbx_t")
	rec.State = StateRunning
	if err := Transition(rec, StateExpired, runtime.ReasonTimeout); err != nil {
		t.Fatalf("Transition: %v", err)
	}
	if rec.State != StateExpired || rec.Reason != runtime.ReasonTimeout {
		t.Fatalf("unexpected record: %+v", rec)
	}
	if err := Transition(rec, StateRunning, ""); err == nil {
		t.Fatal("illegal transition from terminal state should fail")
	}
}

func TestStoreCreatesPrivateSandboxDirectory(t *testing.T) {
	store := NewStore(t.TempDir())
	id := ulid.NewSandbox()
	if err := store.Create(&Record{ID: id, State: StateRequested}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(store.Dir(id))
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o700 {
		t.Fatalf("sandbox dir mode = %04o, want 0700", got)
	}
}
