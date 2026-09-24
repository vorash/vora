package images

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestKey(t *testing.T) {
	cases := map[string]string{
		"vora/base":                             "base/latest",
		"python:3.13-slim":                      "python/3.13-slim",
		"python:3.13":                           "python/3.13",
		"python":                                "python/latest",
		"node:22":                               "node/22",
		"ghcr.io/x/y:tag":                       "y/tag",
		"alpine":                                "alpine/latest",
		"img@sha256:" + strings.Repeat("a", 64): "img/sha256-" + strings.Repeat("a", 64),
		"vora/python:latest":                    "python/latest",
		"":                                      "",
	}
	for in, want := range cases {
		if got := Key(in); got != want {
			t.Errorf("Key(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestName(t *testing.T) {
	cases := map[string]string{
		"vora/base":                             "base",
		"python:3.13-slim":                      "python",
		"node:22":                               "node",
		"ghcr.io/x/y:tag":                       "y",
		"alpine":                                "alpine",
		"img@sha256:" + strings.Repeat("a", 64): "img",
		"":                                      "",
	}
	for in, want := range cases {
		if got := Name(in); got != want {
			t.Errorf("Name(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCacheRejectsHostileReferences(t *testing.T) {
	c := NewCache(t.TempDir())
	for _, ref := range []string{"..:..", "x@sha256:a/../../../outside", "-flag", "x:../../escape", "x@sha256:invalid"} {
		t.Run(ref, func(t *testing.T) {
			if got := c.Rootfs(ref); got != "" {
				t.Fatalf("unsafe reference %q maps to host path %q", ref, got)
			}
			if _, err := c.Resolve(ref); err == nil {
				t.Fatalf("unsafe reference %q accepted", ref)
			}
		})
	}
}

func TestResolveExactTag(t *testing.T) {
	dir := t.TempDir()
	c := NewCache(dir)

	if _, err := c.Resolve("python:3.13"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Resolve miss error = %v, want ErrNotFound", err)
	}
	if c.Has("python:3.13") {
		t.Fatal("Has should be false for missing image")
	}

	if err := os.MkdirAll(filepath.Join(dir, "python", "latest", "rootfs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "python", "3.12", "rootfs"), 0o755); err != nil {
		t.Fatal(err)
	}

	if c.Has("python:3.13") {
		t.Fatal("python:3.13 must not resolve to python/latest or python/3.12")
	}
	got, err := c.Resolve("python")
	if err != nil {
		t.Fatalf("Resolve python: %v", err)
	}
	if want := filepath.Join(dir, "python", "latest", "rootfs"); got != want {
		t.Fatalf("Resolve python = %q, want %q", got, want)
	}
	got, err = c.Resolve("python:3.12")
	if err != nil {
		t.Fatalf("Resolve python:3.12: %v", err)
	}
	if want := filepath.Join(dir, "python", "3.12", "rootfs"); got != want {
		t.Fatalf("Resolve python:3.12 = %q, want %q", got, want)
	}

	names, err := c.Names()
	if err != nil {
		t.Fatalf("Names: %v", err)
	}
	if len(names) != 2 || names[0] != "python:3.12" || names[1] != "python:latest" {
		t.Fatalf("Names = %v, want [python:3.12 python:latest]", names)
	}
}
