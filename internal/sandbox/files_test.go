package sandbox

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/vorash/vora/internal/runtime"
)

func testWorkspace(t *testing.T) runtime.WorkspaceSpec {
	t.Helper()
	root := t.TempDir()
	spec := runtime.WorkspaceSpec{
		RootDir:      root,
		WorkspaceDir: filepath.Join(root, "workspace"),
		HomeDir:      filepath.Join(root, "home"),
	}
	for _, d := range []string{spec.WorkspaceDir, spec.HomeDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return spec
}

func TestResolveFilePath(t *testing.T) {
	spec := testWorkspace(t)
	cases := []struct {
		path    string
		wantRel string
		wantErr error
	}{
		{path: "/workspace/file.txt", wantRel: "file.txt"},
		{path: "/workspace/a/b/file.txt", wantRel: "a/b/file.txt"},
		{path: "/root/.bashrc", wantRel: ".bashrc"},
		{path: "/workspace", wantRel: "."},
		{path: "/tmp/x", wantErr: ErrPathOutsideWritableRoots},
		{path: "/etc/passwd", wantErr: ErrPathOutsideWritableRoots},
		{path: "/workspace/../etc/passwd", wantErr: ErrPathOutsideWritableRoots},
		{path: "", wantErr: nil},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			_, rel, err := ResolveFilePath(spec, tc.path)
			if tc.path == "" {
				if err == nil {
					t.Fatal("want error for an empty path")
				}
				return
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("want err %v, got %v", tc.wantErr, err)
			}
			if tc.wantErr == nil && rel != tc.wantRel {
				t.Fatalf("want rel %q, got %q", tc.wantRel, rel)
			}
		})
	}
}

func TestResolveFilePathRejectsRelative(t *testing.T) {
	spec := testWorkspace(t)
	if _, _, err := ResolveFilePath(spec, "relative.txt"); err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("want absolute-path error, got %v", err)
	}
}

func TestWriteReadFileRoundTrip(t *testing.T) {
	spec := testWorkspace(t)
	content := []byte("hello sandbox\n")

	n, err := WriteFile(spec, "/workspace/dir/file.txt", content, true)
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if n != int64(len(content)) {
		t.Fatalf("wrote %d, want %d", n, len(content))
	}
	got, err := ReadFile(spec, "/workspace/dir/file.txt")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != string(content) {
		t.Fatalf("read %q, want %q", got, content)
	}
	// The bytes must land on the host workspace directory, not somewhere else.
	if _, err := os.Stat(filepath.Join(spec.WorkspaceDir, "dir", "file.txt")); err != nil {
		t.Fatalf("expected host file: %v", err)
	}
}

func TestWriteFileWithoutParentsFails(t *testing.T) {
	spec := testWorkspace(t)
	if _, err := WriteFile(spec, "/workspace/missing/file.txt", []byte("x"), false); err == nil {
		t.Fatal("want error when the parent directory is missing")
	}
}

func TestWriteFileRejectsOversize(t *testing.T) {
	spec := testWorkspace(t)
	_, err := WriteFile(spec, "/workspace/big", make([]byte, MaxTransferBytes+1), true)
	if !errors.Is(err, ErrFileTooLarge) {
		t.Fatalf("want ErrFileTooLarge, got %v", err)
	}
}

func TestReadFileRejectsOversize(t *testing.T) {
	spec := testWorkspace(t)
	if err := os.WriteFile(filepath.Join(spec.WorkspaceDir, "big"), make([]byte, MaxTransferBytes+1), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := ReadFile(spec, "/workspace/big")
	if !errors.Is(err, ErrFileTooLarge) {
		t.Fatalf("want ErrFileTooLarge, got %v", err)
	}
}

func TestFilesAPIRefusesFIFOWithoutBlocking(t *testing.T) {
	spec := testWorkspace(t)
	if err := syscall.Mkfifo(filepath.Join(spec.WorkspaceDir, "pipe"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, call := range map[string]func() error{
		"read":  func() error { _, err := ReadFile(spec, "/workspace/pipe"); return err },
		"write": func() error { _, err := WriteFile(spec, "/workspace/pipe", []byte("x"), false); return err },
	} {
		t.Run(name, func(t *testing.T) {
			done := make(chan error, 1)
			go func() { done <- call() }()
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("FIFO accepted by files API")
				}
			case <-time.After(time.Second):
				t.Fatal("files API blocked on workload-created FIFO")
			}
		})
	}
}

func TestReadFileRefusesSymlinkEscape(t *testing.T) {
	spec := testWorkspace(t)
	secret := filepath.Join(spec.RootDir, "secret")
	if err := os.WriteFile(secret, []byte("host secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(spec.WorkspaceDir, "escape")); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadFile(spec, "/workspace/escape"); err == nil {
		t.Fatal("want symlink escape to be refused")
	}
}

func TestWriteFileRefusesSymlinkEscape(t *testing.T) {
	spec := testWorkspace(t)
	outside := filepath.Join(spec.RootDir, "outside")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(spec.WorkspaceDir, "link")); err != nil {
		t.Fatal(err)
	}
	if _, err := WriteFile(spec, "/workspace/link/owned", []byte("x"), true); err == nil {
		t.Fatal("want symlink escape to be refused")
	}
	if _, err := os.Stat(filepath.Join(outside, "owned")); err == nil {
		t.Fatal("write escaped the writable root")
	}
}
