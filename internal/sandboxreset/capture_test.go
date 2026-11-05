//go:build linux

package sandboxreset

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"testing/iotest"

	"golang.org/x/sys/unix"
)

// A readable table with a valid writable mount sits beside each bad line, so
// skipping or tolerating the bad line would make Capture succeed. A table it
// cannot read in full might be hiding a writable mount.
func TestCaptureFailsClosedOnMalformedMountinfo(t *testing.T) {
	s := newSandbox(t)
	other := s.outside
	good := s.info()
	for name, info := range map[string]string{
		"blank line":                good + "\n",
		"no trailing newline":       strings.TrimSuffix(good, "\n"),
		"line cut before separator": good + "50 1 0:1 / " + other + " rw\n",
		"no separator":              good + "50 1 0:1 / " + other + " rw tmpfs tmpfs rw\n",
		"missing superblock field":  good + "50 1 0:1 / " + other + " rw - tmpfs tmpfs\n",
		"extra field after dash":    good + "50 1 0:1 / " + other + " rw - tmpfs tmpfs rw extra\n",
		"non-numeric id":            good + "x 1 0:1 / " + other + " rw - tmpfs tmpfs rw\n",
		"non-numeric parent":        good + "50 x 0:1 / " + other + " rw - tmpfs tmpfs rw\n",
		"mount options lack rw/ro":  good + "50 1 0:1 / " + other + " nosuid - tmpfs tmpfs rw\n",
		"super options lack rw/ro":  good + "50 1 0:1 / " + other + " rw - tmpfs tmpfs size=1k\n",
		"both rw and ro":            good + "50 1 0:1 / " + other + " rw,ro - tmpfs tmpfs rw\n",
		"truncated escape":          good + "50 1 0:1 / " + other + `\04 rw - tmpfs tmpfs rw` + "\n",
		"non-octal escape":          good + "50 1 0:1 / " + other + `\09x rw - tmpfs tmpfs rw` + "\n",
		"escape above a byte":       good + "50 1 0:1 / " + other + `\777 rw - tmpfs tmpfs rw` + "\n",
		"relative mount point":      good + "50 1 0:1 / relative rw - tmpfs tmpfs rw\n",
		"unclean mount point":       good + "50 1 0:1 / " + s.mount + "/../outside rw - tmpfs tmpfs rw\n",
		"empty":                     "",
		"no root mount":             "50 1 0:1 / " + other + " rw - tmpfs tmpfs rw\n",
		"no writable mount":         rootLine,
	} {
		t.Run(name, func(t *testing.T) {
			m, err := Capture(context.Background(), strings.NewReader(info))
			if err == nil {
				t.Fatalf("Capture succeeded on malformed mountinfo: %+v", m)
			}
		})
	}
	if _, err := Capture(context.Background(), strings.NewReader(good)); err != nil {
		t.Fatalf("the table the bad lines were added to must itself be fine: %v", err)
	}
}

func TestCaptureFailsClosedOnUnreadableMountinfo(t *testing.T) {
	boom := errors.New("read failed")
	if _, err := Capture(context.Background(), iotest.ErrReader(boom)); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the read error", err)
	}
	midway := io.MultiReader(strings.NewReader(rootLine), iotest.ErrReader(boom))
	if _, err := Capture(context.Background(), midway); !errors.Is(err, boom) {
		t.Fatalf("read failing midway: err = %v, want the read error", err)
	}
}

func TestCaptureRejectsWritableRoot(t *testing.T) {
	s := newSandbox(t)
	info := "1 0 8:1 / / rw,relatime - ext4 /dev/sda1 rw\n" + "5 1 0:1 / " + s.mount + " rw - tmpfs tmpfs rw\n"
	if _, err := Capture(context.Background(), strings.NewReader(info)); !errors.Is(err, ErrRootWritable) {
		t.Fatalf("err = %v, want ErrRootWritable", err)
	}
}

func TestCaptureRecordsWhatEachEntryKindNeeds(t *testing.T) {
	s := newSandbox(t)
	s.mkdir("mnt/dir")
	s.write("mnt/file", "hello")
	s.symlink("some/target", "mnt/link")
	s.mknod("mnt/pipe", unix.S_IFIFO|0o600)
	s.mknod("mnt/sock", unix.S_IFSOCK|0o600)
	// Through unix.Chmod, since os.FileMode does not carry these bits numerically.
	for rel, mode := range map[string]uint32{"mnt/dir": 0o1750, "mnt/file": 0o4640, "mnt/sock": 0o660} {
		if err := unix.Chmod(s.path(rel), mode); err != nil {
			t.Fatal(err)
		}
	}

	l := listing(t, capture(t, s.info()), s.mount)

	if want := []string{".", "dir", "file", "link", "pipe", "sock"}; !slices.Equal(rels(l), want) {
		t.Fatalf("entries %q, want %q", rels(l), want)
	}
	for rel, want := range map[string]Entry{
		".":    {Rel: ".", Type: TypeDir, Mode: 0o755},
		"dir":  {Rel: "dir", Type: TypeDir, Mode: 0o1750},
		"file": {Rel: "file", Type: TypeFile, Mode: 0o4640, Size: 5, SHA256: "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"},
		"link": {Rel: "link", Type: TypeSymlink, Mode: 0o777, Target: "some/target"},
		"pipe": {Rel: "pipe", Type: TypeFIFO, Mode: 0o600},
		"sock": {Rel: "sock", Type: TypeSocket, Mode: 0o660},
	} {
		target := s.mount + "/" + rel
		var st unix.Stat_t
		if err := unix.Lstat(target, &st); err != nil {
			t.Fatal(err)
		}
		want.UID, want.GID, want.Ino = st.Uid, st.Gid, st.Ino
		got := entry(t, l, rel)
		if want.Type == TypeDir {
			want.MtimeNs = st.Mtim.Nano()
			// The capture's own walk may have moved it since.
			want.AtimeNs = got.AtimeNs
		} else {
			want.CtimeNs = st.Ctim.Nano()
		}
		if got != want {
			t.Errorf("%s: got %+v, want %+v", rel, got, want)
		}
	}
}

// A device node is a host inode under bwrap, and the host can touch it, so a change
// time there would fail a reset no run caused.
func TestEntryRecordsAChangeTimeOnlyWhereARunIsTheOnlyWriter(t *testing.T) {
	ctime := unix.Timespec{Sec: 1000, Nsec: 5}
	for mode, want := range map[uint32]int64{
		unix.S_IFREG:  ctime.Nano(),
		unix.S_IFLNK:  ctime.Nano(),
		unix.S_IFSOCK: ctime.Nano(),
		unix.S_IFIFO:  ctime.Nano(),
		unix.S_IFCHR:  0,
		unix.S_IFBLK:  0,
		unix.S_IFDIR:  0,
	} {
		got, err := entryFromStat("x", &unix.Stat_t{Mode: mode, Ctim: ctime})
		if err != nil {
			t.Fatal(err)
		}
		if got.CtimeNs != want {
			t.Errorf("mode %#o: CtimeNs = %d, want %d", mode, got.CtimeNs, want)
		}
	}
}

func TestCaptureSortsEveryListing(t *testing.T) {
	s := newSandbox(t)
	for _, name := range []string{"zz", "a", "M", ".hidden", "b"} {
		s.write("mnt/"+name, "")
	}
	s.mkdir("mnt/dir/inner")
	s.write("mnt/dir/inner/z", "")
	s.write("mnt/dir/inner/a", "")
	got := rels(listing(t, capture(t, s.info()), s.mount))
	want := []string{".", ".hidden", "M", "a", "b", "dir", "dir/inner", "dir/inner/a", "dir/inner/z", "zz"}
	if !slices.Equal(got, want) {
		t.Fatalf("entries %q, want %q", got, want)
	}
}

func TestCaptureNeverFollowsASymlink(t *testing.T) {
	s := newSandbox(t)
	s.symlink(s.outside, "mnt/out")
	s.symlink(s.canary, "mnt/outfile")
	l := listing(t, capture(t, s.info()), s.mount)
	if want := []string{".", "out", "outfile"}; !slices.Equal(rels(l), want) {
		t.Fatalf("entries %q, want %q", rels(l), want)
	}
	if got := entry(t, l, "outfile"); got.Type != TypeSymlink || got.SHA256 != "" {
		t.Fatalf("a link to a file should record the link only: %+v", got)
	}
}

func TestCaptureDoesNotCrossANestedMountAndListsWritableOnesSeparately(t *testing.T) {
	s := newSandbox(t)
	s.write("mnt/control", "")
	s.write("mnt/vora-init", "the agent binary")
	s.mkdir("mnt/pts")
	s.write("mnt/pts/ptmx", "")
	s.mkdir("mnt/a/b/deep")
	s.write("mnt/a/b/deep/hidden-below-mount", "")

	m := capture(t, s.info(ro(s.path("mnt/vora-init")), rw(s.path("mnt/pts")), ro(s.path("mnt/a/b/deep"))))

	if len(m.Mounts) != 2 {
		t.Fatalf("want listings for the two writable mounts only, got %+v", m.Mounts)
	}
	parent := listing(t, m, s.mount)
	if want := []string{".", "a", "a/b", "a/b/deep", "control", "pts", "vora-init"}; !slices.Equal(rels(parent), want) {
		t.Fatalf("parent entries %q, want %q", rels(parent), want)
	}
	for _, rel := range []string{"vora-init", "pts", "a/b/deep"} {
		if got, want := entry(t, parent, rel), (Entry{Rel: rel, Type: TypeMountpoint}); got != want {
			t.Errorf("%s: got %+v, want name and type only", rel, got)
		}
	}
	if want := []string{".", "ptmx"}; !slices.Equal(rels(listing(t, m, s.path("mnt/pts"))), want) {
		t.Fatalf("nested writable listing %q, want %q", rels(listing(t, m, s.path("mnt/pts"))), want)
	}
}

// bwrap's --dev binds /dev/null and friends as writable mounts of a single device
// node. A regular file stands in here, since making a device needs root.
func TestCaptureListsAWritableMountWhoseRootIsNotADirectory(t *testing.T) {
	s := newSandbox(t)
	s.mkdir("mnt/dev")
	s.write("mnt/dev/null", "x")
	m := capture(t, s.info(rw(s.path("mnt/dev/null"))))
	l := listing(t, m, s.path("mnt/dev/null"))
	if want := []string{"."}; !slices.Equal(rels(l), want) {
		t.Fatalf("entries %q, want %q", rels(l), want)
	}
	if got := entry(t, l, "."); got.Type != TypeFile || got.Size != 1 {
		t.Fatalf("root entry %+v, want the file itself", got)
	}
}

func TestCaptureFindsAMountWhosePathWasEscaped(t *testing.T) {
	s := newSandbox(t)
	s.mkdir("mnt with space\tand tab")
	s.write("mnt with space\tand tab/file", "")
	path := s.path("mnt with space\tand tab")
	m := capture(t, mountinfo(rw(path)))
	if !strings.Contains(mountinfo(rw(path)), `\040`) {
		t.Fatal("the fixture should escape the space")
	}
	if want := []string{".", "file"}; !slices.Equal(rels(listing(t, m, path)), want) {
		t.Fatalf("entries %q, want %q", rels(listing(t, m, path)), want)
	}
}

func TestCaptureFailsWhenAMountInTheTableCannotBeReached(t *testing.T) {
	s := newSandbox(t)
	for name, info := range map[string]string{
		"writable mount missing":         s.info(rw(s.path("mnt/gone"))),
		"read-only nested mount missing": s.info(ro(s.path("mnt/gone"))),
		"mount path under a file": func() string {
			s.write("mnt/file", "")
			return s.info(ro(s.path("mnt/file/below")))
		}(),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Capture(context.Background(), strings.NewReader(info)); err == nil {
				t.Fatal("Capture succeeded though a mount in the table is not in the tree")
			}
		})
	}
}

func TestCaptureFailsWhenAMountPathRunsThroughASymlink(t *testing.T) {
	s := newSandbox(t)
	s.symlink(s.outside, "linked")
	if _, err := Capture(context.Background(), strings.NewReader(mountinfo(rw(s.path("linked/canary"))))); err == nil {
		t.Fatal("Capture followed a symlink in a mount path")
	}
}

func TestCaptureFailsOnAnUnreadableDirectory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a mode-000 directory")
	}
	s := newSandbox(t)
	s.mkdir("mnt/locked")
	s.chmod("mnt/locked", 0)
	s.chmodBack("mnt/locked")
	if _, err := Capture(context.Background(), strings.NewReader(s.info())); !errors.Is(err, unix.EACCES) {
		t.Fatalf("err = %v, want EACCES", err)
	}
}

func TestCaptureFailsOnAnUnreadableFile(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a mode-000 file")
	}
	s := newSandbox(t)
	s.write("mnt/locked", "x")
	s.chmod("mnt/locked", 0)
	if _, err := Capture(context.Background(), strings.NewReader(s.info())); !errors.Is(err, unix.EACCES) {
		t.Fatalf("err = %v, want EACCES", err)
	}
}

// JSON replaces invalid UTF-8, so the host would hand back a different name.
func TestCaptureRefusesNamesJSONCannotCarry(t *testing.T) {
	t.Run("file name", func(t *testing.T) {
		s := newSandbox(t)
		s.write("mnt/bad\xff", "")
		if _, err := Capture(context.Background(), strings.NewReader(s.info())); err == nil {
			t.Fatal("Capture accepted a name that does not survive JSON")
		}
	})
	t.Run("link target", func(t *testing.T) {
		s := newSandbox(t)
		s.symlink("bad\xff", "mnt/link")
		if _, err := Capture(context.Background(), strings.NewReader(s.info())); err == nil {
			t.Fatal("Capture accepted a target that does not survive JSON")
		}
	})
}

func TestCaptureStopsWhenTheContextIsCancelled(t *testing.T) {
	s := newSandbox(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Capture(ctx, strings.NewReader(s.info())); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

// A terminal only reaches this open if one is swapped in after the lstat. A
// session leader with no terminal would then take it as its controlling terminal,
// and the agent would get its signals.
func TestHashingASwappedInTerminalDoesNotMakeItTheControllingTerminal(t *testing.T) {
	inChild(t, func() {
		if _, err := unix.Setsid(); err != nil {
			t.Fatal(err)
		}
		master, err := unix.Open("/dev/ptmx", unix.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
		if err != nil {
			t.Skipf("no pty to test with: %v", err)
		}
		defer func() { _ = unix.Close(master) }()
		if err := unix.IoctlSetPointerInt(master, unix.TIOCSPTLCK, 0); err != nil {
			t.Fatal(err)
		}
		n, err := unix.IoctlGetInt(master, unix.TIOCGPTN)
		if err != nil {
			t.Fatal(err)
		}
		pts, err := unix.Open("/dev/pts", unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = unix.Close(pts) }()

		s := newSandbox(t)
		s.write("mnt/file", "x")
		var regular unix.Stat_t
		if err := unix.Lstat(s.path("mnt/file"), &regular); err != nil {
			t.Fatal(err)
		}
		if _, err := hashFileAt(pts, strconv.Itoa(n), &regular); err == nil {
			t.Fatal("hashed a terminal as if it were the file the lstat saw")
		}

		fd, err := unix.Open("/dev/tty", unix.O_RDONLY|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
		if err == nil {
			_ = unix.Close(fd)
			t.Fatal("the session gained a controlling terminal")
		}
		if !errors.Is(err, unix.ENXIO) {
			t.Fatalf("open /dev/tty: %v, want ENXIO for a session with no terminal", err)
		}
	})
}

func TestCaptureDigestsNamesAndValuesInAnyOrder(t *testing.T) {
	s := newSandbox(t)
	set := func(rel string, attrs ...[2]string) {
		s.write("mnt/"+rel, "same content")
		for _, a := range attrs {
			s.setxattr("mnt/"+rel, a[0], a[1])
		}
	}
	set("none")
	set("ab", [2]string{"user.a", "1"}, [2]string{"user.b", "2"})
	set("ba", [2]string{"user.b", "2"}, [2]string{"user.a", "1"})
	set("swapped", [2]string{"user.a", "2"}, [2]string{"user.b", "1"})
	set("renamed", [2]string{"user.a", "1"}, [2]string{"user.c", "2"})
	set("fewer", [2]string{"user.a", "1"})
	set("emptyvalue", [2]string{"user.a", ""})
	set("othervalue", [2]string{"user.a", "x"})
	// Without a length on each part, this one's bytes read the same as ab's.
	set("run-together", [2]string{"user.a", "1\x00user.b\x002"})

	l := listing(t, capture(t, s.info()), s.mount)
	digest := func(rel string) string { return entry(t, l, rel).Xattrs }

	if digest("none") != "" {
		t.Errorf("no attributes: digest %q, want empty", digest("none"))
	}
	if digest("ab") != digest("ba") {
		t.Errorf("the order attributes were set in changed the digest")
	}
	seen := map[string]string{"": "none"}
	for _, rel := range []string{"ab", "swapped", "renamed", "fewer", "emptyvalue", "othervalue", "run-together"} {
		if other, dup := seen[digest(rel)]; dup {
			t.Errorf("%s and %s have the same digest", rel, other)
		}
		seen[digest(rel)] = rel
	}
}

// A read that followed the link would record the target's attributes as the link's,
// and could read a file outside the mount.
func TestCaptureReadsALinksOwnAttributesNotItsTargets(t *testing.T) {
	s := newSandbox(t)
	s.write("mnt/target", "x")
	s.setxattr("mnt/target", "user.a", "1")
	s.symlink("target", "mnt/link")
	l := listing(t, capture(t, s.info()), s.mount)
	if entry(t, l, "target").Xattrs == "" {
		t.Fatal("the target's attribute was not seen, so this shows nothing about the link")
	}
	if got := entry(t, l, "link").Xattrs; got != "" {
		t.Fatalf("link digest %q, want empty: it read through to the target", got)
	}
}

// The walk and this read are separate syscalls, so something swapped in between must
// not have its attributes recorded for what the walk saw.
func TestAttributesOfAnObjectSwappedInAreRefused(t *testing.T) {
	s := newSandbox(t)
	s.write("mnt/seen", "x")
	s.write("mnt/swapped", "x")
	parent, err := unix.Open(s.mount, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = unix.Close(parent) }()
	seen := s.lstat("mnt/seen")
	if _, err := xattrDigestAt(parent, "seen", &seen); err != nil {
		t.Fatalf("the object the stat saw: %v", err)
	}
	if _, err := xattrDigestAt(parent, "swapped", &seen); err == nil {
		t.Fatal("read the attributes of another object than the stat saw")
	}
}

// Reading the attributes must not need an open that the mode forbids. The mount is
// in the temp directory, so this reads nothing of the system's.
func TestCaptureReadsAttributesOfNodesItCannotOpen(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can open a mode-000 node")
	}
	s := newSandbox(t)
	s.mknod("mnt/pipe", unix.S_IFIFO)
	s.mknod("mnt/sock", unix.S_IFSOCK)
	l := listing(t, capture(t, s.info()), s.mount)
	if want := []string{".", "pipe", "sock"}; !slices.Equal(rels(l), want) {
		t.Fatalf("entries %q, want %q", rels(l), want)
	}
}

// These are the device shapes real sandboxes list: a bind of a host device, a mode
// 000 root-owned node under devpts, and the terminal device, none of which may be
// opened. This only reads attributes of one node at a time. The temp tree cannot
// hold a device without root.
func TestAttributesOfRealDeviceNodesReadWithoutOpeningThem(t *testing.T) {
	for _, p := range []string{"/dev/null", "/dev/tty", "/dev/pts/ptmx", "/dev/pts"} {
		t.Run(p, func(t *testing.T) {
			parent, err := unix.Open(filepath.Dir(p), unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
			if err != nil {
				t.Skipf("no %s here: %v", filepath.Dir(p), err)
			}
			defer func() { _ = unix.Close(parent) }()
			var st unix.Stat_t
			if err := lstatAt(parent, filepath.Base(p), &st); err != nil {
				t.Skipf("no %s here: %v", p, err)
			}
			if _, err := xattrDigestAt(parent, filepath.Base(p), &st); err != nil {
				t.Fatalf("%s (mode %#o): %v", p, st.Mode, err)
			}
		})
	}
}

func TestAttributeReadErrorsFailClosed(t *testing.T) {
	seed := func(t *testing.T) *sandbox {
		s := newSandbox(t)
		s.write("mnt/file", "x")
		s.setxattr("mnt/file", "user.a", "1")
		return s
	}
	// Each of these leaves attributes that could exist unread, so none can mean "no
	// attributes".
	for _, tc := range []struct {
		name  string
		nr    uint32
		errno unix.Errno
	}{
		{"list EIO", unix.SYS_LISTXATTR, unix.EIO},
		{"list EACCES", unix.SYS_LISTXATTR, unix.EACCES},
		{"list EPERM", unix.SYS_LISTXATTR, unix.EPERM},
		{"list ENODATA", unix.SYS_LISTXATTR, unix.ENODATA},
		{"list ERANGE", unix.SYS_LISTXATTR, unix.ERANGE},
		{"list ENOSYS", unix.SYS_LISTXATTR, unix.ENOSYS},
		{"listed name then EIO", unix.SYS_GETXATTR, unix.EIO},
		{"listed name then EACCES", unix.SYS_GETXATTR, unix.EACCES},
		{"listed name then ENODATA", unix.SYS_GETXATTR, unix.ENODATA},
		{"listed name then unsupported", unix.SYS_GETXATTR, unix.ENOTSUP},
		{"listed name then ERANGE", unix.SYS_GETXATTR, unix.ERANGE},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inChild(t, func() {
				s := seed(t)
				failSyscalls(t, map[uint32]unix.Errno{tc.nr: tc.errno})
				if _, err := Capture(context.Background(), strings.NewReader(s.info())); !errors.Is(err, tc.errno) {
					t.Fatalf("err = %v, want %v", err, tc.errno)
				}
			})
		})
	}
	// listxattr(2)'s answer for a filesystem with no attributes.
	t.Run("list unsupported is an empty set", func(t *testing.T) {
		inChild(t, func() {
			s := seed(t)
			failSyscalls(t, map[uint32]unix.Errno{unix.SYS_LISTXATTR: unix.ENOTSUP})
			if got := entry(t, listing(t, capture(t, s.info()), s.mount), "file").Xattrs; got != "" {
				t.Fatalf("digest %q, want empty", got)
			}
		})
	})
}

func TestManifestSurvivesJSON(t *testing.T) {
	s := newSandbox(t)
	s.write("mnt/name with \"quotes\"\nand newline", "content")
	s.symlink("target with space", "mnt/link")
	m, err := Capture(context.Background(), strings.NewReader(s.info(ro(s.path("mnt/link")))))
	if err != nil {
		t.Fatal(err)
	}
	if got := roundTrip(t, m); !reflect.DeepEqual(got, m) {
		t.Fatalf("round trip changed the manifest:\n got %+v\nwant %+v", got, m)
	}
}

// A real mount on another device cannot be made without privilege, so this gives
// the walk a root device that the temp tree is not on, which is how the walk
// sees such a mount. A directory there must stop it, unentered.
func TestWalkRefusesToEnterADirectoryOnAnotherDevice(t *testing.T) {
	s := newSandbox(t)
	s.mkdir("mnt/dir")
	s.write("mnt/dir/file", "")
	parent, base, err := openParent(s.mount)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = unix.Close(parent) }()
	var root unix.Stat_t
	if err := lstatAt(parent, base, &root); err != nil {
		t.Fatal(err)
	}
	w := walker{ctx: context.Background(), rootDev: root.Dev + 1}
	if err := w.visit(parent, base, "."); !errors.Is(err, ErrUnlistedMount) {
		t.Fatalf("err = %v, want ErrUnlistedMount", err)
	}
	if len(w.entries) != 1 {
		t.Fatalf("walk went past the boundary: %+v", w.entries)
	}
}

// Device nodes and owners other than the caller's need root. Both are skipped on
// an unprivileged run.
func TestCaptureRecordsADeviceNumber(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("mknod of a device needs root")
	}
	s := newSandbox(t)
	rdev := unix.Mkdev(1, 3)
	if err := unix.Mknod(s.path("mnt/null"), unix.S_IFCHR|0o666, int(rdev)); err != nil {
		t.Skipf("mknod refused here: %v", err)
	}
	got := entry(t, listing(t, capture(t, s.info()), s.mount), "null")
	if got.Type != TypeCharDev || got.Rdev != rdev {
		t.Fatalf("got %+v, want chardev with rdev %d", got, rdev)
	}
}

func TestCaptureRecordsTheOwner(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("chown needs root")
	}
	s := newSandbox(t)
	s.write("mnt/file", "")
	if err := os.Chown(s.path("mnt/file"), 1234, 5678); err != nil {
		t.Skipf("chown refused here: %v", err)
	}
	got := entry(t, listing(t, capture(t, s.info()), s.mount), "file")
	if got.UID != 1234 || got.GID != 5678 {
		t.Fatalf("got %+v, want uid 1234 gid 5678", got)
	}
}
