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

// Cache is a directory of <name>/rootfs entries.
type Cache struct {
	root string
}

// NewCache returns a cache rooted at dir (the images directory).
func NewCache(dir string) *Cache { return &Cache{root: dir} }

// Root returns the cache root.
func (c *Cache) Root() string { return c.root }

// Name derives the cache entry name from an image reference.
//
//	vora/base        -> base
//	python:3.13-slim -> python
//	node:22          -> node
//	ghcr.io/x/y:tag  -> y
func Name(ref string) string {
	s := ref
	if i := strings.IndexByte(s, '@'); i >= 0 {
		s = s[:i]
	}
	if i := strings.LastIndexByte(s, '/'); i >= 0 {
		s = s[i+1:]
	}
	if i := strings.IndexByte(s, ':'); i >= 0 {
		s = s[:i]
	}
	return s
}

// Rootfs returns the rootfs path for an image reference. It does not check
// existence; use Has or Resolve for that.
func (c *Cache) Rootfs(ref string) string {
	return filepath.Join(c.root, Name(ref), "rootfs")
}

// Resolve returns the rootfs path for an image reference, or ErrNotFound.
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

// Names returns the cached image names, sorted.
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
		if info, err := os.Stat(filepath.Join(c.root, e.Name(), "rootfs")); err == nil && info.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}
