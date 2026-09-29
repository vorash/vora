package images

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/containerd/containerd/v2/core/mount"

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

// Rootless containerd reports snapshot paths inside its own mount namespace
// (/var/lib/containerd/...). A runner outside that namespace, such as the
// non-root system unit, must resolve them to the host directory, or no image
// can start a sandbox.
func TestRootfsDirResolvesRootlessContainerdPaths(t *testing.T) {
	data := t.TempDir()
	t.Setenv("XDG_DATA_HOME", data)
	const rel = "io.containerd.snapshotter.v1.native/snapshots/987654321/fs"
	if _, err := os.Stat("/var/lib/containerd/" + rel); err == nil {
		t.Skip("snapshot path exists on this host")
	}
	host := filepath.Join(data, "containerd", rel)
	if err := os.MkdirAll(host, 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := rootfsDir([]mount.Mount{{Type: "bind", Source: "/var/lib/containerd/" + rel}}, filepath.Join(t.TempDir(), "unused"))
	if err != nil {
		t.Fatal(err)
	}
	if got != host {
		t.Fatalf("rootfsDir = %q, want the host path %q", got, host)
	}
}

func TestRootlessHostPathStaysUnderTheDataRoot(t *testing.T) {
	data := t.TempDir()
	t.Setenv("XDG_DATA_HOME", data)
	for src, want := range map[string]string{
		"/var/lib/containerd/io.containerd.snapshotter.v1.native/snapshots/1/fs": filepath.Join(data, "containerd/io.containerd.snapshotter.v1.native/snapshots/1/fs"),
		"/var/lib/containerd/../../../etc":                                       filepath.Join(data, "containerd/etc"),
		"/var/lib/containerdx/snapshots/1/fs":                                    "",
		"/srv/snapshots/1/fs":                                                    "",
	} {
		if got := rootlessHostPath(src); got != want {
			t.Errorf("rootlessHostPath(%q) = %q, want %q", src, got, want)
		}
	}
}
