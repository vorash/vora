package runtime

import (
	"context"
	"errors"
	"testing"
	"time"
)

func validSpec() SandboxSpec {
	return SandboxSpec{
		ID:    "sbx_test",
		Image: "vora/base",
		Resources: ResourceLimits{
			CPUMillis:   1000,
			MemoryBytes: 512 << 20,
			Pids:        128,
		},
		Network: NetworkPolicy{Mode: NetworkNone},
		TTL:     time.Minute,
	}
}

func TestValidateAcceptsValidSpec(t *testing.T) {
	if err := validSpec().Validate(); err != nil {
		t.Fatalf("valid spec rejected: %v", err)
	}
}

func TestValidateRejectsBadSpecs(t *testing.T) {
	cases := map[string]func(*SandboxSpec){
		"missing id":     func(s *SandboxSpec) { s.ID = "" },
		"missing image":  func(s *SandboxSpec) { s.Image = "" },
		"zero memory":    func(s *SandboxSpec) { s.Resources.MemoryBytes = 0 },
		"zero cpu":       func(s *SandboxSpec) { s.Resources.CPUMillis = 0 },
		"zero pids":      func(s *SandboxSpec) { s.Resources.Pids = 0 },
		"empty net mode": func(s *SandboxSpec) { s.Network.Mode = "" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			s := validSpec()
			mutate(&s)
			err := s.Validate()
			if !errors.Is(err, ErrInvalidSpec) {
				t.Fatalf("Validate() = %v, want ErrInvalidSpec", err)
			}
		})
	}
}

func TestCapabilitiesSupportsNetwork(t *testing.T) {
	c := Capabilities{NetworkModes: []NetworkMode{NetworkNone}}
	if !c.SupportsNetwork(NetworkNone) {
		t.Error("expected NetworkNone to be supported")
	}
	if c.SupportsNetwork(NetworkRestricted) {
		t.Error("expected NetworkRestricted to be unsupported")
	}
}

type fakeBackend struct {
	name string
	caps Capabilities
}

func (f *fakeBackend) Name() string               { return f.name }
func (f *fakeBackend) Capabilities() Capabilities { return f.caps }
func (f *fakeBackend) Create(context.Context, SandboxSpec) (Handle, error) {
	return nil, ErrUnsupported
}
func (f *fakeBackend) Start(context.Context, Handle) error { return ErrUnsupported }
func (f *fakeBackend) Exec(context.Context, Handle, ExecRequest) (ExecStream, error) {
	return nil, ErrUnsupported
}
func (f *fakeBackend) Signal(context.Context, Handle, int32) error { return ErrUnsupported }
func (f *fakeBackend) Stats(context.Context, Handle) (*Stats, error) {
	return nil, ErrUnsupported
}
func (f *fakeBackend) Stop(context.Context, Handle) error    { return ErrUnsupported }
func (f *fakeBackend) Destroy(context.Context, Handle) error { return ErrUnsupported }

func TestRegistry(t *testing.T) {
	r := NewRegistry()
	if _, ok := r.Default(); ok {
		t.Fatal("empty registry should have no default")
	}
	if err := r.Register(&fakeBackend{name: "bwrap"}); err != nil {
		t.Fatalf("register bwrap: %v", err)
	}
	if err := r.Register(&fakeBackend{name: "gvisor"}); err != nil {
		t.Fatalf("register gvisor: %v", err)
	}
	if err := r.Register(&fakeBackend{name: "bwrap"}); err == nil {
		t.Fatal("duplicate registration should fail")
	}

	def, ok := r.Default()
	if !ok || def.Name() != "bwrap" {
		t.Fatalf("default = %v, want bwrap", def)
	}
	if _, ok := r.Get("gvisor"); !ok {
		t.Fatal("gvisor should be registered")
	}
	names := r.Names()
	if len(names) != 2 || names[0] != "bwrap" || names[1] != "gvisor" {
		t.Fatalf("Names() = %v", names)
	}
}
