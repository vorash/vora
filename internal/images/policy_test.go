package images

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/vorash/vora/internal/config"
)

type fakeVerifier struct{ err error }

func (f fakeVerifier) Verify(context.Context, string) error { return f.err }

func TestPolicyOffIsNoop(t *testing.T) {
	p, err := NewPolicy(config.Config{}, nil)
	if err != nil {
		t.Fatalf("NewPolicy: %v", err)
	}
	if p.Enabled() {
		t.Fatal("policy must be disabled by default")
	}
	if err := p.Verify(context.Background(), "ghcr.io/example/image:tag"); err != nil {
		t.Fatalf("disabled policy must not verify: %v", err)
	}
}

func TestPolicyEnforceRequiresKey(t *testing.T) {
	_, err := NewPolicy(config.Config{ImagePolicy: config.ImagePolicyEnforce}, nil)
	if err == nil {
		t.Fatal("enforce without a key must fail, so enforcement is never silently absent")
	}
}

func TestPolicyVerifyRefusesUntrusted(t *testing.T) {
	p := &Policy{mode: config.ImagePolicyEnforce, verifier: fakeVerifier{err: errors.New("no matching signatures")}}
	err := p.Verify(context.Background(), "ghcr.io/example/image:tag")
	if err == nil {
		t.Fatal("an unverifiable image must be refused")
	}
}

func TestPolicyVerifyAllowsTrusted(t *testing.T) {
	p := &Policy{mode: config.ImagePolicyEnforce, verifier: fakeVerifier{}}
	if err := p.Verify(context.Background(), "ghcr.io/example/image:tag"); err != nil {
		t.Fatalf("verified image refused: %v", err)
	}
}

func TestResolverRefusesWhenPolicyMisconfigured(t *testing.T) {
	r := NewResolver(config.Config{ImagePolicy: config.ImagePolicyEnforce, ImagesMode: config.ImagesCache}, nil)
	defer r.Close()
	if _, err := r.Resolve(context.Background(), "ghcr.io/example/image:tag"); err == nil {
		t.Fatal("a misconfigured enforce policy must refuse every image")
	}
}

func TestEnforceNeverUsesCacheOrMutableTag(t *testing.T) {
	r := NewResolver(config.Config{ImagesMode: config.ImagesCache}, nil)
	defer r.Close()
	r.policy = &Policy{mode: config.ImagePolicyEnforce, verifier: fakeVerifier{}}
	for _, ref := range []string{"ghcr.io/example/image:tag", "ghcr.io/example/image@sha256:" + strings.Repeat("a", 64)} {
		if err := os.MkdirAll(r.cache.Rootfs(ref), 0o755); err != nil {
			t.Fatal(err)
		}
		if _, err := r.Resolve(context.Background(), ref); err == nil {
			t.Fatalf("enforce accepted %q without an authenticated digest pull", ref)
		}
	}
}

func TestCosignVerifierCommand(t *testing.T) {
	var gotName string
	var gotArgs []string
	v := &cosignVerifier{
		path: "/usr/bin/cosign",
		key:  "/keys/cosign.pub",
		run: func(_ context.Context, name string, args ...string) error {
			gotName, gotArgs = name, args
			return nil
		},
	}
	if err := v.Verify(context.Background(), "ghcr.io/example/image@sha256:"+strings.Repeat("a", 64)); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if gotName != "/usr/bin/cosign" {
		t.Fatalf("binary = %q", gotName)
	}
	want := []string{"verify", "--key", "/keys/cosign.pub", "--", "ghcr.io/example/image@sha256:" + strings.Repeat("a", 64)}
	if len(gotArgs) != len(want) {
		t.Fatalf("args = %v", gotArgs)
	}
	for i := range want {
		if gotArgs[i] != want[i] {
			t.Fatalf("args = %v, want %v", gotArgs, want)
		}
	}
}
