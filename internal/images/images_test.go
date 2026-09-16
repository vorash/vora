package images

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestName(t *testing.T) {
	cases := map[string]string{
		"vora/base":        "base",
		"python:3.13-slim": "python",
		"node:22":          "node",
		"ghcr.io/x/y:tag":  "y",
		"alpine":           "alpine",
		"img@sha256:abc":   "img",
		"":                 "",
	}
	for in, want := range cases {
		if got := Name(in); got != want {
			t.Errorf("Name(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestResolveAndNames(t *testing.T) {
	dir := t.TempDir()
	c := NewCache(dir)

	if _, err := c.Resolve("python:3.13"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Resolve miss error = %v, want ErrNotFound", err)
	}
	if c.Has("python:3.13") {
		t.Fatal("Has should be false for missing image")
	}

	if err := os.MkdirAll(filepath.Join(dir, "python", "rootfs"), 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := c.Resolve("python:3.13")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if want := filepath.Join(dir, "python", "rootfs"); got != want {
		t.Fatalf("Resolve = %q, want %q", got, want)
	}

	names, err := c.Names()
	if err != nil {
		t.Fatalf("Names: %v", err)
	}
	if len(names) != 1 || names[0] != "python" {
		t.Fatalf("Names = %v", names)
	}
}
