//go:build linux

package sandboxreset

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"testing/iotest"
	"time"

	"golang.org/x/sys/unix"

	"github.com/vorash/vora/internal/seccomp"
)

// restore is the only way a test reaches Restore. It refuses any manifest with a
// path outside the temp directory, so no test bug or deliberately broken code
// can aim a restore at a real system path.
func restore(t *testing.T, m Manifest, info string) (int, error) {
	t.Helper()
	assertOnlyTempPaths(t, m)
	return Restore(context.Background(), m, strings.NewReader(info))
}

func assertOnlyTempPaths(t *testing.T, m Manifest) {
	t.Helper()
	tempRoot, err := filepath.EvalSymlinks(os.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range m.Mounts {
		if !strings.HasPrefix(l.Path, tempRoot+"/") {
			t.Fatalf("refusing to restore %q: not under %s", l.Path, tempRoot)
		}
	}
}

// seedTree is the start state most tests capture: one of each listed kind.
func seedTree(s *sandbox) {
	s.t.Helper()
	s.write("mnt/file", "hello")
	s.mkdir("mnt/dir/sub")
	s.write("mnt/dir/inner", "inner")
	s.symlink("file", "mnt/link")
	s.mknod("mnt/pipe", unix.S_IFIFO|0o600)
	s.mknod("mnt/sock", unix.S_IFSOCK|0o660)
}

var seededNames = []string{"dir", "file", "link", "pipe", "sock"}

func TestRestoreRemovesWhatARunPlanted(t *testing.T) {
	s := newSandbox(t)
	seedTree(s)
	info := s.info()
	m := capture(t, info)

	s.write("mnt/.hidden", "secret")
	s.mkdir("mnt/.config/app/deep/deeper")
	s.write("mnt/.config/app/deep/deeper/usercustomize.py", "import evil")
	s.write("mnt/dir/planted-in-listed-dir", "secret")
	s.write("mnt/dir/sub/.pth", "secret")
	s.mknod("mnt/planted-pipe", unix.S_IFIFO|0o600)
	s.mknod("mnt/planted-sock", unix.S_IFSOCK|0o600)
	s.write("mnt/bad\xffname", "names that are not UTF-8 still go")
	s.symlink("nonexistent/dangling", "mnt/dangling")
	if err := os.Link(s.path("mnt/file"), s.path("mnt/hardlink-to-listed")); err != nil {
		t.Fatal(err)
	}

	removed, err := restore(t, m, info)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	// .hidden, .config tree (5 objects), two planted in listed dirs, pipe, sock,
	// bad name, dangling link, hard link.
	if want := 13; removed != want {
		t.Errorf("removed = %d, want %d", removed, want)
	}
	s.assertNames("mnt", seededNames...)
	s.assertNames("mnt/dir", "inner", "sub")
	s.assertNames("mnt/dir/sub")
}

func TestRestoreRemovesDirectoriesItCannotReadOrWrite(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory modes")
	}
	s := newSandbox(t)
	seedTree(s)
	info := s.info()
	m := capture(t, info)

	s.mkdir("mnt/locked/nested")
	s.write("mnt/locked/nested/secret", "x")
	s.write("mnt/locked/secret", "x")
	s.chmod("mnt/locked/nested", 0)
	s.chmod("mnt/locked", 0)
	s.chmodBack("mnt/locked")

	s.mkdir("mnt/readonly")
	s.write("mnt/readonly/secret", "x")
	s.chmod("mnt/readonly", 0o500)
	s.chmodBack("mnt/readonly")

	s.mkdir("mnt/writeonly")
	s.write("mnt/writeonly/secret", "x")
	s.chmod("mnt/writeonly", 0o200)
	s.chmodBack("mnt/writeonly")

	s.write("mnt/locked-file", "x")
	s.chmod("mnt/locked-file", 0)

	if _, err := restore(t, m, info); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	s.assertNames("mnt", seededNames...)
}

func TestRestoreIsIdempotent(t *testing.T) {
	s := newSandbox(t)
	seedTree(s)
	info := s.info()
	m := capture(t, info)
	s.write("mnt/planted", "x")
	if removed, err := restore(t, m, info); err != nil || removed != 1 {
		t.Fatalf("first Restore: removed %d, %v", removed, err)
	}
	if removed, err := restore(t, m, info); err != nil || removed != 0 {
		t.Fatalf("second Restore: removed %d, %v", removed, err)
	}
}

func TestRestoreUnlinksASymlinkOutWithoutTouchingItsTarget(t *testing.T) {
	s := newSandbox(t)
	seedTree(s)
	info := s.info()
	m := capture(t, info)

	s.symlink(s.outside, "mnt/to-dir")
	s.symlink(s.canary, "mnt/to-file")
	s.mkdir("mnt/tree")
	s.symlink(s.outside, "mnt/tree/to-dir")
	s.symlink("../../outside", "mnt/tree/relative")

	_, err := restore(t, m, info)
	s.assertCanary()
	s.assertNames("outside", "canary")
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	s.assertNames("mnt", seededNames...)
}

// The listed directory's contents are what a restore would prune, so a symlink
// standing in for it must never be entered.
func TestRestoreRefusesASymlinkReplacingAListedDirectory(t *testing.T) {
	s := newSandbox(t)
	seedTree(s)
	info := s.info()
	m := capture(t, info)

	s.remove("mnt/dir")
	s.symlink(s.outside, "mnt/dir")
	s.write("outside/would-look-planted", "keep")

	_, err := restore(t, m, info)
	s.assertCanary()
	s.assertNames("outside", "canary", "would-look-planted")
	if !errors.Is(err, ErrUnrestorable) {
		t.Fatalf("err = %v, want ErrUnrestorable", err)
	}
}

func TestRestoreRefusesASymlinkReplacingTheMountRoot(t *testing.T) {
	s := newSandbox(t)
	info := s.info()
	m := capture(t, info)

	s.remove("mnt")
	s.symlink(s.outside, "mnt")

	_, err := restore(t, m, info)
	s.assertCanary()
	s.assertNames("outside", "canary")
	if !errors.Is(err, ErrUnrestorable) {
		t.Fatalf("err = %v, want ErrUnrestorable", err)
	}
}

// The mount root itself moved, so its inode and mode still match the listing.
// Only refusing the symlink on the way stops a restore from acting on it.
func TestRestoreRefusesASymlinkOnTheWayToTheMount(t *testing.T) {
	s := newSandbox(t)
	s.mkdir("parent/mnt")
	s.mkdir("elsewhere")
	info := mountinfo(rw(s.path("parent/mnt")))
	m := capture(t, info)

	if err := os.Rename(s.path("parent/mnt"), s.path("elsewhere/mnt")); err != nil {
		t.Fatal(err)
	}
	s.remove("parent")
	s.symlink(s.path("elsewhere"), "parent")
	s.write("elsewhere/mnt/would-look-planted", "keep")

	_, err := restore(t, m, info)
	s.assertNames("elsewhere/mnt", "would-look-planted")
	if err == nil {
		t.Fatal("Restore followed a symlink in the mount path")
	}
}

// The stat and the open in a removal are separate syscalls. A thread here keeps
// turning a planted directory into a symlink to the canary's directory through
// that window. Restore may succeed or fail under this. Only a dead canary is wrong.
func TestRestoreSurvivesASymlinkSwappedInDuringRemoval(t *testing.T) {
	s := newSandbox(t)
	info := s.info()
	m := capture(t, info)

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			_ = os.Mkdir(s.path("mnt/victim"), 0o755)
			_ = os.Remove(s.path("mnt/victim"))
			_ = os.Symlink(s.outside, s.path("mnt/victim"))
			_ = os.Remove(s.path("mnt/victim"))
		}
	}()
	for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); {
		_, _ = restore(t, m, info)
		s.assertCanary()
	}
	close(stop)
	wg.Wait()
	s.assertNames("outside", "canary")
}

// A listed directory has the same window, between the stat that matched the
// listing and the open.
func TestRestoreSurvivesASymlinkSwappedInForAListedDirectory(t *testing.T) {
	s := newSandbox(t)
	s.mkdir("mnt/listed")
	info := s.info()
	m := capture(t, info)

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			_ = os.Rename(s.path("mnt/listed"), s.path("parked"))
			_ = os.Symlink(s.outside, s.path("mnt/listed"))
			_ = os.Remove(s.path("mnt/listed"))
			_ = os.Rename(s.path("parked"), s.path("mnt/listed"))
		}
	}()
	for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); {
		_, _ = restore(t, m, info)
		s.assertCanary()
	}
	close(stop)
	wg.Wait()
	s.assertNames("outside", "canary")
}

// An entry in the listing cannot be put back with its inode, and a socket has a
// listener behind it, so nothing is repaired: the caller destroys the sandbox.
func TestRestoreRejectsAListedEntryThatChangedOrWentAway(t *testing.T) {
	replaceWithNewInode := func(s *sandbox, rel, content string) {
		s.write(rel+".new", content)
		if err := os.Rename(s.path(rel+".new"), s.path(rel)); err != nil {
			s.t.Fatal(err)
		}
	}
	for name, change := range map[string]func(s *sandbox){
		"file content, same size":    func(s *sandbox) { s.write("mnt/file", "jello") },
		"file grown":                 func(s *sandbox) { s.write("mnt/file", "hello world") },
		"file truncated":             func(s *sandbox) { s.write("mnt/file", "") },
		"file mode":                  func(s *sandbox) { s.chmod("mnt/file", 0o600) },
		"file gone":                  func(s *sandbox) { s.remove("mnt/file") },
		"file same bytes, new inode": func(s *sandbox) { replaceWithNewInode(s, "mnt/file", "hello") },
		"file replaced by directory": func(s *sandbox) { s.remove("mnt/file"); s.mkdir("mnt/file") },
		"file replaced by symlink":   func(s *sandbox) { s.remove("mnt/file"); s.symlink(s.canary, "mnt/file") },
		"nested file gone":           func(s *sandbox) { s.remove("mnt/dir/inner") },
		"nested file content":        func(s *sandbox) { s.write("mnt/dir/inner", "INNER") },
		"directory mode":             func(s *sandbox) { s.chmod("mnt/dir", 0o700) },
		"directory mode 000":         func(s *sandbox) { s.chmod("mnt/dir", 0); s.chmodBack("mnt/dir") },
		"directory gone":             func(s *sandbox) { s.remove("mnt/dir") },
		"directory replaced by file": func(s *sandbox) { s.remove("mnt/dir"); s.write("mnt/dir", "") },
		"mount root mode":            func(s *sandbox) { s.chmod("mnt", 0o700); s.chmodBack("mnt") },
		"mount root gone":            func(s *sandbox) { s.remove("mnt") },
		"symlink retargeted":         func(s *sandbox) { s.remove("mnt/link"); s.symlink("elsewhere", "mnt/link") },
		"symlink gone":               func(s *sandbox) { s.remove("mnt/link") },
		"symlink replaced by file":   func(s *sandbox) { s.remove("mnt/link"); s.write("mnt/link", "") },
		"fifo gone":                  func(s *sandbox) { s.remove("mnt/pipe") },
		"fifo mode":                  func(s *sandbox) { s.chmod("mnt/pipe", 0o644) },
		"fifo replaced by file":      func(s *sandbox) { s.remove("mnt/pipe"); s.write("mnt/pipe", "") },
		"socket gone":                func(s *sandbox) { s.remove("mnt/sock") },
		"socket replaced by a new one": func(s *sandbox) {
			s.mknod("mnt/sock.new", unix.S_IFSOCK|0o660)
			if err := os.Rename(s.path("mnt/sock.new"), s.path("mnt/sock")); err != nil {
				s.t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			s := newSandbox(t)
			seedTree(s)
			info := s.info()
			m := capture(t, info)
			change(s)
			_, err := restore(t, m, info)
			s.assertCanary()
			if !errors.Is(err, ErrUnrestorable) {
				t.Fatalf("err = %v, want ErrUnrestorable", err)
			}
		})
	}
}

func TestRestoreRejectsAChangedOwner(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("chown needs root")
	}
	s := newSandbox(t)
	seedTree(s)
	info := s.info()
	m := capture(t, info)
	if err := os.Chown(s.path("mnt/file"), 1234, 5678); err != nil {
		t.Skipf("chown refused here: %v", err)
	}
	if _, err := restore(t, m, info); !errors.Is(err, ErrUnrestorable) {
		t.Fatalf("err = %v, want ErrUnrestorable", err)
	}
}

func TestRestoreRejectsAChangedDeviceNumber(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("mknod of a device needs root")
	}
	s := newSandbox(t)
	if err := unix.Mknod(s.path("mnt/dev"), unix.S_IFCHR|0o666, int(unix.Mkdev(1, 3))); err != nil {
		t.Skipf("mknod refused here: %v", err)
	}
	info := s.info()
	m := capture(t, info)
	s.remove("mnt/dev")
	if err := unix.Mknod(s.path("mnt/dev"), unix.S_IFCHR|0o666, int(unix.Mkdev(1, 5))); err != nil {
		t.Skipf("mknod refused here: %v", err)
	}
	if _, err := restore(t, m, info); !errors.Is(err, ErrUnrestorable) {
		t.Fatalf("err = %v, want ErrUnrestorable", err)
	}
}

func TestRestoreLeavesANestedMountpointAlone(t *testing.T) {
	s := newSandbox(t)
	seedTree(s)
	s.write("mnt/vora-init", "the agent binary")
	s.mkdir("mnt/pts")
	s.write("mnt/pts/ptmx", "")
	s.mkdir("mnt/dir/sub/rodir")
	info := s.info(ro(s.path("mnt/vora-init")), ro(s.path("mnt/pts")), ro(s.path("mnt/dir/sub/rodir")))
	m := capture(t, info)

	// Under a real mount these are the other mount's contents, not this one's. A
	// walk that entered would remove them; one that treated the name as planted
	// would try to remove the mountpoint.
	s.write("mnt/pts/not-ours", "x")
	s.write("mnt/dir/sub/rodir/not-ours", "x")
	s.write("mnt/vora-init", "other mount's bytes")
	s.write("mnt/planted", "x")

	removed, err := restore(t, m, info)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if removed != 1 {
		t.Errorf("removed = %d, want only the planted file", removed)
	}
	s.assertNames("mnt/pts", "not-ours", "ptmx")
	s.assertNames("mnt/dir/sub/rodir", "not-ours")
	s.assertExists("mnt/vora-init")
	s.assertNames("mnt", append([]string{"pts", "vora-init"}, seededNames...)...)
}

func TestRestoreRejectsANestedMountpointThatIsGone(t *testing.T) {
	s := newSandbox(t)
	s.mkdir("mnt/pts")
	info := s.info(ro(s.path("mnt/pts")))
	m := capture(t, info)
	s.remove("mnt/pts")
	if _, err := restore(t, m, info); !errors.Is(err, ErrUnrestorable) {
		t.Fatalf("err = %v, want ErrUnrestorable", err)
	}
}

func TestRestoreRestoresEachWritableMountByItsOwnListing(t *testing.T) {
	s := newSandbox(t)
	s.mkdir("mnt/shm")
	s.write("mnt/shm/listed", "x")
	s.mkdir("mnt/dev")
	s.write("mnt/dev/null", "")
	s.mkdir("second")
	info := s.info(rw(s.path("mnt/shm")), rw(s.path("mnt/dev/null")), rw(s.path("second")))
	m := capture(t, info)

	s.write("mnt/planted", "x")
	s.write("mnt/shm/planted", "x")
	s.write("mnt/dev/planted", "x")
	s.write("second/planted", "x")

	if removed, err := restore(t, m, info); err != nil || removed != 4 {
		t.Fatalf("Restore: removed %d, %v", removed, err)
	}
	s.assertNames("mnt", "dev", "shm")
	s.assertNames("mnt/shm", "listed")
	s.assertNames("mnt/dev", "null")
	s.assertNames("second")

	s.write("mnt/dev/null", "written")
	if _, err := restore(t, m, info); !errors.Is(err, ErrUnrestorable) {
		t.Fatalf("a changed file-root mount: err = %v, want ErrUnrestorable", err)
	}
}

// Nothing may be removed on the word of a table that does not match: the mount
// could be somewhere else now, or something could be hiding under a new one.
func TestRestoreRefusesToActWhenTheMountTableChanged(t *testing.T) {
	s := newSandbox(t)
	s.mkdir("mnt/sub")
	captured := s.info(ro(s.path("mnt/sub")))
	line := func(id int, path, opts, fstype string) string {
		return fmt.Sprintf("%d 1 0:9 / %s %s - %s tmpfs rw\n", id, path, opts, fstype)
	}
	mnt, sub := s.mount, s.path("mnt/sub")
	for name, live := range map[string]string{
		"new nested mount":       s.info(ro(sub), ro(s.path("mnt/new"))),
		"new writable mount":     s.info(ro(sub), rw(s.path("other"))),
		"new mount elsewhere":    s.info(ro(sub), ro(s.path("outside"))),
		"nested mount gone":      s.info(),
		"listed mount gone":      mountinfo(ro(sub)),
		"remounted, new id":      rootLine + line(11, mnt, "rw,nosuid,nodev", "tmpfs") + line(99, sub, "ro,nosuid,nodev", "tmpfs"),
		"mount type changed":     rootLine + line(10, mnt, "rw,nosuid,nodev", "ext4") + line(11, sub, "ro,nosuid,nodev", "tmpfs"),
		"listed mount read-only": rootLine + line(10, mnt, "ro,nosuid,nodev", "tmpfs") + line(11, sub, "ro,nosuid,nodev", "tmpfs"),
	} {
		t.Run(name, func(t *testing.T) {
			m := capture(t, captured)
			s.write("mnt/planted", "x")
			if _, err := restore(t, m, live); !errors.Is(err, ErrMountsChanged) {
				t.Fatalf("err = %v, want ErrMountsChanged", err)
			}
			s.assertNames("mnt", "planted", "sub")
			s.remove("mnt/planted")
		})
	}
}

// Without the check, the nested mount missing from this listing would look
// planted and Restore would try to remove it.
func TestRestoreRefusesAnInvalidManifestBeforeTouchingAnything(t *testing.T) {
	s := newSandbox(t)
	s.mkdir("mnt/pts")
	s.write("mnt/pts/ptmx", "")
	info := s.info(ro(s.path("mnt/pts")))
	m := capture(t, info)
	l := listing(t, m, s.mount)
	for i, e := range l.Entries {
		if e.Type == TypeMountpoint {
			m.Mounts[0].Entries = append(l.Entries[:i:i], l.Entries[i+1:]...)
		}
	}
	s.write("mnt/planted", "x")

	if _, err := restore(t, m, info); err == nil {
		t.Fatal("Restore accepted a manifest that drops a nested mount")
	}
	s.assertNames("mnt", "planted", "pts")
	s.assertNames("mnt/pts", "ptmx")
}

func TestRestoreFailsClosedOnBadMountinfo(t *testing.T) {
	boom := errors.New("read failed")
	s := newSandbox(t)
	info := s.info()
	m := capture(t, info)
	s.write("mnt/planted", "x")

	assertOnlyTempPaths(t, m)
	if _, err := Restore(context.Background(), m, iotest.ErrReader(boom)); !errors.Is(err, boom) {
		t.Errorf("unreadable: err = %v, want the read error", err)
	}
	if _, err := restore(t, m, strings.TrimSuffix(info, "\n")); err == nil {
		t.Error("truncated mountinfo was accepted")
	}
	if _, err := restore(t, m, ""); err == nil {
		t.Error("empty mountinfo was accepted")
	}
	if _, err := restore(t, Manifest{}, info); err == nil {
		t.Error("an empty manifest was accepted")
	}
	s.assertNames("mnt", "planted")
}

func TestRestoreStopsWhenTheContextIsCancelled(t *testing.T) {
	s := newSandbox(t)
	info := s.info()
	m := capture(t, info)
	s.write("mnt/planted", "x")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	assertOnlyTempPaths(t, m)
	if _, err := Restore(ctx, m, strings.NewReader(info)); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	s.assertNames("mnt", "planted")
}

func TestRestoreFailsIfRemovalFails(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory modes")
	}
	s := newSandbox(t)
	s.mkdir("mnt/dir")
	s.chmod("mnt/dir", 0o500)
	s.chmodBack("mnt/dir")
	info := s.info()
	m := capture(t, info)

	s.chmod("mnt/dir", 0o700)
	s.write("mnt/dir/planted", "x")
	s.chmod("mnt/dir", 0o500)

	// The listed directory keeps its listed mode, which forbids the unlink, and
	// nothing may loosen a mode the listing records.
	if _, err := restore(t, m, info); !errors.Is(err, unix.EACCES) {
		t.Fatalf("err = %v, want EACCES", err)
	}
	s.assertNames("mnt/dir", "planted")
}

// The same device trick as the capture test: the planted directory looks like it
// is on another device, so a restore must refuse to remove through it.
func TestRemoveRefusesADirectoryOnAnotherDevice(t *testing.T) {
	s := newSandbox(t)
	s.mkdir("mnt/foreign")
	s.write("mnt/foreign/file", "x")
	parent, err := unix.Open(s.mount, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = unix.Close(parent) }()
	var st unix.Stat_t
	if err := unix.Fstat(parent, &st); err != nil {
		t.Fatal(err)
	}
	p := pruner{ctx: context.Background(), rootDev: st.Dev + 1}
	if err := p.remove(parent, "foreign"); !errors.Is(err, ErrUnlistedMount) {
		t.Fatalf("err = %v, want ErrUnlistedMount", err)
	}
	s.assertNames("mnt/foreign", "file")
}

func TestRestoreRemovesADeepPlantedTree(t *testing.T) {
	s := newSandbox(t)
	info := s.info()
	m := capture(t, info)
	s.plantDeepTree("mnt")
	removed, err := restore(t, m, info)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if removed != deepTreeDepth {
		t.Errorf("removed = %d, want %d", removed, deepTreeDepth)
	}
	s.assertNames("mnt")
}

// The error reaches a log line that the design appends to, and the workload
// chooses how deep the tree is.
func TestADeepTreeDoesNotSizeTheError(t *testing.T) {
	t.Run("removal failing at the bottom", func(t *testing.T) {
		inChild(t, func() {
			s := newSandbox(t)
			info := s.info()
			m := capture(t, info)
			s.plantDeepTree("mnt")
			failSyscalls(t, map[uint32]unix.Errno{unix.SYS_UNLINKAT: unix.EIO})
			_, err := restore(t, m, info)
			if !errors.Is(err, unix.EIO) {
				t.Fatalf("err = %v, want EIO", err)
			}
			assertSmallError(t, err)
		})
	})
	t.Run("capture failing at the bottom", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root reads a mode-000 file")
		}
		s := newSandbox(t)
		deepest := s.plantDeepTree("mnt")
		leaf, err := unix.Openat(deepest, "leaf", unix.O_CREAT|unix.O_WRONLY|unix.O_CLOEXEC, 0)
		if err != nil {
			t.Fatal(err)
		}
		_ = unix.Close(leaf)
		_, err = Capture(context.Background(), strings.NewReader(s.info()))
		if !errors.Is(err, unix.EACCES) {
			t.Fatalf("err = %v, want EACCES", err)
		}
		assertSmallError(t, err)
	})
	t.Run("listed entry changed at the bottom", func(t *testing.T) {
		s := newSandbox(t)
		deepest := s.plantDeepTree("mnt")
		leaf, err := unix.Openat(deepest, "leaf", unix.O_CREAT|unix.O_WRONLY|unix.O_CLOEXEC, 0o644)
		if err != nil {
			t.Fatal(err)
		}
		_ = unix.Close(leaf)
		info := s.info()
		m := capture(t, info)
		if err := unix.Fchmodat(deepest, "leaf", 0o600, 0); err != nil {
			t.Fatal(err)
		}
		_, err = restore(t, m, info)
		if !errors.Is(err, ErrUnrestorable) {
			t.Fatalf("err = %v, want ErrUnrestorable", err)
		}
		assertSmallError(t, err)
	})
	t.Run("proof walk finding it", func(t *testing.T) {
		s := newSandbox(t)
		m := capture(t, s.info())
		s.plantDeepTree("mnt")
		err := verifyListings(context.Background(), m)
		if !errors.Is(err, ErrNotClean) {
			t.Fatalf("err = %v, want ErrNotClean", err)
		}
		assertSmallError(t, err)
	})
}

// A walk holds one descriptor per level, so a deep enough tree runs out of them.
func TestRunningOutOfDescriptorsFailsClosed(t *testing.T) {
	limit := unix.Rlimit{Cur: 64, Max: 64}
	t.Run("restore", func(t *testing.T) {
		inChild(t, func() {
			s := newSandbox(t)
			info := s.info()
			m := capture(t, info)
			s.plantDeepTree("mnt")
			if err := unix.Setrlimit(unix.RLIMIT_NOFILE, &limit); err != nil {
				t.Fatal(err)
			}
			if _, err := restore(t, m, info); !errors.Is(err, unix.EMFILE) {
				t.Fatalf("err = %v, want EMFILE", err)
			}
		})
	})
	t.Run("capture", func(t *testing.T) {
		inChild(t, func() {
			s := newSandbox(t)
			s.plantDeepTree("mnt")
			if err := unix.Setrlimit(unix.RLIMIT_NOFILE, &limit); err != nil {
				t.Fatal(err)
			}
			if _, err := Capture(context.Background(), strings.NewReader(s.info())); !errors.Is(err, unix.EMFILE) {
				t.Fatalf("err = %v, want EMFILE", err)
			}
		})
	})
}

// Restore prunes before this walk, so calling Restore cannot show that the walk
// would catch something the prune left.
func TestTheProofWalkReportsWhatThePruneLeft(t *testing.T) {
	s := newSandbox(t)
	seedTree(s)
	m := capture(t, s.info())
	s.write("mnt/planted", "x")
	if err := verifyListings(context.Background(), m); !errors.Is(err, ErrNotClean) {
		t.Fatalf("planted file: err = %v, want ErrNotClean", err)
	}
	s.assertExists("mnt/planted")
	s.remove("mnt/planted")
	s.write("mnt/file", "jello")
	if err := verifyListings(context.Background(), m); !errors.Is(err, ErrUnrestorable) {
		t.Fatalf("changed file: err = %v, want ErrUnrestorable", err)
	}
}

func TestRestoreRemovesNothingFromACleanMemberAndPasses(t *testing.T) {
	s := newSandbox(t)
	seedTree(s)
	info := s.info()
	m := capture(t, info)
	if removed, err := restore(t, m, info); err != nil || removed != 0 {
		t.Fatalf("Restore on a clean tree: removed %d, %v", removed, err)
	}
	s.assertNames("mnt", seededNames...)
}

// These call validate directly and never Restore, because the point of several
// is a path that Restore must never be pointed at, even in a test.
func TestManifestValidationRefusesWhatRestoreCouldBeMisledBy(t *testing.T) {
	s := newSandbox(t)
	seedTree(s)
	s.mkdir("mnt/sub")
	good := capture(t, s.info(ro(s.path("mnt/sub"))))
	if err := good.validate(); err != nil {
		t.Fatalf("a captured manifest must validate: %v", err)
	}

	for name, tamper := range map[string]func(m *Manifest){
		"no listings":             func(m *Manifest) { m.Mounts = nil },
		"no table":                func(m *Manifest) { m.Table = nil },
		"listing for root":        func(m *Manifest) { m.Mounts[0].Path = "/" },
		"listing for real path":   func(m *Manifest) { m.Mounts[0].Path = "/etc" },
		"listing path with dots":  func(m *Manifest) { m.Mounts[0].Path = s.mount + "/../.." },
		"listing not in table":    func(m *Manifest) { m.Mounts = append(m.Mounts, MountListing{Path: "/var"}) },
		"table says root rw":      func(m *Manifest) { m.Table[0].Writable = true },
		"table path relative":     func(m *Manifest) { m.Table[1].Path = "tmp" },
		"table path with dots":    func(m *Manifest) { m.Table[1].Path = s.mount + "/../etc" },
		"no entries":              func(m *Manifest) { m.Mounts[0].Entries = nil },
		"entry escapes with dots": func(m *Manifest) { m.Mounts[0].Entries[1].Rel = "../x" },
		"entry escapes inside":    func(m *Manifest) { m.Mounts[0].Entries[1].Rel = "dir/../../x" },
		"absolute entry":          func(m *Manifest) { m.Mounts[0].Entries[1].Rel = "/etc" },
		"empty entry path":        func(m *Manifest) { m.Mounts[0].Entries[1].Rel = "" },
		"unknown type":            func(m *Manifest) { m.Mounts[0].Entries[1].Type = "door" },
		"duplicate entry":         func(m *Manifest) { m.Mounts[0].Entries[2] = m.Mounts[0].Entries[1] },
		"unsorted entries": func(m *Manifest) {
			e := m.Mounts[0].Entries
			e[1], e[2] = e[2], e[1]
		},
		"root is a mountpoint": func(m *Manifest) { m.Mounts[0].Entries[0].Type = TypeMountpoint },
		"root missing":         func(m *Manifest) { m.Mounts[0].Entries = m.Mounts[0].Entries[1:] },
		"nested mount dropped from listing": func(m *Manifest) {
			kept := m.Mounts[0].Entries[:0]
			for _, e := range m.Mounts[0].Entries {
				if e.Type != TypeMountpoint {
					kept = append(kept, e)
				}
			}
			m.Mounts[0].Entries = kept
		},
		"mountpoint the table lacks": func(m *Manifest) {
			for i, e := range m.Mounts[0].Entries {
				if e.Rel == "file" {
					m.Mounts[0].Entries[i] = Entry{Rel: "file", Type: TypeMountpoint}
				}
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			m := roundTrip(t, good)
			tamper(&m)
			if reflect.DeepEqual(m, good) {
				t.Fatal("the tamper changed nothing")
			}
			if err := m.validate(); err == nil {
				t.Fatal("validate accepted it")
			}
		})
	}
}

// vora-init runs with its seccomp filter on and PR_SET_DUMPABLE 0, which hands
// its /proc/self files to root.
func TestRestoreWorksUnderTheAgentsRestrictions(t *testing.T) {
	inChild(t, func() {
		if err := seccomp.Install(); err != nil {
			t.Fatalf("seccomp: %v", err)
		}
		if err := unix.Prctl(unix.PR_SET_DUMPABLE, 0, 0, 0, 0); err != nil {
			t.Fatalf("set non-dumpable: %v", err)
		}
		dumpable, err := unix.PrctlRetInt(unix.PR_GET_DUMPABLE, 0, 0, 0, 0)
		if err != nil || dumpable != 0 {
			t.Fatalf("dumpable = %d, %v", dumpable, err)
		}

		s := newSandbox(t)
		seedTree(s)
		info := s.info()
		m := capture(t, info)
		s.mkdir("mnt/locked/nested")
		s.write("mnt/locked/nested/secret", "x")
		s.chmod("mnt/locked/nested", 0)
		s.chmod("mnt/locked", 0)
		s.chmodBack("mnt/locked")
		s.symlink(s.outside, "mnt/out")

		if _, err := restore(t, m, info); err != nil {
			t.Fatalf("Restore: %v", err)
		}
		s.assertNames("mnt", seededNames...)
		s.assertCanary()
	})
}
