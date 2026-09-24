package images

import (
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"

	"github.com/vorash/vora/internal/config"
)

// Verifier verifies that an image reference carries a trusted signature.
type Verifier interface {
	Verify(ctx context.Context, ref string) error
}

// Policy applies an image signature policy before an image is pulled and
// unpacked. When disabled it is a no-op; when enforcing, an image whose
// signature cannot be verified is refused.
type Policy struct {
	mode     string
	verifier Verifier
	logger   *slog.Logger
}

// NewPolicy builds a policy from configuration. In enforce mode a verifier is
// required; without one it returns an error so enforcement is never silently
// absent. When disabled it logs loudly, because trusting every image is a
// security decision that should be visible.
func NewPolicy(cfg config.Config, logger *slog.Logger) (*Policy, error) {
	if logger == nil {
		logger = slog.Default()
	}
	switch cfg.ImagePolicy {
	case "", config.ImagePolicyOff:
		logger.Warn("image signature verification is disabled; images are trusted (set VORA_IMAGE_POLICY=enforce to require signatures)")
		return &Policy{mode: config.ImagePolicyOff, logger: logger}, nil
	case config.ImagePolicyEnforce:
		v, err := newCosignVerifier(cfg)
		if err != nil {
			return nil, err
		}
		logger.Info("image signature verification enforced", "cosign", v.path, "key", v.key)
		return &Policy{mode: config.ImagePolicyEnforce, verifier: v, logger: logger}, nil
	default:
		return nil, fmt.Errorf("images: unknown image policy %q", cfg.ImagePolicy)
	}
}

// Enabled reports whether a signature is required.
func (p *Policy) Enabled() bool { return p != nil && p.mode == config.ImagePolicyEnforce }

// Verify enforces the policy for ref. It is a no-op when disabled.
func (p *Policy) Verify(ctx context.Context, ref string) error {
	if p == nil || !p.Enabled() {
		return nil
	}
	if err := p.verifier.Verify(ctx, ref); err != nil {
		return fmt.Errorf("images: signature verification failed for %s: %w", ref, err)
	}
	return nil
}

// cosignVerifier runs `cosign verify --key <key> <ref>` and treats a non-zero
// exit as an untrusted image. The command's output is included in the error.
type cosignVerifier struct {
	path string
	key  string
	run  func(ctx context.Context, name string, args ...string) error
}

func newCosignVerifier(cfg config.Config) (*cosignVerifier, error) {
	if strings.TrimSpace(cfg.CosignKey) == "" {
		return nil, fmt.Errorf("images: VORA_IMAGE_POLICY=enforce requires VORA_COSIGN_KEY (path to a cosign public key)")
	}
	path := cfg.CosignPath
	if path == "" {
		found, err := exec.LookPath("cosign")
		if err != nil {
			return nil, fmt.Errorf("images: cosign not found (install cosign or set VORA_COSIGN_PATH): %w", err)
		}
		path = found
	}
	return &cosignVerifier{path: path, key: cfg.CosignKey, run: runCommand}, nil
}

func (v *cosignVerifier) Verify(ctx context.Context, ref string) error {
	return v.run(ctx, v.path, "verify", "--key", v.key, "--", ref)
}

func runCommand(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
