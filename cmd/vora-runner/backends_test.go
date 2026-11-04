package main

import (
	"errors"
	"io"
	"log/slog"
	"slices"
	"strings"
	"testing"

	"github.com/vorash/vora/internal/config"
	"github.com/vorash/vora/internal/runtime"
	"github.com/vorash/vora/internal/runtime/bwrap"
	"github.com/vorash/vora/internal/runtime/gvisor"
)

type fakeBackend struct {
	runtime.Backend
	name  string
	modes []runtime.NetworkMode
}

func (f fakeBackend) Name() string { return f.name }

func (f fakeBackend) Capabilities() runtime.Capabilities {
	return runtime.Capabilities{NetworkModes: f.modes}
}

func builds(name string, modes ...runtime.NetworkMode) backendBuilder {
	return backendBuilder{name, func() (runtime.Backend, error) {
		return fakeBackend{name: name, modes: modes}, nil
	}}
}

func fails(name string, err error) backendBuilder {
	return backendBuilder{name, func() (runtime.Backend, error) { return nil, err }}
}

func mustNotBuild(t *testing.T, name string) backendBuilder {
	return backendBuilder{name, func() (runtime.Backend, error) {
		t.Errorf("built %s, which VORA_BACKENDS does not list", name)
		return fakeBackend{name: name}, nil
	}}
}

func backendSet(t *testing.T, v string) config.BackendSet {
	t.Helper()
	set, err := config.ParseBackends(v)
	if err != nil {
		t.Fatal(err)
	}
	return set
}

var discard = slog.New(slog.NewTextHandler(io.Discard, nil))

// The root gVisor runner on a host that also runs a non-root bwrap runner:
// it must not construct bwrap (cgroup.New, bwrap.New) and must advertise only
// gvisor and gvisor's network modes, which are what the registry reporter
// sends vora-api for placement.
func TestRegisterSelectedRegistersAndAdvertisesOnlyListedBackends(t *testing.T) {
	reg := runtime.NewRegistry()
	builders := []backendBuilder{
		mustNotBuild(t, config.BackendBwrap),
		builds(config.BackendGvisor, runtime.NetworkNone, runtime.NetworkRestricted),
	}
	if err := registerSelected(reg, backendSet(t, "gvisor"), builders, discard); err != nil {
		t.Fatal(err)
	}
	if got := reg.Names(); !slices.Equal(got, []string{"gvisor"}) {
		t.Fatalf("registered backends = %v, want [gvisor]", got)
	}
	if got := advertisedNetworkModes(reg); !slices.Equal(got, []string{"none", "restricted"}) {
		t.Fatalf("advertised network modes = %v, want gvisor's only", got)
	}
}

// An operator who listed gvisor asked for it. A runner that started without
// it would register with vora-api looking healthy and take no gVisor work, so
// the failure must stop startup and name the backend and its cause.
func TestRegisterSelectedFailsWhenListedBackendFails(t *testing.T) {
	cause := errors.New("runsc needs the runner at uid 0")
	builders := []backendBuilder{
		builds(config.BackendBwrap),
		fails(config.BackendGvisor, cause),
	}
	err := registerSelected(runtime.NewRegistry(), backendSet(t, "bwrap,gvisor"), builders, discard)
	if !errors.Is(err, cause) || !strings.Contains(err.Error(), "gvisor") {
		t.Fatalf("err = %v, want one naming gvisor and wrapping its cause", err)
	}
}

// Without VORA_BACKENDS the runner keeps its old behavior: a backend that
// cannot start on this host (gVisor on the non-root runner) is skipped.
func TestRegisterSelectedSkipsFailingBackendWithoutAllowlist(t *testing.T) {
	reg := runtime.NewRegistry()
	builders := []backendBuilder{
		builds(config.BackendBwrap, runtime.NetworkFull),
		fails(config.BackendGvisor, errors.New("not uid 0")),
	}
	if err := registerSelected(reg, config.BackendSet{}, builders, discard); err != nil {
		t.Fatal(err)
	}
	if got := reg.Names(); !slices.Equal(got, []string{"bwrap"}) {
		t.Fatalf("registered backends = %v, want [bwrap]", got)
	}
}

// A listed name with no builder (a backend added to the allowlist but not to
// the runner) must fail rather than leave the runner without it.
func TestRegisterSelectedFailsWhenListedBackendHasNoBuilder(t *testing.T) {
	builders := []backendBuilder{builds(config.BackendBwrap)}
	if err := registerSelected(runtime.NewRegistry(), backendSet(t, "bwrap,gvisor"), builders, discard); err == nil {
		t.Fatal("registerSelected succeeded without the listed gvisor backend")
	}
}

// VORA_BACKENDS names, registry names and the names vora-api matches a
// sandbox's backend against must be the same strings.
func TestBackendNamesMatchRuntimeNames(t *testing.T) {
	if got := (&bwrap.Backend{}).Name(); got != config.BackendBwrap {
		t.Fatalf("bwrap backend is named %q, config says %q", got, config.BackendBwrap)
	}
	if got := (&gvisor.Backend{}).Name(); got != config.BackendGvisor {
		t.Fatalf("gvisor backend is named %q, config says %q", got, config.BackendGvisor)
	}
}
