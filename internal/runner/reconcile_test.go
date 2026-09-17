package runner

import (
	"context"
	"testing"

	"github.com/vorash/vora/internal/runtime"
	"github.com/vorash/vora/internal/sandbox"
)

type fakeSweeper struct {
	fakeBackend
	listed []string
	reaped []string
}

func (f *fakeSweeper) List(context.Context) ([]string, error) { return f.listed, nil }
func (f *fakeSweeper) Reap(_ context.Context, id string) error {
	f.reaped = append(f.reaped, id)
	return nil
}

func TestReconcileReapsActiveStoreRecord(t *testing.T) {
	sw := &fakeSweeper{fakeBackend: fakeBackend{name: "gvisor"}, listed: []string{"sbx_live"}}
	reg := runtime.NewRegistry()
	if err := reg.Register(sw); err != nil {
		t.Fatal(err)
	}
	s := testServer(t, reg)
	rec := &sandbox.Record{
		ID:    "sbx_live",
		State: sandbox.StateRunning,
		Spec:  runtime.SandboxSpec{ID: "sbx_live", Backend: "gvisor"},
	}
	if err := s.store.Create(rec); err != nil {
		t.Fatal(err)
	}
	if err := s.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(sw.reaped) == 0 || sw.reaped[0] != "sbx_live" {
		t.Fatalf("reaped = %v, want [sbx_live]", sw.reaped)
	}
	got, err := s.store.Load("sbx_live")
	if err != nil {
		t.Fatal(err)
	}
	if got.State != sandbox.StateFailed {
		t.Fatalf("state = %s, want FAILED", got.State)
	}
}

func TestReconcileReapsUnknownRuntime(t *testing.T) {
	sw := &fakeSweeper{fakeBackend: fakeBackend{name: "gvisor"}, listed: []string{"sbx_ghost"}}
	reg := runtime.NewRegistry()
	if err := reg.Register(sw); err != nil {
		t.Fatal(err)
	}
	s := testServer(t, reg)
	if err := s.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(sw.reaped) != 1 || sw.reaped[0] != "sbx_ghost" {
		t.Fatalf("reaped = %v, want [sbx_ghost]", sw.reaped)
	}
}
