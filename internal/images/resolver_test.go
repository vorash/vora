package images

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/vorash/vora/internal/config"
)

func TestCandidateRefsAndNormalize(t *testing.T) {
	cases := map[string]string{
		"python:3.13-slim":  "docker.io/library/python:3.13-slim",
		"python":            "docker.io/library/python:latest",
		"vora/base":         "docker.io/vora/base:latest",
		"ghcr.io/x/y:1":     "ghcr.io/x/y:1",
		"alpine@sha256:abc": "alpine@sha256:abc",
	}
	for in, want := range cases {
		if got := normalizedRef(in); got != want {
			t.Errorf("normalizedRef(%q) = %q, want %q", in, got, want)
		}
	}
	refs := candidateRefs("vora/base")
	if len(refs) < 2 || refs[0] != "vora/base" || refs[1] != "docker.io/vora/base:latest" {
		t.Fatalf("candidateRefs(vora/base) = %v", refs)
	}
}

// TestResolverContainerd exercises the containerd snapshot path against a real
// rootless containerd. It is opt-in so CI without containerd stays green.
func TestResolverContainerd(t *testing.T) {
	if os.Getenv("VORA_TEST_CONTAINERD") == "" {
		t.Skip("set VORA_TEST_CONTAINERD=1 to run against a live containerd")
	}
	ref := os.Getenv("VORA_TEST_IMAGE")
	if ref == "" {
		ref = "alpine:3.20"
	}
	ns := os.Getenv("VORA_TEST_CONTAINERD_NS")
	if ns == "" {
		ns = "default"
	}
	cfg := config.Config{
		StateDir:              t.TempDir(),
		ContainerdNamespace:   ns,
		ContainerdSnapshotter: "native",
		ImagesMode:            config.ImagesContainerd,
	}
	r := NewResolver(cfg, nil)
	defer r.Close()
	if !r.UsesContainerd() {
		t.Fatal("resolver did not connect to containerd")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	rootfs, err := r.Resolve(ctx, ref)
	if err != nil {
		t.Fatalf("resolve %s: %v", ref, err)
	}
	info, err := os.Stat(rootfs)
	if err != nil || !info.IsDir() {
		t.Fatalf("rootfs %q is not a directory: %v", rootfs, err)
	}
	if _, err := os.Stat(filepath.Join(rootfs, "bin")); err != nil {
		t.Fatalf("rootfs %q has no /bin: %v", rootfs, err)
	}
	t.Logf("resolved %s -> %s", ref, rootfs)
	// A second resolve must be served from the in-process cache.
	if again, err := r.Resolve(ctx, ref); err != nil || again != rootfs {
		t.Fatalf("second resolve: %q, %v", again, err)
	}
}
