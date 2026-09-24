package snapshot

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
)

// Logical directory names inside a snapshot archive and a restored sandbox
// root. Archives never contain host paths.
const (
	WorkspaceDir = "workspace"
	HomeDir      = "home"
)

// Folders maps a logical directory name to the host directory to archive or
// restore into.
type Folders map[string]string

// Archive writes a tar of dirs into w and returns the number of bytes written.
//
// The copy is crash-consistent: the sandbox may keep writing while it runs, so
// files may tear (a truncated tail) and files created or removed mid-walk may
// be missed. A file that shrinks mid-copy is zero-padded to keep the archive
// structurally valid.
func Archive(w io.Writer, dirs Folders) (int64, error) {
	return archive(w, dirs, nil, 0)
}

// ArchiveLimited aborts before an archive can consume more than maxBytes.
func ArchiveLimited(w io.Writer, dirs Folders, maxBytes int64) (int64, error) {
	if maxBytes <= 0 {
		return 0, fmt.Errorf("snapshot: archive limit must be positive")
	}
	return archive(w, dirs, nil, maxBytes)
}

func archiveWithHook(w io.Writer, dirs Folders, beforeOpen func(bucket, rel string)) (int64, error) {
	return archive(w, dirs, beforeOpen, 0)
}

func archive(w io.Writer, dirs Folders, beforeOpen func(bucket, rel string), maxBytes int64) (int64, error) {
	cw := &countingWriter{w: w}
	var output io.Writer = cw
	if maxBytes > 0 {
		output = &boundedWriter{w: cw, max: maxBytes}
	}
	tw := tar.NewWriter(output)
	names := make([]string, 0, len(dirs))
	for name := range dirs {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		root, err := os.OpenRoot(dirs[name])
		if err != nil {
			return 0, err
		}
		err = archiveDir(tw, root, name, ".", beforeOpen)
		root.Close()
		if err != nil {
			return 0, err
		}
	}
	if err := tw.Close(); err != nil {
		return 0, err
	}
	return cw.n, nil
}

func archiveDir(tw *tar.Writer, root *os.Root, bucket, dir string, beforeOpen func(bucket, rel string)) error {
	f, err := root.OpenFile(dir, os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return skipVanished(err)
	}
	entries, err := f.ReadDir(-1)
	f.Close()
	if err != nil {
		return err
	}
	for _, entry := range entries {
		rel := filepath.Join(dir, entry.Name())
		if beforeOpen != nil {
			beforeOpen(bucket, rel)
		}
		info, err := root.Lstat(rel)
		if err != nil {
			if err := skipVanished(err); err != nil {
				return err
			}
			continue
		}
		if info.Mode().Type() != entry.Type().Type() {
			continue
		}
		var link string
		if info.Mode()&os.ModeSymlink != 0 {
			link, err = root.Readlink(rel)
			if err != nil {
				if err := skipVanished(err); err != nil {
					return err
				}
				continue
			}
		}
		var opened *os.File
		if info.IsDir() || info.Mode().IsRegular() {
			flags := os.O_RDONLY | syscall.O_NONBLOCK | syscall.O_NOFOLLOW
			if info.IsDir() {
				flags |= syscall.O_DIRECTORY
			}
			opened, err = root.OpenFile(rel, flags, 0)
			if err != nil {
				if errors.Is(err, syscall.ELOOP) || errors.Is(err, fs.ErrNotExist) {
					continue
				}
				return err
			}
			actual, statErr := opened.Stat()
			if statErr != nil || actual.Mode().Type() != info.Mode().Type() {
				opened.Close()
				if statErr != nil {
					return statErr
				}
				continue
			}
			info = actual
		}
		hdr, err := tar.FileInfoHeader(info, link)
		if err == nil {
			hdr.Name = path.Join(bucket, filepath.ToSlash(rel))
			err = tw.WriteHeader(hdr)
		}
		if err == nil && info.IsDir() {
			err = archiveDir(tw, root, bucket, rel, beforeOpen)
		}
		if err == nil && info.Mode().IsRegular() {
			var n int64
			n, err = io.Copy(tw, io.LimitReader(opened, hdr.Size))
			for pad := hdr.Size - n; err == nil && pad > 0; pad-- {
				_, err = tw.Write([]byte{0})
			}
		}
		if opened != nil {
			opened.Close()
		}
		if err != nil {
			return err
		}
	}
	return nil
}

type boundedWriter struct {
	w   io.Writer
	max int64
	n   int64
}

func (b *boundedWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > b.max-b.n {
		return 0, fmt.Errorf("snapshot: archive exceeds %d bytes", b.max)
	}
	n, err := b.w.Write(p)
	b.n += int64(n)
	return n, err
}

// skipVanished lets Archive be crash-consistent: an entry that disappears
// between walking and reading is skipped instead of failing the snapshot.
func skipVanished(err error) error {
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// Restore extracts a snapshot tar into dirs.
//
// A snapshot is untrusted input (the workload authored it), so extraction
// never writes outside the target directories and never follows a symlink:
// paths that are absolute or traverse upwards are refused or contained,
// symlinked path components are refused, and leaves are replaced rather than
// written through. Symlinks are restored verbatim — an escaping link is inert
// data the workload could create at runtime anyway. Non regular/dir/symlink
// entries (hardlinks, devices, FIFOs) fail loudly. Only permission bits are
// restored — ownership and setuid bits are not (the runner is unprivileged and
// setuid in a snapshot is a footgun).
func Restore(r io.Reader, dirs Folders) error {
	return restore(r, dirs, 0)
}

// RestoreLimited refuses archives whose regular-file content exceeds maxBytes.
func RestoreLimited(r io.Reader, dirs Folders, maxBytes int64) error {
	if maxBytes <= 0 {
		return fmt.Errorf("snapshot: restore limit must be positive")
	}
	return restore(r, dirs, maxBytes)
}

func restore(r io.Reader, dirs Folders, maxBytes int64) error {
	tr := tar.NewReader(r)
	var restored int64
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("snapshot: read archive: %w", err)
		}
		root, rel, err := splitEntry(dirs, hdr.Name)
		if err != nil {
			return err
		}
		target := filepath.Join(root, rel)
		if err := safePath(root, target); err != nil {
			return err
		}
		// safePath has vetted every component up to the first missing one,
		// so creating the parents cannot escape or traverse a symlink.
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			if fi, err := os.Lstat(target); err == nil && fi.IsDir() {
				continue
			}
			if err := replaceLeaf(target, func() error {
				return os.Mkdir(target, hdr.FileInfo().Mode().Perm()&0o777)
			}); err != nil {
				return err
			}
		case tar.TypeReg:
			if maxBytes > 0 && (hdr.Size < 0 || hdr.Size > maxBytes-restored) {
				return fmt.Errorf("snapshot: restored files exceed %d bytes", maxBytes)
			}
			restored += hdr.Size
			if err := replaceLeaf(target, func() error {
				f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, hdr.FileInfo().Mode().Perm()&0o777)
				if err != nil {
					return err
				}
				defer f.Close()
				_, err = io.Copy(f, tr)
				return err
			}); err != nil {
				return err
			}
		case tar.TypeSymlink:
			if err := replaceLeaf(target, func() error {
				return os.Symlink(hdr.Linkname, target)
			}); err != nil {
				return err
			}
		default:
			return fmt.Errorf("snapshot: unsupported entry %q (type %v)", hdr.Name, hdr.Typeflag)
		}
	}
}

// splitEntry validates an archive path and splits it into its target root and
// root-relative path.
func splitEntry(dirs Folders, name string) (root, rel string, err error) {
	name = strings.TrimPrefix(filepath.ToSlash(name), "/")
	bucket, rest, ok := strings.Cut(name, "/")
	if !ok {
		return "", "", fmt.Errorf("snapshot: malformed entry %q", name)
	}
	root, ok = dirs[bucket]
	if !ok {
		return "", "", fmt.Errorf("snapshot: entry %q outside snapshot", name)
	}
	cleaned := path.Clean("/" + rest)
	if cleaned == "/" {
		return "", "", fmt.Errorf("snapshot: malformed entry %q", name)
	}
	rel = filepath.FromSlash(strings.TrimPrefix(cleaned, "/"))
	return root, rel, nil
}

// safePath rejects a target that escapes root or whose parent path would be
// traversed through a symlink. The leaf itself is not vetted: replaceLeaf
// replaces it without ever following it.
func safePath(root, target string) error {
	rel, err := filepath.Rel(root, target)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("snapshot: entry escapes target directory: %q", target)
	}
	parts := strings.Split(rel, string(filepath.Separator))
	cur := root
	for _, part := range parts[:len(parts)-1] {
		cur = filepath.Join(cur, part)
		fi, err := os.Lstat(cur)
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("snapshot: entry %q traverses a symlink", rel)
		}
	}
	return nil
}

// replaceLeaf creates fn's entry at target without ever following an existing
// leaf (which may be a symlink the archive placed earlier).
func replaceLeaf(target string, fn func() error) error {
	if err := os.Remove(target); err != nil && !os.IsNotExist(err) {
		return err
	}
	return fn()
}

type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}
