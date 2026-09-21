package api

import (
	"context"
	"testing"
	"time"

	v1 "github.com/vorash/vora/api/proto/v1"
	"github.com/vorash/vora/internal/tunnel"
)

func TestRejectReason(t *testing.T) {
	res := &v1.ResourceLimits{CpuMillis: 1000, MemoryBytes: 512 << 20}
	r := Runner{
		ID: "r", Backends: []string{"bwrap"}, NetworkModes: []string{"none", "restricted"},
		CPUMillis: 4000, MemoryBytes: 4 << 30, MaxSandboxes: 10,
	}
	cases := []struct {
		name    string
		backend string
		network string
		used    sandboxResources
		want    string
	}{
		{"fits", "bwrap", "restricted", sandboxResources{}, ""},
		{"wrong backend", "gvisor", "none", sandboxResources{}, "backend"},
		{"wrong network", "bwrap", "full", sandboxResources{}, "network"},
		{"full", "bwrap", "none", sandboxResources{count: 10}, "max_sandboxes"},
		{"memory", "bwrap", "none", sandboxResources{mem: 4 << 30}, "memory"},
		{"cpu", "bwrap", "none", sandboxResources{cpu: 4000}, "cpu"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := rejectReason(r, c.backend, c.network, res, c.used); got != c.want {
				t.Fatalf("rejectReason = %q, want %q", got, c.want)
			}
		})
	}
}

func TestPlaceRunnerPicksLeastLoaded(t *testing.T) {
	fr := &fakeRunner{}
	s := New(Options{
		Store:        NewMemStore(),
		BootstrapKey: bootstrap,
		DialRunner:   func(string) (RunnerClient, error) { return fr, nil },
		Now:          time.Now,
	})
	ctx := context.Background()
	now := time.Now()
	for _, id := range []string{"run_loaded", "run_idle"} {
		if err := s.store.UpsertRunner(ctx, Runner{
			ID: id, Address: "127.0.0.1:9200", Backends: []string{"bwrap"},
			NetworkModes: []string{"none"}, CPUMillis: 4000, MemoryBytes: 4 << 30,
			MaxSandboxes: 10, LastHeartbeat: now,
		}); err != nil {
			t.Fatal(err)
		}
	}
	// Load one runner with a committed sandbox.
	if err := s.store.SetRoute(ctx, SandboxRoute{
		SandboxID: "sbx_1", RunnerID: "run_loaded", TenantID: "t",
		CPUMillis: 2000, MemoryBytes: 1 << 30,
	}); err != nil {
		t.Fatal(err)
	}

	pl, err := s.placeRunner(ctx, &v1.SandboxSpec{
		Backend:   "bwrap",
		Network:   &v1.NetworkPolicy{Mode: v1.NetworkMode_NETWORK_NONE},
		Resources: &v1.ResourceLimits{CpuMillis: 1000, MemoryBytes: 512 << 20},
	})
	if err != nil {
		t.Fatalf("placeRunner: %v", err)
	}
	if pl.runner.ID != "run_idle" {
		t.Fatalf("chose %s, want the idle runner", pl.runner.ID)
	}
	if pl.reason != "least-loaded" {
		t.Fatalf("reason = %q", pl.reason)
	}
}

func TestPlaceRunnerRejectsIncapableRunner(t *testing.T) {
	fr := &fakeRunner{}
	s := New(Options{
		Store:      NewMemStore(),
		DialRunner: func(string) (RunnerClient, error) { return fr, nil },
		Now:        time.Now,
	})
	ctx := context.Background()
	if err := s.store.UpsertRunner(ctx, Runner{
		ID: "run_bwrap", Address: "127.0.0.1:9200", Backends: []string{"bwrap"},
		NetworkModes: []string{"none"}, LastHeartbeat: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	// A gVisor-restricted request cannot be served by this runner.
	_, err := s.placeRunner(ctx, &v1.SandboxSpec{
		Backend:   "gvisor",
		Network:   &v1.NetworkPolicy{Mode: v1.NetworkMode_NETWORK_RESTRICTED},
		Resources: &v1.ResourceLimits{CpuMillis: 1000, MemoryBytes: 256 << 20},
	})
	if err == nil {
		t.Fatal("expected no capable runner")
	}
}

func TestRegisterMarksTunnelledRunner(t *testing.T) {
	s := New(Options{Store: NewMemStore(), Now: time.Now, Tunnel: tunnel.NewServer("", nil)})
	ctx := context.Background()
	if _, err := s.Register(ctx, &v1.RegisterRunnerRequest{
		RunnerId: "run_nat", Address: "", Backends: []string{"bwrap"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Register(ctx, &v1.RegisterRunnerRequest{
		RunnerId: "run_direct", Address: "10.0.0.5:9200", Backends: []string{"bwrap"},
	}); err != nil {
		t.Fatal(err)
	}
	nat, err := s.store.Runner(ctx, "run_nat")
	if err != nil {
		t.Fatal(err)
	}
	if !nat.Tunneled {
		t.Fatal("a runner that registers without an address must be tunnelled")
	}
	direct, err := s.store.Runner(ctx, "run_direct")
	if err != nil {
		t.Fatal(err)
	}
	if direct.Tunneled {
		t.Fatal("a runner with an address is not tunnelled")
	}
}

func TestDialTunnelledRunnerWithoutTunnelFails(t *testing.T) {
	s := New(Options{
		Store: NewMemStore(),
		DialRunner: func(string) (RunnerClient, error) {
			t.Fatal("must not dial the network for a tunnelled runner without a tunnel")
			return nil, nil
		},
		Now: time.Now,
	})
	if _, err := s.dialRunner(Runner{ID: "run_nat", Tunneled: true}); err == nil {
		t.Fatal("dialing a tunnelled runner without a tunnel must fail")
	}
}
