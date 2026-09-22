package snapshot

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// buildTar writes the given entries (in order) into a raw tar.
func buildTar(t *testing.T, entries []tar.Header) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, hdr := range entries {
		h := hdr
		if h.Typeflag == tar.TypeReg {
			h.Size = int64(len(h.Name))
		}
		if err := tw.WriteHeader(&h); err != nil {
			t.Fatalf("write header %q: %v", h.Name, err)
		}
		if h.Typeflag == tar.TypeReg {
			if _, err := tw.Write([]byte(h.Name)); err != nil {
				t.Fatalf("write body %q: %v", h.Name, err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	return &buf
}

func setupDirs(t *testing.T) (Folders, string) {
	t.Helper()
	base := t.TempDir()
	dirs := Folders{
		WorkspaceDir: filepath.Join(base, "workspace"),
		HomeDir:      filepath.Join(base, "home"),
	}
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return dirs, base
}

func TestArchiveRestoreRoundTrip(t *testing.T) {
	src, _ := setupDirs(t)
	if err := os.MkdirAll(filepath.Join(src[WorkspaceDir], "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		// {bucket}/{relative path} -> content
		WorkspaceDir + "/main.py":         "print('hi')",
		WorkspaceDir + "/nested/data.txt": "data",
		HomeDir + "/.profile":             "export X=1",
	}
	for name, body := range files {
		bucket, rel, _ := strings.Cut(name, "/")
		p := filepath.Join(src[bucket], filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o640); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("main.py", filepath.Join(src[WorkspaceDir], "link.py")); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	n, err := Archive(&buf, src)
	if err != nil {
		t.Fatalf("archive: %v", err)
	}
	if n == 0 {
		t.Fatal("archive: no bytes written")
	}

	dst, _ := setupDirs(t)
	if err := Restore(&buf, dst); err != nil {
		t.Fatalf("restore: %v", err)
	}
	for name, body := range files {
		bucket, rel, _ := strings.Cut(name, "/")
		got, err := os.ReadFile(filepath.Join(dst[bucket], filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("restored %s: %v", name, err)
		}
		if string(got) != body {
			t.Fatalf("restored %s = %q, want %q", name, got, body)
		}
	}
	link, err := os.Readlink(filepath.Join(dst[WorkspaceDir], "link.py"))
	if err != nil || link != "main.py" {
		t.Fatalf("restored symlink = %q (%v), want main.py", link, err)
	}
}

// TestRestoreRefusesEscape walks the archive attacks a malicious workload
// would mount: every one of them must fail loudly and leave nothing outside the
// target directories. The boundary matters because a fork restores workload-
// authored data into a fresh sandbox directory — a single accepted entry would
// write anywhere the runner can.
func TestRestoreRefusesEscape(t *testing.T) {
	outside := map[string][]tar.Header{
		"absolute path": {
			{Name: "/etc/passwd", Typeflag: tar.TypeReg, Mode: 0o644},
		},
		"bucket outside snapshot": {
			{Name: "../evil", Typeflag: tar.TypeReg, Mode: 0o644},
		},
		"symlink pointing outside": {
			{Name: WorkspaceDir + "/link", Typeflag: tar.TypeSymlink, Linkname: "../../../etc", Mode: 0o777},
		},
		"absolute symlink": {
			{Name: WorkspaceDir + "/link", Typeflag: tar.TypeSymlink, Linkname: "/etc", Mode: 0o777},
		},
		"write through symlinked dir": {
			{Name: WorkspaceDir + "/d", Typeflag: tar.TypeSymlink, Linkname: "x", Mode: 0o777},
			{Name: WorkspaceDir + "/d/file", Typeflag: tar.TypeReg, Mode: 0o644},
		},
		"hardlink": {
			{Name: WorkspaceDir + "/l", Typeflag: tar.TypeLink, Linkname: "f", Mode: 0o644},
		},
		"device node": {
			{Name: WorkspaceDir + "/dev", Typeflag: tar.TypeChar, Mode: 0o644},
		},
	}

	for name, entries := range outside {
		t.Run(name, func(t *testing.T) {
			dst, base := setupDirs(t)
			err := Restore(buildTar(t, entries), dst)
			if err == nil {
				t.Fatal("restore: accepted a hostile entry, want error")
			}
			// Nothing may have landed beside the target directories.
			entries, readErr := os.ReadDir(filepath.Dir(dst[WorkspaceDir]))
			if readErr != nil {
				t.Fatal(readErr)
			}
			for _, e := range entries {
				if e.Name() != WorkspaceDir && e.Name() != HomeDir {
					t.Fatalf("hostile entry created %q outside the targets (base %s)", e.Name(), base)
				}
			}
		})
	}
}

// TestRestoreReplacesLeafSymlinks proves a later entry cannot write *through* a
// symlink an earlier entry placed at the same path: the symlink is replaced,
// not followed.
func TestRestoreReplacesLeafSymlinks(t *testing.T) {
	dst, _ := setupDirs(t)
	if err := os.WriteFile(filepath.Join(dst[HomeDir], "victim"), []byte("safe"), 0o644); err != nil {
		t.Fatal(err)
	}
	entries := []tar.Header{
		{Name: WorkspaceDir + "/f", Typeflag: tar.TypeSymlink, Linkname: "../home/victim", Mode: 0o777},
	}
	// The symlink itself points at its own directory root's sibling; reject it
	// up front so the write-through case is built from an in-root link instead.
	if err := Restore(buildTar(t, entries), dst); err == nil {
		t.Fatal("restore: accepted an escaping symlink")
	}

	entries = []tar.Header{
		{Name: WorkspaceDir + "/f", Typeflag: tar.TypeSymlink, Linkname: "g", Mode: 0o777},
		{Name: WorkspaceDir + "/f", Typeflag: tar.TypeReg, Mode: 0o644},
	}
	if err := Restore(buildTar(t, entries), dst); err != nil {
		t.Fatalf("restore: %v", err)
	}
	fi, err := os.Lstat(filepath.Join(dst[WorkspaceDir], "f"))
	if err != nil {
		t.Fatal(err)
	}
	if !fi.Mode().IsRegular() {
		t.Fatalf("f = %v, want a regular file (symlink replaced, not followed)", fi.Mode())
	}
	victim, err := os.ReadFile(filepath.Join(dst[HomeDir], "victim"))
	if err != nil || string(victim) != "safe" {
		t.Fatalf("victim = %q (%v), want untouched", victim, err)
	}
}
