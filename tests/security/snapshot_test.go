//go:build security

package security

import (
	"archive/tar"
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vorash/vora/internal/runtime"
	"github.com/vorash/vora/internal/snapshot"
)

// snapshotFolders maps a sandbox's writable roots the way the runner does when
// it snapshots (ADR 023: workspace and home; /tmp is a tmpfs by design).
func snapshotFolders(ws runtime.WorkspaceSpec) snapshot.Folders {
	return snapshot.Folders{
		snapshot.WorkspaceDir: ws.WorkspaceDir,
		snapshot.HomeDir:      ws.HomeDir,
	}
}

// TestForkedSandboxesAreIndependent asserts the fan-out contract on real
// sandboxes: a fork reproduces the source's file state as of the copy, and
// every fork owns its writes from its first one. Shared state would let
// colluding or escaping workloads across forks read and poison each other.
func TestForkedSandboxesAreIndependent(t *testing.T) {
	forEachBackend(t, func(t *testing.T, f *fixture) {
		srcID := "sbx_snap_src"
		srcH := f.create(srcID, limits(64, 32))
		if _, _, code := f.exec(srcH, "sh", "-c",
			"echo shared > /workspace/seen.txt && echo only-in-source > /workspace/source.txt"); code != 0 {
			t.Fatalf("setup exec exit = %d", code)
		}

		srcWS := f.ws.Paths(srcID)
		var archive bytes.Buffer
		if _, err := snapshot.Archive(&archive, snapshotFolders(srcWS)); err != nil {
			t.Fatalf("archive: %v", err)
		}

		// State written after the copy must not appear in any fork.
		if _, _, code := f.exec(srcH, "sh", "-c", "echo later > /workspace/later.txt"); code != 0 {
			t.Fatalf("post-snapshot exec exit = %d", code)
		}

		forkWS := map[string]runtime.WorkspaceSpec{}
		for _, id := range []string{"sbx_snap_fork1", "sbx_snap_fork2"} {
			ws := f.ws.Paths(id)
			if _, err := f.ws.Create(ws); err != nil {
				t.Fatal(err)
			}
			if err := snapshot.Restore(bytes.NewReader(archive.Bytes()), snapshotFolders(ws)); err != nil {
				t.Fatalf("restore %s: %v", id, err)
			}
			forkWS[id] = ws
		}

		h1 := f.create("sbx_snap_fork1", limits(64, 32))
		h2 := f.create("sbx_snap_fork2", limits(64, 32))

		for name, h := range map[string]runtime.Handle{"fork1": h1, "fork2": h2} {
			out, _, code := f.exec(h, "cat", "/workspace/seen.txt")
			if code != 0 || strings.TrimSpace(out) != "shared" {
				t.Fatalf("%s sees %q (exit %d), want %q", name, out, code, "shared")
			}
			if _, _, code := f.exec(h, "test", "!", "-f", "/workspace/later.txt"); code != 0 {
				t.Fatalf("%s sees state written after the snapshot", name)
			}
		}

		// One fork's writes are invisible to its sibling and to the source.
		if _, _, code := f.exec(h1, "sh", "-c", "echo poisoned > /workspace/marker.txt"); code != 0 {
			t.Fatalf("fork1 write exit = %d", code)
		}
		for name, h := range map[string]runtime.Handle{"fork2": h2, "source": srcH} {
			if _, _, code := f.exec(h, "test", "!", "-f", "/workspace/marker.txt"); code != 0 {
				t.Fatalf("%s sees fork1's writes", name)
			}
		}
	})
}

// TestSnapshotRestoreCannotEscape is the hostile-snapshot boundary: everything
// in a snapshot was authored by untrusted workload code. A snapshot whose
// entries try to write through its own symlinks must fail the restore loudly
// and leave nothing on the host outside the target directories — otherwise a
// crafted snapshot becomes arbitrary host file write at fork time.
func TestSnapshotRestoreCannotEscape(t *testing.T) {
	hostTarget := filepath.Join(os.TempDir(), fmt.Sprintf("host-pwned-%d", os.Getpid()))
	t.Cleanup(func() { _ = os.Remove(hostTarget) })

	forEachBackend(t, func(t *testing.T, f *fixture) {
		srcID := "sbx_snap_evil"
		srcH := f.create(srcID, limits(64, 32))
		// The workload authors an escaping symlink in its own state — legal
		// inside the sandbox, where "/" is the sandbox's own rootfs.
		if _, _, code := f.exec(srcH, "sh", "-c",
			fmt.Sprintf("ln -s %s /workspace/evil && echo pwn > /workspace/normal.txt", filepath.Dir(hostTarget))); code != 0 {
			t.Fatalf("setup exec exit = %d", code)
		}

		srcWS := f.ws.Paths(srcID)
		var buf bytes.Buffer
		if _, err := snapshot.Archive(&buf, snapshotFolders(srcWS)); err != nil {
			t.Fatalf("archive: %v", err)
		}

		// A tampered snapshot adds one entry that writes *through* the link.
		// Lexically inside the workspace, resolved on the host it is the
		// temp directory above.
		hostile := appendEntry(t, buf.Bytes(),
			tar.Header{Name: "workspace/evil/" + filepath.Base(hostTarget), Typeflag: tar.TypeReg, Mode: 0o644},
			[]byte("pwned"))

		ws := f.ws.Paths("sbx_snap_evil_fork")
		if _, err := f.ws.Create(ws); err != nil {
			t.Fatal(err)
		}
		err := snapshot.Restore(bytes.NewReader(hostile), snapshotFolders(ws))
		if err == nil {
			t.Fatal("restore accepted an entry written through a symlink")
		}
		if _, statErr := os.Stat(hostTarget); statErr == nil {
			t.Fatalf("hostile snapshot wrote %s on the host", hostTarget)
		}
	})
}

// appendEntry re-packs a tar and appends one more entry to it.
func appendEntry(t *testing.T, raw []byte, hdr tar.Header, body []byte) []byte {
	t.Helper()
	var out bytes.Buffer
	src := tar.NewReader(bytes.NewReader(raw))
	dst := tar.NewWriter(&out)
	for {
		h, err := src.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("read archive: %v", err)
		}
		if err := dst.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if _, err := io.Copy(dst, src); err != nil {
			t.Fatal(err)
		}
	}
	hdr.Size = int64(len(body))
	if err := dst.WriteHeader(&hdr); err != nil {
		t.Fatal(err)
	}
	if _, err := dst.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := dst.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}
