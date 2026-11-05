//go:build linux

package sandboxreset

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"slices"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

// Capture lists every writable mount in the table mountinfo holds, except /proc
// and /sys. It fails if "/" is writable. Call it before the first workload runs,
// so the listing is the start state.
func Capture(ctx context.Context, mountinfo io.Reader) (Manifest, error) {
	table, err := parseMountinfo(mountinfo)
	if err != nil {
		return Manifest{}, fmt.Errorf("sandboxreset: capture: %w", err)
	}
	plan, err := selectMounts(table)
	if err != nil {
		return Manifest{}, fmt.Errorf("sandboxreset: capture: %w", err)
	}
	m := Manifest{Table: table}
	for _, p := range plan {
		entries, err := listMount(ctx, p.Path, p.Nested)
		if err != nil {
			return Manifest{}, fmt.Errorf("sandboxreset: capture %s: %w", p.Path, err)
		}
		m.Mounts = append(m.Mounts, MountListing{Path: p.Path, Entries: entries})
	}
	// A mount in the table that the walk did not reach, such as one covered by a
	// later mount, fails here.
	if err := m.validate(); err != nil {
		return Manifest{}, fmt.Errorf("sandboxreset: capture: %w", err)
	}
	return m, nil
}

// listMount walks one mount. nested holds the relative paths of the mounts
// inside it, which it records and does not enter.
func listMount(ctx context.Context, mountPath string, nested map[string]bool) ([]Entry, error) {
	parent, base, err := openParent(mountPath)
	if err != nil {
		return nil, err
	}
	defer func() { _ = unix.Close(parent) }()
	var root unix.Stat_t
	if err := lstatAt(parent, base, &root); err != nil {
		return nil, err
	}
	w := walker{ctx: ctx, nested: nested, rootDev: root.Dev}
	if err := w.visit(parent, base, "."); err != nil {
		return nil, err
	}
	sort.Slice(w.entries, func(i, j int) bool { return w.entries[i].Rel < w.entries[j].Rel })
	return w.entries, nil
}

type walker struct {
	ctx     context.Context
	nested  map[string]bool
	rootDev uint64
	entries []Entry
}

func (w *walker) visit(parent int, name, rel string) error {
	if err := w.ctx.Err(); err != nil {
		return err
	}
	var st unix.Stat_t
	if err := lstatAt(parent, name, &st); err != nil {
		return fmt.Errorf("%s: %w", shortRel(rel), err)
	}
	if w.nested[rel] {
		w.entries = append(w.entries, Entry{Rel: rel, Type: TypeMountpoint})
		return nil
	}
	e, err := entryFromStat(rel, &st)
	if err != nil {
		return err
	}
	switch e.Type {
	case TypeSymlink:
		if e.Target, err = readlinkAt(parent, name); err != nil {
			return fmt.Errorf("%s: %w", shortRel(rel), err)
		}
		// JSON would hand back a different target from the host.
		if !utf8.ValidString(e.Target) {
			return fmt.Errorf("%s: link target %q is not valid UTF-8", shortRel(rel), e.Target)
		}
	case TypeFile:
		if e.SHA256, err = hashFileAt(parent, name, &st); err != nil {
			return fmt.Errorf("%s: %w", shortRel(rel), err)
		}
	}
	if e.Xattrs, err = xattrDigestAt(parent, name, &st); err != nil {
		return fmt.Errorf("%s: %w", shortRel(rel), err)
	}
	w.entries = append(w.entries, e)
	if e.Type != TypeDir {
		return nil
	}

	if st.Dev != w.rootDev {
		return fmt.Errorf("%w: %s", ErrUnlistedMount, shortRel(rel))
	}
	fd, err := openDirAt(parent, name, &st)
	if err != nil {
		return fmt.Errorf("%s: %w", shortRel(rel), err)
	}
	defer func() { _ = unix.Close(fd) }()
	names, err := readNames(fd)
	if err != nil {
		return fmt.Errorf("%s: %w", shortRel(rel), err)
	}
	for _, child := range names {
		if err := w.visit(fd, child, childRel(rel, child)); err != nil {
			return err
		}
	}
	return nil
}

func childRel(rel, name string) string {
	if rel == "." {
		return name
	}
	return rel + "/" + name
}

// shortRel bounds a path in an error. A planted tree decides how deep a path can
// be, and it must not decide how long a line in the reset log is.
func shortRel(rel string) string {
	const limit = 256
	if len(rel) <= limit {
		return rel
	}
	return "..." + rel[len(rel)-limit:]
}

func entryFromStat(rel string, st *unix.Stat_t) (Entry, error) {
	e := Entry{Rel: rel, Mode: st.Mode & 0o7777, UID: st.Uid, GID: st.Gid, Ino: st.Ino}
	switch st.Mode & unix.S_IFMT {
	case unix.S_IFDIR:
		e.Type = TypeDir
	case unix.S_IFREG:
		e.Type = TypeFile
		e.Size = st.Size
	case unix.S_IFLNK:
		e.Type = TypeSymlink
	case unix.S_IFCHR:
		e.Type = TypeCharDev
		e.Rdev = st.Rdev
	case unix.S_IFBLK:
		e.Type = TypeBlockDev
		e.Rdev = st.Rdev
	case unix.S_IFSOCK:
		e.Type = TypeSocket
	case unix.S_IFIFO:
		e.Type = TypeFIFO
	default:
		return Entry{}, fmt.Errorf("%s: unknown file type %#o", shortRel(rel), st.Mode&unix.S_IFMT)
	}
	return e, nil
}

// openParent opens the directory holding mountPath one component at a time,
// without following a symlink. The path came from mountinfo, which is canonical,
// so a symlink on the way means the tree was rearranged since then.
func openParent(mountPath string) (fd int, base string, err error) {
	fd, err = unix.Open("/", unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, "", fmt.Errorf("open /: %w", err)
	}
	for c := range strings.SplitSeq(strings.Trim(path.Dir(mountPath), "/"), "/") {
		if c == "" {
			continue
		}
		next, err := unix.Openat(fd, c, unix.O_PATH|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		_ = unix.Close(fd)
		if err != nil {
			return -1, "", fmt.Errorf("open %s: component %q: %w", mountPath, c, err)
		}
		fd = next
	}
	return fd, path.Base(mountPath), nil
}

func lstatAt(dirfd int, name string, st *unix.Stat_t) error {
	return unix.Fstatat(dirfd, name, st, unix.AT_SYMLINK_NOFOLLOW)
}

// openDirAt opens the directory st described, and fails if name is now anything
// else.
func openDirAt(parent int, name string, st *unix.Stat_t) (int, error) {
	fd, err := unix.Openat(parent, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, err
	}
	if err := checkSameObject(fd, st); err != nil {
		_ = unix.Close(fd)
		return -1, err
	}
	return fd, nil
}

func checkSameObject(fd int, st *unix.Stat_t) error {
	var opened unix.Stat_t
	if err := unix.Fstat(fd, &opened); err != nil {
		return err
	}
	if opened.Dev != st.Dev || opened.Ino != st.Ino || opened.Mode&unix.S_IFMT != st.Mode&unix.S_IFMT {
		return errors.New("object was replaced while being opened")
	}
	return nil
}

func readNames(fd int) ([]string, error) {
	buf := make([]byte, 8*1024)
	var names []string
	for {
		n, err := unix.ReadDirent(fd, buf)
		if err != nil {
			return nil, err
		}
		if n == 0 {
			return names, nil
		}
		_, _, names = unix.ParseDirent(buf[:n], -1, names)
	}
}

func readlinkAt(dirfd int, name string) (string, error) {
	buf := make([]byte, unix.PathMax)
	n, err := unix.Readlinkat(dirfd, name, buf)
	if err != nil {
		return "", err
	}
	return string(buf[:n]), nil
}

// xattrDigestAt digests the extended attributes of what st described. O_PATH needs
// no permission on the object and never reaches a device's driver, so this works on
// a mode-000 /dev/pts/ptmx and does not open /dev/tty. It is pinned against a swap
// like every other open here.
func xattrDigestAt(parent int, name string, st *unix.Stat_t) (string, error) {
	fd, err := unix.Openat(parent, name, unix.O_PATH|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return "", err
	}
	defer func() { _ = unix.Close(fd) }()
	if err := checkSameObject(fd, st); err != nil {
		return "", err
	}
	return xattrDigest(procFD(fd))
}

// xattrDigest hashes the sorted names and values of an object's attributes, and
// is empty when it has none. Each is length-prefixed, so that bytes cannot move
// from one to the next and hash the same.
//
// The calls follow the /proc link on purpose: the kernel resolves it to the pinned
// object and stops there, even if that object is a symlink. llistxattr would read
// the /proc link itself and always find nothing. Nothing else runs while this does,
// so an attribute changing between two calls is an error and not a retry.
func xattrDigest(proc string) (string, error) {
	size, err := unix.Listxattr(proc, nil)
	// The kernel says this for a filesystem or file type with no attributes at all.
	// Once a name is listed, the same error from reading it is a failure.
	if errors.Is(err, unix.ENOTSUP) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("list xattrs: %w", err)
	}
	if size == 0 {
		return "", nil
	}
	list := make([]byte, size)
	if size, err = unix.Listxattr(proc, list); err != nil {
		return "", fmt.Errorf("list xattrs: %w", err)
	}
	names := strings.Split(strings.TrimSuffix(string(list[:size]), "\x00"), "\x00")
	slices.Sort(names)

	h := sha256.New()
	for _, name := range names {
		value, err := readXattr(proc, name)
		if err != nil {
			return "", fmt.Errorf("read xattr %q: %w", name, err)
		}
		for _, part := range [][]byte{[]byte(name), value} {
			h.Write(binary.BigEndian.AppendUint64(nil, uint64(len(part))))
			h.Write(part)
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func readXattr(proc, name string) ([]byte, error) {
	size, err := unix.Getxattr(proc, name, nil)
	if err != nil || size == 0 {
		return nil, err
	}
	value := make([]byte, size)
	size, err = unix.Getxattr(proc, name, value)
	if err != nil {
		return nil, err
	}
	return value[:size], nil
}

func procFD(fd int) string {
	return "/proc/self/fd/" + strconv.Itoa(fd)
}

func hashFileAt(parent int, name string, st *unix.Stat_t) (string, error) {
	// O_NONBLOCK so a FIFO swapped in cannot hang the walk before the check below.
	fd, err := unix.Openat(parent, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		return "", err
	}
	f := os.NewFile(uintptr(fd), name)
	defer func() { _ = f.Close() }()
	if err := checkSameObject(fd, st); err != nil {
		return "", err
	}
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", err
	}
	if n != st.Size {
		return "", fmt.Errorf("read %d bytes, stat said %d", n, st.Size)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
