//go:build integration

package integration

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/grpc"

	v1 "github.com/vorash/vora/api/proto/v1"
	"github.com/vorash/vora/internal/api"
	"github.com/vorash/vora/internal/cgroup"
	"github.com/vorash/vora/internal/client"
	"github.com/vorash/vora/internal/config"
	"github.com/vorash/vora/internal/registry"
	"github.com/vorash/vora/internal/runner"
	"github.com/vorash/vora/internal/runtime"
	"github.com/vorash/vora/internal/runtime/bwrap"
	"github.com/vorash/vora/internal/sandbox"
	"github.com/vorash/vora/internal/ulid"
)

// TestAPIControlPlane drives the full control-plane path: a runner registers
// with vora-api over gRPC, then create/exec/logs run over the HTTP/JSON API
// with a bearer key, and an unknown key is rejected.
func TestAPIControlPlane(t *testing.T) {
	initBin := buildBinary(t, "vora-init")
	t.Setenv("VORA_INIT_BINARY", initBin)

	cgroups, err := cgroup.New(config.Config{SystemdMode: config.SystemdUser})
	if err != nil {
		t.Skipf("systemd user manager unavailable: %v", err)
	}
	rootfs := imageRootfs(t, "base")
	realImages := filepath.Dir(filepath.Dir(rootfs))

	stateDir := t.TempDir()
	if err := os.Symlink(realImages, filepath.Join(stateDir, "images")); err != nil {
		t.Fatalf("symlink images: %v", err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := config.Config{StateDir: stateDir, SystemdMode: config.SystemdUser, DefaultTTL: time.Minute}

	backend, err := bwrap.New(cfg, cgroups, logger)
	if err != nil {
		t.Fatalf("bwrap backend: %v", err)
	}
	reg := runtime.NewRegistry()
	if err := reg.Register(backend); err != nil {
		t.Fatalf("register backend: %v", err)
	}
	runnerID := ulid.NewRunner()
	runnerSrv := runner.NewServer(cfg, sandbox.NewStore(cfg.SandboxesDir()), reg, runnerID, logger)

	// Runner gRPC endpoint (the address the API dials).
	runnerLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("runner listen: %v", err)
	}
	runnerGRPC := grpc.NewServer()
	v1.RegisterRunnerServiceServer(runnerGRPC, runnerSrv)
	go func() { _ = runnerGRPC.Serve(runnerLn) }()
	t.Cleanup(runnerGRPC.Stop)

	// Control plane: HTTP API + registry gRPC.
	const bootKey = "vora_bootstrap_integration"
	const joinToken = "join-integration"
	apiSrv := api.New(api.Options{Store: api.NewMemStore(), Logger: logger, BootstrapKey: bootKey, JoinToken: joinToken})
	t.Cleanup(func() { _ = apiSrv.Close() })

	regLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("registry listen: %v", err)
	}
	regGRPC := grpc.NewServer()
	v1.RegisterRunnerRegistryServer(regGRPC, apiSrv)
	go func() { _ = regGRPC.Serve(regLn) }()
	t.Cleanup(regGRPC.Stop)

	httpSrv := httptest.NewServer(apiSrv.Handler())
	defer httpSrv.Close()

	// The runner dials out and registers, then heartbeats.
	regClient, err := registry.Dial(regLn.Addr().String(), joinToken)
	if err != nil {
		t.Fatalf("registry dial: %v", err)
	}
	defer regClient.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	reporter := &registry.Reporter{
		Client:   regClient,
		RunnerID: runnerID,
		Address:  runnerLn.Addr().String(),
		Backends: []string{"bwrap"},
		Version:  "test",
		Logger:   logger,
		Interval: 200 * time.Millisecond,
	}
	go reporter.Run(ctx)

	// Wait for the API to see a healthy runner.
	httpc, err := client.DialAPI(httpSrv.URL, bootKey)
	if err != nil {
		t.Fatalf("dial api: %v", err)
	}
	defer httpc.Close()
	waitFor(t, 10*time.Second, func() bool {
		runners, err := httpc.ListRunners(ctx)
		if err != nil {
			return false
		}
		for _, r := range runners {
			if r.ID == runnerID && r.Healthy {
				return true
			}
		}
		return false
	})

	// Bad key -> rejected.
	bad, err := client.DialAPI(httpSrv.URL, "vora_wrong")
	if err != nil {
		t.Fatalf("dial bad key: %v", err)
	}
	defer bad.Close()
	if _, err := bad.ListSandboxes(ctx, false); err == nil {
		t.Fatal("expected the API to reject an unknown key")
	}

	// Create + start through the API, then exec.
	sb, err := httpc.CreateSandbox(ctx, &v1.SandboxSpec{
		Image:     "vora/base",
		Resources: &v1.ResourceLimits{CpuMillis: 1000, MemoryBytes: 256 << 20, Pids: 64},
		Network:   &v1.NetworkPolicy{Mode: v1.NetworkMode_NETWORK_NONE},
	})
	if err != nil {
		t.Fatalf("CreateSandbox over API: %v", err)
	}
	if sb.GetState() != v1.SandboxState_RUNNING {
		t.Fatalf("state = %v, want RUNNING", sb.GetState())
	}
	t.Cleanup(func() { _ = httpc.DestroySandbox(context.Background(), sb.GetId()) })

	stream, err := httpc.Exec(ctx, &v1.ExecSandboxRequest{
		SandboxId: sb.GetId(),
		Exec:      &v1.ExecRequest{Command: "/bin/sh", Args: []string{"-c", "echo api-e2e"}},
	})
	if err != nil {
		t.Fatalf("Exec over API: %v", err)
	}
	var out string
	for {
		chunk, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("recv: %v", err)
		}
		out += string(chunk.GetStdout())
	}
	if out != "api-e2e\n" {
		t.Fatalf("stdout = %q, want %q", out, "api-e2e\n")
	}

	// Logs (lifecycle events) over the API.
	events, err := httpc.Events(ctx, sb.GetId())
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	if len(events) == 0 {
		t.Fatal("expected lifecycle events")
	}
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("condition not met within timeout")
}
