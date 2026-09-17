package runner

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	v1 "github.com/vorash/vora/api/proto/v1"
	"github.com/vorash/vora/internal/config"
	"github.com/vorash/vora/internal/egress"
	"github.com/vorash/vora/internal/images"
	"github.com/vorash/vora/internal/runtime"
	"github.com/vorash/vora/internal/sandbox"
)

// brokerBackend is a backend that records what it was handed at start, so a
// test can drive the real runner create/start path without a sandbox.
type brokerBackend struct {
	creds   []egress.Credential
	started bool
}

type brokerHandle struct{ id string }

func (h brokerHandle) ID() string { return h.id }

func (b *brokerBackend) Name() string { return "bwrap" }
func (b *brokerBackend) Capabilities() runtime.Capabilities {
	return runtime.Capabilities{
		NetworkModes:           []runtime.NetworkMode{runtime.NetworkNone, runtime.NetworkRestricted},
		EnforcesResourceLimits: true,
	}
}
func (b *brokerBackend) Create(_ context.Context, spec runtime.SandboxSpec) (runtime.Handle, error) {
	return brokerHandle{id: spec.ID}, nil
}
func (b *brokerBackend) Start(_ context.Context, _ runtime.Handle) error {
	b.started = true
	return nil
}
func (b *brokerBackend) StartWithCredentials(_ context.Context, _ runtime.Handle, creds []egress.Credential) error {
	b.started = true
	// Copy, because the runner zeroes its own slice once the start call
	// returns; the test needs to see what the backend actually got.
	b.creds = append([]egress.Credential(nil), creds...)
	return nil
}
func (b *brokerBackend) Exec(context.Context, runtime.Handle, runtime.ExecRequest) (runtime.ExecStream, error) {
	return nil, runtime.ErrUnsupported
}
func (b *brokerBackend) Signal(context.Context, runtime.Handle, int32) error { return nil }
func (b *brokerBackend) Stats(context.Context, runtime.Handle) (*runtime.Stats, error) {
	return &runtime.Stats{}, nil
}
func (b *brokerBackend) Stop(context.Context, runtime.Handle) error    { return nil }
func (b *brokerBackend) Destroy(context.Context, runtime.Handle) error { return nil }

// TestBrokerSecretsAreNeverPersisted asserts the credential reaches the backend
// for the start call and exists nowhere the runner persists or logs: only the
// granted alias survives in the sandbox record.
//
// Why it matters: the runner's state directory and logs outlive the sandbox and
// are read by operators and backups. A credential there defeats brokering.
func TestBrokerSecretsAreNeverPersisted(t *testing.T) {
	cfg := config.Config{
		StateDir:   t.TempDir(),
		ImagesMode: config.ImagesCache,
		DefaultTTL: time.Minute,
	}
	if err := os.MkdirAll(images.NewCache(cfg.ImagesDir()).Rootfs("base"), 0o755); err != nil {
		t.Fatalf("fake image: %v", err)
	}

	backend := &brokerBackend{}
	registry := runtime.NewRegistry()
	if err := registry.Register(backend); err != nil {
		t.Fatalf("register: %v", err)
	}
	store := sandbox.NewStore(cfg.SandboxesDir())
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(io.MultiWriter(&logs), nil))
	s := NewServer(cfg, store, registry, "run_test", logger)

	const secret = "ghp_secret_that_must_not_be_persisted"
	ctx := context.Background()
	if _, err := s.CreateSandbox(ctx, &v1.CreateSandboxRequest{
		Spec: &v1.SandboxSpec{
			SandboxId: "sbx_persist",
			Image:     "base",
			Resources: &v1.ResourceLimits{CpuMillis: 1000, MemoryBytes: 256 << 20, Pids: 64},
			Network: &v1.NetworkPolicy{
				Mode:           v1.NetworkMode_NETWORK_RESTRICTED,
				AllowedDomains: []string{"api.example.com"},
				BrokerAliases:  []string{"github"},
			},
		},
		BrokerSecrets: []*v1.BrokerCredential{
			{Alias: "github", Host: "api.example.com", Scheme: egress.SchemeBearer, Value: secret},
		},
	}); err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	if _, err := s.StartSandbox(ctx, &v1.StartSandboxRequest{SandboxId: "sbx_persist"}); err != nil {
		t.Fatalf("StartSandbox: %v", err)
	}

	if !backend.started || len(backend.creds) != 1 || backend.creds[0].Value != secret {
		t.Fatalf("backend did not receive the credential: %+v", backend.creds)
	}

	raw, err := os.ReadFile(filepath.Join(store.Dir("sbx_persist"), "config.json"))
	if err != nil {
		t.Fatalf("read record: %v", err)
	}
	if strings.Contains(string(raw), secret) {
		t.Fatalf("credential value persisted in the sandbox record: %s", raw)
	}
	if !strings.Contains(string(raw), "broker_aliases") || !strings.Contains(string(raw), "github") {
		t.Fatalf("the granted alias is missing from the record: %s", raw)
	}
	if strings.Contains(logs.String(), secret) {
		t.Fatalf("credential value appeared in runner logs: %s", logs.String())
	}

	// The staged copy is consumed by the start call, so it cannot be replayed.
	if got := s.takeBrokerCredentials("sbx_persist"); got != nil {
		t.Fatalf("credentials survived the start call: %+v", got)
	}
}

// TestBrokerStartWithoutCredentialsFailsClosed asserts a sandbox that grants
// aliases cannot start when its credentials are gone (for example after a
// runner restart), instead of quietly running unbrokered.
func TestBrokerStartWithoutCredentialsFailsClosed(t *testing.T) {
	cfg := config.Config{StateDir: t.TempDir(), ImagesMode: config.ImagesCache, DefaultTTL: time.Minute}
	if err := os.MkdirAll(images.NewCache(cfg.ImagesDir()).Rootfs("base"), 0o755); err != nil {
		t.Fatalf("fake image: %v", err)
	}
	backend := &brokerBackend{}
	registry := runtime.NewRegistry()
	if err := registry.Register(backend); err != nil {
		t.Fatalf("register: %v", err)
	}
	s := NewServer(cfg, sandbox.NewStore(cfg.SandboxesDir()), registry, "run_test",
		slog.New(slog.NewTextHandler(io.Discard, nil)))

	ctx := context.Background()
	if _, err := s.CreateSandbox(ctx, &v1.CreateSandboxRequest{
		Spec: &v1.SandboxSpec{
			SandboxId: "sbx_no_creds",
			Image:     "base",
			Resources: &v1.ResourceLimits{CpuMillis: 1000, MemoryBytes: 256 << 20, Pids: 64},
			Network: &v1.NetworkPolicy{
				Mode:           v1.NetworkMode_NETWORK_RESTRICTED,
				AllowedDomains: []string{"api.example.com"},
				BrokerAliases:  []string{"github"},
			},
		},
	}); err == nil {
		t.Fatal("create accepted a grant without a credential")
	}
}
