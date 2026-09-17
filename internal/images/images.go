// Package images resolves sandbox images to read-only rootfs directories in
// the image cache produced by scripts/build-images.sh.
package images

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ErrNotFound means the image has not been built into the cache.
var ErrNotFound = errors.New("images: image not found in cache")

// Cache is a directory of <name>/<tag>/rootfs entries.
type Cache struct {
	root string
}

// NewCache returns a cache rooted at dir (the images directory).
func NewCache(dir string) *Cache { return &Cache{root: dir} }

// Root returns the cache root.
func (c *Cache) Root() string { return c.root }

// Name is the image name (last path component, no tag or digest).
//
//	vora/base        -> base
//	python:3.13-slim -> python
//	ghcr.io/x/y:tag  -> y
func Name(ref string) string {
	name, _ := split(ref)
	return name
}

// Key is the cache directory relative to the images root: <name>/<tag>.
//
//	python:3.13        -> python/3.13
//	python             -> python/latest
//	vora/python:latest -> python/latest
//	img@sha256:abc     -> img/sha256-abc
func Key(ref string) string {
	name, tag := split(ref)
	if name == "" {
		return ""
	}
	return name + "/" + tag
}

func split(ref string) (name, tag string) {
	s := strings.TrimSpace(ref)
	if s == "" {
		return "", ""
	}
	if i := strings.IndexByte(s, '@'); i >= 0 {
		digest := s[i+1:]
		s = s[:i]
		name = lastComponent(stripTag(s))
		return name, strings.ReplaceAll(digest, ":", "-")
	}
	name = lastComponent(s)
	if i := strings.IndexByte(name, ':'); i >= 0 {
		return name[:i], name[i+1:]
	}
	if name == "" {
		return "", ""
	}
	return name, "latest"
}

func lastComponent(s string) string {
	if i := strings.LastIndexByte(s, '/'); i >= 0 {
		return s[i+1:]
	}
	return s
}

func stripTag(s string) string {
	if i := strings.IndexByte(s, ':'); i >= 0 {
		return s[:i]
	}
	return s
}

// Rootfs returns the rootfs path for an image reference. It does not check
// existence; use Has or Resolve for that.
func (c *Cache) Rootfs(ref string) string {
	return filepath.Join(c.root, Key(ref), "rootfs")
}

// Resolve returns the rootfs path for an image reference, or ErrNotFound.
// The tag must match exactly; python:3.13 does not fall back to python/latest.
func (c *Cache) Resolve(ref string) (string, error) {
	if Name(ref) == "" {
		return "", fmt.Errorf("images: empty image reference")
	}
	path := c.Rootfs(ref)
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("%w: %s (run 'make images')", ErrNotFound, ref)
		}
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("images: %s exists but is not a directory", path)
	}
	return path, nil
}

// Has reports whether the image is cached.
func (c *Cache) Has(ref string) bool {
	_, err := c.Resolve(ref)
	return err == nil
}

// Names returns the cached image references (name:tag), sorted.
func (c *Cache) Names() ([]string, error) {
	entries, err := os.ReadDir(c.root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		tags, err := os.ReadDir(filepath.Join(c.root, e.Name()))
		if err != nil {
			continue
		}
		for _, t := range tags {
			if !t.IsDir() || strings.HasPrefix(t.Name(), ".") {
				continue
			}
			rootfs := filepath.Join(c.root, e.Name(), t.Name(), "rootfs")
			if info, err := os.Stat(rootfs); err == nil && info.IsDir() {
				names = append(names, e.Name()+":"+t.Name())
			}
		}
	}
	sort.Strings(names)
	return names, nil
}
