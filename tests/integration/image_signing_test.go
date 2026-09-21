//go:build integration

package integration

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/vorash/vora/internal/config"
	"github.com/vorash/vora/internal/images"
)

// TestImageSignaturePolicy enforces a real cosign policy: a signed image is
// accepted and an unsigned (or tampered) one is refused before any pull.
//
// It needs cosign, a registry and a signing key, so it is skipped unless the
// caller provides them:
//
//	VORA_TEST_COSIGN_KEY=cosign.pub \
//	VORA_TEST_SIGNED_IMAGE=ghcr.io/example/signed:tag \
//	VORA_TEST_UNSIGNED_IMAGE=ghcr.io/example/unsigned:tag \
//	go test -tags=integration -run TestImageSignaturePolicy ./tests/integration/...
func TestImageSignaturePolicy(t *testing.T) {
	if _, err := exec.LookPath("cosign"); err != nil {
		t.Skip("cosign not installed")
	}
	signed := os.Getenv("VORA_TEST_SIGNED_IMAGE")
	unsigned := os.Getenv("VORA_TEST_UNSIGNED_IMAGE")
	key := os.Getenv("VORA_TEST_COSIGN_KEY")
	if signed == "" || unsigned == "" || key == "" {
		t.Skip("set VORA_TEST_COSIGN_KEY, VORA_TEST_SIGNED_IMAGE and VORA_TEST_UNSIGNED_IMAGE")
	}

	cfg := config.Config{
		ImagePolicy: config.ImagePolicyEnforce,
		CosignKey:   key,
		ImagesMode:  config.ImagesCache, // policy runs before any source
	}
	r := images.NewResolver(cfg, nil)
	defer r.Close()
	ctx := context.Background()

	// The signed image must not be refused by the policy. (Resolve then fails
	// to find a local rootfs, which is not what this test is about.)
	if _, err := r.Resolve(ctx, signed); err != nil && strings.Contains(err.Error(), "signature verification failed") {
		t.Fatalf("signed image refused: %v", err)
	}
	// The unsigned image must be refused with a signature error.
	if _, err := r.Resolve(ctx, unsigned); err == nil || !strings.Contains(err.Error(), "signature verification failed") {
		t.Fatalf("unsigned image not refused by the policy: %v", err)
	}
}
