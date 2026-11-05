//go:build linux

package sandboxreset

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

const rootLine = "1 0 8:1 / / ro,relatime shared:1 - ext4 /dev/sda1 rw,errors=remount-ro\n"

type fakeMount struct {
	path string
	opts string
}

func rw(p string) fakeMount { return fakeMount{path: p, opts: "rw"} }
func ro(p string) fakeMount { return fakeMount{path: p, opts: "ro"} }

// mountinfo renders a table with a read-only root, so the only writable mounts
// are the temp directories a test names. No test depends on this machine's
// mounts.
func mountinfo(mounts ...fakeMount) string {
	var b strings.Builder
	b.WriteString(rootLine)
	for i, m := range mounts {
		fmt.Fprintf(&b, "%d 1 0:%d / %s %s,nosuid,nodev - tmpfs tmpfs rw\n", i+10, i+40, escapeMountPath(m.path), m.opts)
	}
	return b.String()
}

func escapeMountPath(p string) string {
	return strings.NewReplacer(`\`, `\134`, " ", `\040`, "\t", `\011`, "\n", `\012`).Replace(p)
}

// sandbox is a fake sandbox in a temp directory: one mount, and beside it a
// directory the mount must never reach, holding a canary.
type sandbox struct {
	t       *testing.T
	base    string
	mount   string
	outside string
	canary  string
}

func newSandbox(t *testing.T) *sandbox {
	t.Helper()
	// Mount paths have to be canonical, as mountinfo's are.
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := &sandbox{
		t:       t,
		base:    base,
		mount:   filepath.Join(base, "mnt"),
		outside: filepath.Join(base, "outside"),
		canary:  filepath.Join(base, "outside", "canary"),
	}
	s.mkdir("mnt")
	s.mkdir("outside")
	s.write("outside/canary", "must survive")
	return s
}

// info is the table for the sandbox's own mount plus any extra ones.
func (s *sandbox) info(extra ...fakeMount) string {
	return mountinfo(append([]fakeMount{rw(s.mount)}, extra...)...)
}

func (s *sandbox) path(rel string) string { return filepath.Join(s.base, rel) }

func (s *sandbox) mkdir(rel string) {
	s.t.Helper()
	if err := os.MkdirAll(s.path(rel), 0o755); err != nil {
		s.t.Fatal(err)
	}
}

func (s *sandbox) write(rel, content string) {
	s.t.Helper()
	if err := os.WriteFile(s.path(rel), []byte(content), 0o644); err != nil {
		s.t.Fatal(err)
	}
}

func (s *sandbox) symlink(target, rel string) {
	s.t.Helper()
	if err := os.Symlink(target, s.path(rel)); err != nil {
		s.t.Fatal(err)
	}
}

func (s *sandbox) mknod(rel string, mode uint32) {
	s.t.Helper()
	if err := unix.Mknod(s.path(rel), mode, 0); err != nil {
		s.t.Fatal(err)
	}
}

func (s *sandbox) chmod(rel string, mode os.FileMode) {
	s.t.Helper()
	if err := os.Chmod(s.path(rel), mode); err != nil {
		s.t.Fatal(err)
	}
}

// chmodBack lets t.TempDir clean up a directory a test left unreadable.
func (s *sandbox) chmodBack(rel string) {
	s.t.Cleanup(func() { _ = os.Chmod(s.path(rel), 0o755) })
}

func (s *sandbox) remove(rel string) {
	s.t.Helper()
	if err := os.RemoveAll(s.path(rel)); err != nil {
		s.t.Fatal(err)
	}
}

func (s *sandbox) names(rel string) []string {
	s.t.Helper()
	dirents, err := os.ReadDir(s.path(rel))
	if err != nil {
		s.t.Fatal(err)
	}
	names := make([]string, 0, len(dirents))
	for _, d := range dirents {
		names = append(names, d.Name())
	}
	return names
}

func (s *sandbox) assertNames(rel string, want ...string) {
	s.t.Helper()
	slices.Sort(want)
	if got := s.names(rel); !slices.Equal(got, want) {
		s.t.Fatalf("%s holds %q, want %q", rel, got, want)
	}
}

func (s *sandbox) assertExists(rel string) {
	s.t.Helper()
	if _, err := os.Lstat(s.path(rel)); err != nil {
		s.t.Fatalf("%s should still exist: %v", rel, err)
	}
}

func (s *sandbox) assertCanary() {
	s.t.Helper()
	data, err := os.ReadFile(s.canary)
	if err != nil {
		s.t.Fatalf("canary outside the mount is gone: %v", err)
	}
	if string(data) != "must survive" {
		s.t.Fatalf("canary outside the mount changed: %q", data)
	}
}

// capture lists the tree and sends the manifest through JSON, the way it reaches
// the helper's stdin under gVisor.
func capture(t *testing.T, info string) Manifest {
	t.Helper()
	m, err := Capture(context.Background(), strings.NewReader(info))
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	return roundTrip(t, m)
}

func roundTrip(t *testing.T, m Manifest) Manifest {
	t.Helper()
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	var out Manifest
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal manifest: %v", err)
	}
	return out
}

func listing(t *testing.T, m Manifest, path string) MountListing {
	t.Helper()
	for _, l := range m.Mounts {
		if l.Path == path {
			return l
		}
	}
	t.Fatalf("no listing for %s in %+v", path, m.Mounts)
	return MountListing{}
}

func entry(t *testing.T, l MountListing, rel string) Entry {
	t.Helper()
	for _, e := range l.Entries {
		if e.Rel == rel {
			return e
		}
	}
	t.Fatalf("no entry %q in listing of %s", rel, l.Path)
	return Entry{}
}

func rels(l MountListing) []string {
	out := make([]string, 0, len(l.Entries))
	for _, e := range l.Entries {
		out = append(out, e.Rel)
	}
	return out
}
