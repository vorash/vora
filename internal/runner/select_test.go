package runner

import (
	"context"
	"io"
	"log/slog"
	"os"
	"testing"

	"github.com/vorash/vora/internal/config"
	"github.com/vorash/vora/internal/runtime"
	"github.com/vorash/vora/internal/sandbox"
)

type fakeBackend struct {
	name     string
	noLimits bool
}

func (f *fakeBackend) Name() string { return f.name }
func (f *fakeBackend) Capabilities() runtime.Capabilities {
	return runtime.Capabilities{
		NetworkModes:           []runtime.NetworkMode{runtime.NetworkNone},
		EnforcesResourceLimits: !f.noLimits,
	}
}
func (f *fakeBackend) Create(context.Context, runtime.SandboxSpec) (runtime.Handle, error) {
	return nil, runtime.ErrUnsupported
}
func (f *fakeBackend) Start(context.Context, runtime.Handle) error { return nil }
func (f *fakeBackend) Exec(context.Context, runtime.Handle, runtime.ExecRequest) (runtime.ExecStream, error) {
	return nil, runtime.ErrUnsupported
}
func (f *fakeBackend) Signal(context.Context, runtime.Handle, int32) error { return nil }
func (f *fakeBackend) Stats(context.Context, runtime.Handle) (*runtime.Stats, error) {
	return &runtime.Stats{}, nil
}
func (f *fakeBackend) Meter(context.Context, runtime.Handle) (runtime.UsageMeter, error) {
	return &fakeMeter{}, nil
}
func (f *fakeBackend) Stop(context.Context, runtime.Handle) error    { return nil }
func (f *fakeBackend) Destroy(context.Context, runtime.Handle) error { return nil }

func testRegistry(t *testing.T) *runtime.Registry {
	t.Helper()
	r := runtime.NewRegistry()
	if err := r.Register(&fakeBackend{name: "bwrap"}); err != nil {
		t.Fatal(err)
	}
	if err := r.Register(&fakeBackend{name: "gvisor"}); err != nil {
		t.Fatal(err)
	}
	return r
}

func testServer(t *testing.T, reg *runtime.Registry) *Server {
	t.Helper()
	cfg := config.Config{StateDir: t.TempDir()}
	store := sandbox.NewStore(cfg.SandboxesDir())
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	s := NewServer(cfg, store, reg, "run_test", logger)
	if err := s.LoadPools(); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestSelectBackendExplicit(t *testing.T) {
	s := testServer(t, testRegistry(t))
	for _, name := range []string{"bwrap", "gvisor"} {
		b, err := s.selectBackend(runtime.SandboxSpec{Backend: name})
		if err != nil {
			t.Fatalf("selectBackend(%q): %v", name, err)
		}
		if b.Name() != name {
			t.Fatalf("selectBackend(%q) = %s", name, b.Name())
		}
	}
	if _, err := s.selectBackend(runtime.SandboxSpec{Backend: "nope"}); err == nil {
		t.Fatal("unknown backend should error")
	}
}

func TestSelectBackendAuto(t *testing.T) {
	s := testServer(t, testRegistry(t))
	b, err := s.selectBackend(runtime.SandboxSpec{})
	if err != nil {
		t.Fatalf("auto select: %v", err)
	}
	// Tests run unprivileged, so auto-selection must pick bwrap (gVisor needs a
	// user namespace / root).
	want := "bwrap"
	if os.Getuid() == 0 {
		want = "gvisor"
	}
	if b.Name() != want {
		t.Fatalf("auto select = %s, want %s", b.Name(), want)
	}
}

func TestSelectBackendSkipsUnlimitedGvisor(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("gVisor auto-prefer only applies as uid 0")
	}
	r := runtime.NewRegistry()
	if err := r.Register(&fakeBackend{name: "bwrap"}); err != nil {
		t.Fatal(err)
	}
	if err := r.Register(&fakeBackend{name: "gvisor", noLimits: true}); err != nil {
		t.Fatal(err)
	}
	s := testServer(t, r)
	b, err := s.selectBackend(runtime.SandboxSpec{})
	if err != nil {
		t.Fatal(err)
	}
	if b.Name() != "bwrap" {
		t.Fatalf("unlimited gVisor was auto-selected: %s", b.Name())
	}
}
