package sandbox

import (
	"errors"
	"fmt"
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

// Records on disk outlive the binary that wrote them, and they were written two
// ways before a measured zero could be told from a missing value.
func TestOldExecRecordsReadBack(t *testing.T) {
	const (
		beforeUsage = `{"execution_id":"exe_01KA0000000000000000000000","sandbox_id":"sbx_a","command":"/bin/true","started_at":"2026-01-01T00:00:00Z","finished_at":"2026-01-01T00:00:01Z","exit_code":0,"reason":"Exited","stdout_bytes":0,"stderr_bytes":0}`
		zeroOmitted = `{"execution_id":"exe_01KB0000000000000000000000","sandbox_id":"sbx_a","command":"/bin/true","started_at":"2026-01-02T00:00:00Z","finished_at":"2026-01-02T00:00:01Z","exit_code":0,"reason":"Exited","stdout_bytes":0,"stderr_bytes":0,"cpu_millis":120,"memory_peak_bytes":4096,"memory_peak_scope":1,"egress_denied":2}`
		noScope     = `{"execution_id":"exe_01KC0000000000000000000000","sandbox_id":"sbx_a","command":"/bin/true","started_at":"2026-01-03T00:00:00Z","finished_at":"2026-01-03T00:00:01Z","exit_code":0,"reason":"Exited","stdout_bytes":0,"stderr_bytes":0,"memory_peak_bytes":8192}`
	)
	dir := t.TempDir()
	for i, body := range []string{beforeUsage, zeroOmitted, noScope} {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("exe_%d.json", i)), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	recs, err := ReadExecRecords(dir)
	if err != nil || len(recs) != 3 {
		t.Fatalf("ReadExecRecords = %v, %v", recs, err)
	}

	first := recs[0]
	if first.CPUMillis != nil || first.MemoryPeakBytes != nil || first.OOMKills != nil || first.PidsLimitHits != nil ||
		first.EgressAllowed != nil || first.EgressDenied != nil || first.MemoryPeakScope != runtime.PeakUnspecified {
		t.Errorf("a record from before usage reads as measured: %+v", first)
	}

	second := recs[1]
	if second.CPUMillis == nil || *second.CPUMillis != 120 || second.MemoryPeakBytes == nil || *second.MemoryPeakBytes != 4096 ||
		second.MemoryPeakScope != runtime.PeakWindow || second.EgressDenied == nil || *second.EgressDenied != 2 {
		t.Errorf("an old record lost what it did hold: %+v", second)
	}
	if second.OOMKills != nil {
		t.Errorf("oom_kills is absent from the file and reads as %d", *second.OOMKills)
	}

	third := recs[2]
	if third.MemoryPeakBytes == nil || *third.MemoryPeakBytes != 8192 || third.MemoryPeakScope != runtime.PeakUnspecified {
		t.Errorf("a lifetime peak with no scope: %+v", third)
	}
}
