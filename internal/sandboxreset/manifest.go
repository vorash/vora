//go:build linux

// Package sandboxreset is the in-sandbox half of resetting a sandbox between
// runs. Capture lists every writable mount before the first workload runs.
// Restore later removes whatever the listing does not have, refuses anything
// the listing has that changed, and proves the result on a fresh walk.
//
// Restore only removes. It never recreates or repairs an entry, because the
// caller destroys a sandbox it cannot prove clean.
package sandboxreset

import (
	"errors"
	"fmt"
	"io/fs"
	"slices"
)

var (
	// ErrRootWritable means "/" is writable, so changes could land outside
	// every listing.
	ErrRootWritable = errors.New("sandboxreset: root mount is writable")
	// ErrUnrestorable means an entry in the listing changed or is gone.
	ErrUnrestorable = errors.New("sandboxreset: listed entry changed")
	// ErrNotClean means the tree holds entries the listing does not have.
	ErrNotClean = errors.New("sandboxreset: unlisted entries present")
	// ErrMountsChanged means the live mount table is not the captured one, so
	// the listings no longer describe every writable mount.
	ErrMountsChanged = errors.New("sandboxreset: mount table changed")
	// ErrUnlistedMount means a walk reached a directory on another device that
	// the mount table does not list.
	ErrUnlistedMount = errors.New("sandboxreset: unlisted mount boundary")
)

// Manifest is what one sandbox looked like at capture time. It travels as JSON
// and the host keeps it, so a workload cannot edit it.
type Manifest struct {
	// Table is the whole mount table, sorted by mount ID. A bind mount on the
	// same filesystem has the same st_dev as its parent, so only the table can
	// show that a mount appeared or moved.
	Table []Mount `json:"table"`
	// Mounts has one listing per writable mount, sorted by Path.
	Mounts []MountListing `json:"mounts"`
}

// MountListing is one writable mount and everything under it, up to the next
// mount boundary.
type MountListing struct {
	Path string `json:"path"`
	// Entries is sorted by Rel and always holds the mount root, Rel ".". The
	// root is not always a directory: bwrap binds /dev/null as its own mount.
	Entries []Entry `json:"entries"`
}

// EntryType is the kind of a file system object.
type EntryType string

const (
	TypeDir      EntryType = "dir"
	TypeFile     EntryType = "file"
	TypeSymlink  EntryType = "symlink"
	TypeCharDev  EntryType = "chardev"
	TypeBlockDev EntryType = "blockdev"
	TypeSocket   EntryType = "socket"
	TypeFIFO     EntryType = "fifo"
	// TypeMountpoint is a mount nested in this one. Only Rel and Type are set:
	// what it holds is not this listing's.
	TypeMountpoint EntryType = "mountpoint"
)

// Entry identifies one file system object. Two equal entries mean that a later
// run reads the same type, permissions, owner, device, link target, content and
// extended attributes from the same inode. Timestamps are not part of it, because removing a planted
// file changes its directory's.
type Entry struct {
	Rel    string    `json:"rel"`
	Type   EntryType `json:"type"`
	Mode   uint32    `json:"mode"`
	UID    uint32    `json:"uid"`
	GID    uint32    `json:"gid"`
	Ino    uint64    `json:"ino"`
	Rdev   uint64    `json:"rdev,omitempty"`
	Size   int64     `json:"size,omitempty"`
	SHA256 string    `json:"sha256,omitempty"`
	Target string    `json:"target,omitempty"`
	// Xattrs is a digest of every extended attribute, POSIX ACLs included. It is
	// empty when there are none.
	Xattrs string `json:"xattrs,omitempty"`
}

// validate is the check at the boundary where a manifest comes back from the
// host: every path Restore acts on must come from a table that selects it.
func (m Manifest) validate() error {
	plan, err := selectMounts(m.Table)
	if err != nil {
		return err
	}
	if len(plan) != len(m.Mounts) {
		return fmt.Errorf("manifest has %d listings, its table selects %d", len(m.Mounts), len(plan))
	}
	for i, want := range plan {
		l := m.Mounts[i]
		if l.Path != want.Path {
			return fmt.Errorf("listing %d is %s, its table selects %s", i, l.Path, want.Path)
		}
		if err := validateEntries(l, want.Nested); err != nil {
			return fmt.Errorf("listing %s: %w", l.Path, err)
		}
	}
	return nil
}

func validateEntries(l MountListing, nested map[string]bool) error {
	seen := 0
	hasRoot := false
	for i, e := range l.Entries {
		// ValidPath also refuses invalid UTF-8, which JSON would hand back changed.
		if !fs.ValidPath(e.Rel) {
			return fmt.Errorf("entry path %q is not a plain relative path", e.Rel)
		}
		if i > 0 && l.Entries[i-1].Rel >= e.Rel {
			return fmt.Errorf("entries not strictly sorted at %q", e.Rel)
		}
		if !slices.Contains(entryTypes, e.Type) {
			return fmt.Errorf("entry %q has unknown type %q", e.Rel, e.Type)
		}
		if e.Rel == "." {
			hasRoot = e.Type != TypeMountpoint
		}
		if e.Type == TypeMountpoint {
			if !nested[e.Rel] {
				return fmt.Errorf("entry %q is a mountpoint the table does not list", e.Rel)
			}
			seen++
		}
	}
	if !hasRoot {
		return errors.New("no entry for the mount root")
	}
	// Restore would treat a nested mount missing from the listing as planted.
	if seen != len(nested) {
		return fmt.Errorf("%d nested mounts listed, the table has %d", seen, len(nested))
	}
	return nil
}

var entryTypes = []EntryType{TypeDir, TypeFile, TypeSymlink, TypeCharDev, TypeBlockDev, TypeSocket, TypeFIFO, TypeMountpoint}

// diff compares a listing to a fresh walk. extra holds what got has and want
// does not. changed holds what want has that got lacks or describes differently.
func diff(want, got []Entry) (extra, changed []string) {
	wantByRel := make(map[string]Entry, len(want))
	for _, w := range want {
		wantByRel[w.Rel] = w
	}
	gotByRel := make(map[string]Entry, len(got))
	for _, g := range got {
		gotByRel[g.Rel] = g
		if _, listed := wantByRel[g.Rel]; !listed {
			extra = append(extra, g.Rel)
		}
	}
	for _, w := range want {
		if g, ok := gotByRel[w.Rel]; !ok || g != w {
			changed = append(changed, w.Rel)
		}
	}
	return extra, changed
}
