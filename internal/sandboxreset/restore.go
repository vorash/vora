//go:build linux

package sandboxreset

import (
	"context"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// Restore removes everything m does not list and then proves the result on a
// fresh walk. It returns the number of objects removed. A nil error means every
// writable mount matches m exactly. Any other outcome means the sandbox was not
// proven clean and may be partly restored.
//
// mountinfo must be the live table. The caller must have stopped every other
// process in the sandbox first, or the proof says nothing about the next moment.
func Restore(ctx context.Context, m Manifest, mountinfo io.Reader) (removed int, err error) {
	if err := m.validate(); err != nil {
		return 0, fmt.Errorf("sandboxreset: restore: invalid manifest: %w", err)
	}
	live, err := parseMountinfo(mountinfo)
	if err != nil {
		return 0, fmt.Errorf("sandboxreset: restore: %w", err)
	}
	if !slices.Equal(m.Table, live) {
		return 0, fmt.Errorf("sandboxreset: restore: %w: captured %+v, live %+v", ErrMountsChanged, m.Table, live)
	}
	for _, l := range m.Mounts {
		n, err := pruneMount(ctx, l)
		removed += n
		if err != nil {
			return removed, fmt.Errorf("sandboxreset: restore %s: %w", l.Path, err)
		}
	}
	if err := verifyListings(ctx, m); err != nil {
		return removed, fmt.Errorf("sandboxreset: restore: %w", err)
	}
	return removed, nil
}

func verifyListings(ctx context.Context, m Manifest) error {
	for _, l := range m.Mounts {
		nested := map[string]bool{}
		for _, e := range l.Entries {
			if e.Type == TypeMountpoint {
				nested[e.Rel] = true
			}
		}
		got, err := listMount(ctx, l.Path, nested)
		if err != nil {
			return fmt.Errorf("%s: %w", l.Path, err)
		}
		extra, changed := diff(l.Entries, got)
		if len(changed) > 0 {
			return fmt.Errorf("%w: %s: %s", ErrUnrestorable, l.Path, summarize(changed))
		}
		if len(extra) > 0 {
			return fmt.Errorf("%w: %s: %s", ErrNotClean, l.Path, summarize(extra))
		}
	}
	return nil
}

func summarize(rels []string) string {
	const shown = 5
	if len(rels) <= shown {
		return strings.Join(rels, ", ")
	}
	return fmt.Sprintf("%s and %d more", strings.Join(rels[:shown], ", "), len(rels)-shown)
}

// pruneMount removes what l does not list from one mount.
func pruneMount(ctx context.Context, l MountListing) (int, error) {
	parent, base, err := openParent(l.Path)
	if err != nil {
		return 0, err
	}
	defer func() { _ = unix.Close(parent) }()
	var root unix.Stat_t
	if err := lstatAt(parent, base, &root); err != nil {
		return 0, fmt.Errorf("%w: mount root: %w", ErrUnrestorable, err)
	}
	p := pruner{ctx: ctx, rootDev: root.Dev, want: make(map[string]Entry, len(l.Entries))}
	for _, e := range l.Entries {
		p.want[e.Rel] = e
	}
	err = p.pruneListed(parent, base, ".", &root)
	return p.removed, err
}

type pruner struct {
	ctx     context.Context
	rootDev uint64
	want    map[string]Entry
	removed int
}

// pruneListed handles an entry the listing has. It rejects anything stat can
// already tell has changed, so a file a run grew is never hashed. Content and
// link targets are left to the verify walk.
func (p *pruner) pruneListed(parent int, name, rel string, st *unix.Stat_t) error {
	want := p.want[rel]
	if want.Type == TypeMountpoint {
		return nil
	}
	got, err := entryFromStat(rel, st)
	if err != nil {
		return err
	}
	want.SHA256, want.Target = "", ""
	if got != want {
		return fmt.Errorf("%w: %s", ErrUnrestorable, rel)
	}
	if got.Type != TypeDir {
		return nil
	}
	if st.Dev != p.rootDev {
		return fmt.Errorf("%w: %s", ErrUnlistedMount, rel)
	}

	fd, err := openDirAt(parent, name, st)
	if err != nil {
		return fmt.Errorf("%s: %w", rel, err)
	}
	defer func() { _ = unix.Close(fd) }()
	names, err := readNames(fd)
	if err != nil {
		return fmt.Errorf("%s: %w", rel, err)
	}
	for _, child := range names {
		if err := p.ctx.Err(); err != nil {
			return err
		}
		cr := childRel(rel, child)
		if _, listed := p.want[cr]; !listed {
			if err := p.remove(fd, child); err != nil {
				return fmt.Errorf("remove %q: %w", cr, err)
			}
			continue
		}
		var childSt unix.Stat_t
		if err := lstatAt(fd, child, &childSt); err != nil {
			return fmt.Errorf("%s: %w", cr, err)
		}
		if err := p.pruneListed(fd, child, cr, &childSt); err != nil {
			return err
		}
	}
	return nil
}

// remove deletes name from parent, and if it is a directory, everything in it.
// Every step is relative to a directory it already holds open, and a symlink is
// only ever unlinked. So nothing outside the mount can be reached, even if the
// tree is rearranged during the walk.
func (p *pruner) remove(parent int, name string) error {
	if err := p.ctx.Err(); err != nil {
		return err
	}
	var st unix.Stat_t
	if err := lstatAt(parent, name, &st); err != nil {
		return err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFDIR {
		if err := unix.Unlinkat(parent, name, 0); err != nil {
			return err
		}
		p.removed++
		return nil
	}
	if st.Dev != p.rootDev {
		return ErrUnlistedMount
	}

	fd, err := openForRemoval(parent, name, &st)
	if err != nil {
		return err
	}
	defer func() { _ = unix.Close(fd) }()
	names, err := readNames(fd)
	if err != nil {
		return err
	}
	for _, child := range names {
		if err := p.remove(fd, child); err != nil {
			return fmt.Errorf("%q: %w", child, err)
		}
	}
	if err := unix.Unlinkat(parent, name, unix.AT_REMOVEDIR); err != nil {
		return err
	}
	p.removed++
	return nil
}

// openForRemoval opens a directory of any mode, including 000, after making it
// 0700. fchmodat(2) follows symlinks and fchmodat2 may be missing under gVisor,
// so the chmod goes through /proc to an O_PATH handle that O_NOFOLLOW already
// pinned to this directory.
func openForRemoval(parent int, name string, st *unix.Stat_t) (int, error) {
	handle, err := unix.Openat(parent, name, unix.O_PATH|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, err
	}
	defer func() { _ = unix.Close(handle) }()
	if err := checkSameObject(handle, st); err != nil {
		return -1, err
	}
	if err := unix.Chmod("/proc/self/fd/"+strconv.Itoa(handle), 0o700); err != nil {
		return -1, fmt.Errorf("chmod: %w", err)
	}
	return unix.Openat(handle, ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
}
