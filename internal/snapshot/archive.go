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
	cw := &countingWriter{w: w}
	tw := tar.NewWriter(cw)

	names := make([]string, 0, len(dirs))
	for name := range dirs {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		root := dirs[name]
		err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return skipVanished(err)
			}
			rel, err := filepath.Rel(root, p)
			if err != nil {
				return err
			}
			if rel == "." {
				return nil
			}
			info, err := d.Info()
			if err != nil {
				return skipVanished(err)
			}
			link := ""
			if info.Mode()&os.ModeSymlink != 0 {
				if link, err = os.Readlink(p); err != nil {
					return skipVanished(err)
				}
			}
			hdr, err := tar.FileInfoHeader(info, link)
			if err != nil {
				return err
			}
			hdr.Name = path.Join(name, filepath.ToSlash(rel))
			if err := tw.WriteHeader(hdr); err != nil {
				return err
			}
			if !info.Mode().IsRegular() {
				return nil
			}
			f, err := os.Open(p)
			if err != nil {
				return skipVanished(err)
			}
			defer f.Close()
			n, err := io.Copy(tw, io.LimitReader(f, hdr.Size))
			if err != nil {
				return err
			}
			for pad := hdr.Size - n; pad > 0; pad-- {
				if _, err := tw.Write([]byte{0}); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			return 0, err
		}
	}
	if err := tw.Close(); err != nil {
		return 0, err
	}
	return cw.n, nil
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
	tr := tar.NewReader(r)
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
