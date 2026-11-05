//go:build linux

package sandboxreset

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"unsafe"

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
	base, err := filepath.EvalSymlinks(tempBase(t))
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

// deepTreeDepth is deep enough that wrapping a name per level, or printing the
// whole path, would make an error far longer than errorSizeLimit.
const (
	deepTreeDepth  = 600
	errorSizeLimit = 2048
)

// plantDeepTree nests deepTreeDepth directories under rel and returns the deepest
// one, open. It steps by descriptor because the whole path is longer than a
// syscall takes.
func (s *sandbox) plantDeepTree(rel string) int {
	s.t.Helper()
	name := strings.Repeat("d", 100)
	fd, err := unix.Open(s.path(rel), unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		s.t.Fatal(err)
	}
	for range deepTreeDepth {
		if err := unix.Mkdirat(fd, name, 0o755); err != nil {
			s.t.Fatal(err)
		}
		next, err := unix.Openat(fd, name, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
		_ = unix.Close(fd)
		if err != nil {
			s.t.Fatal(err)
		}
		fd = next
	}
	s.t.Cleanup(func() { _ = unix.Close(fd) })
	return fd
}

func assertSmallError(t *testing.T, err error) {
	t.Helper()
	if n := len(err.Error()); n > errorSizeLimit {
		t.Fatalf("error is %d bytes, want at most %d: %.200s...", n, errorSizeLimit, err.Error())
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

const (
	childTestEnv = "VORA_SANDBOXRESET_TEST_CHILD"
	childBaseEnv = "VORA_SANDBOXRESET_TEST_BASE"
	childDone    = "sandboxreset-child-body-finished"
)

// inChild runs body in a fresh copy of the test binary, for what applies to a
// whole process: a seccomp filter, a descriptor limit, a new session. Nothing
// after the inChild call runs in the child's test, and the parent fails unless
// body ran to its end.
func inChild(t *testing.T, body func()) {
	t.Helper()
	if os.Getenv(childTestEnv) == t.Name() {
		body()
		fmt.Println(childDone)
		return
	}
	parts := strings.Split(t.Name(), "/")
	for i, p := range parts {
		parts[i] = "^" + regexp.QuoteMeta(p) + "$"
	}
	cmd := exec.Command(os.Args[0], "-test.v", "-test.run="+strings.Join(parts, "/"))
	cmd.Env = append(os.Environ(), childTestEnv+"="+t.Name(), childBaseEnv+"="+t.TempDir())
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("child failed: %v\n%s", err, out)
	}
	if strings.Contains(string(out), "--- SKIP") {
		t.Skipf("child skipped:\n%s", out)
	}
	if !strings.Contains(string(out), childDone) {
		t.Fatalf("child did not run to its end:\n%s", out)
	}
}

// tempBase gives a child a directory its parent removes, because a child may
// have broken the syscalls its own t.TempDir cleanup needs.
func tempBase(t *testing.T) string {
	t.Helper()
	parent := os.Getenv(childBaseEnv)
	if parent == "" {
		return t.TempDir()
	}
	base, err := os.MkdirTemp(parent, "child")
	if err != nil {
		t.Fatal(err)
	}
	return base
}

// failSyscalls makes the kernel refuse each syscall with its errno from now on,
// in every thread. It is how a test reaches a real error path that no file this
// user can make would produce. Call it only inside inChild.
func failSyscalls(t *testing.T, errnos map[uint32]unix.Errno) {
	t.Helper()
	filter := []unix.SockFilter{{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: 0}}
	for nr, errno := range errnos {
		filter = append(filter,
			unix.SockFilter{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, Jt: 0, Jf: 1, K: nr},
			unix.SockFilter{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_ERRNO | uint32(errno)},
		)
	}
	filter = append(filter, unix.SockFilter{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_ALLOW})
	prog := unix.SockFprog{Len: uint16(len(filter)), Filter: &filter[0]}
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	if _, _, errno := unix.Syscall(unix.SYS_SECCOMP, unix.SECCOMP_SET_MODE_FILTER,
		unix.SECCOMP_FILTER_FLAG_TSYNC, uintptr(unsafe.Pointer(&prog))); errno != 0 {
		t.Fatal(errno)
	}
}
