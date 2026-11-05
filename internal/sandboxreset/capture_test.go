//go:build linux

package sandboxreset

import (
	"context"
	"errors"
	"io"
	"os"
	"reflect"
	"slices"
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
		if got := entry(t, l, rel); got != want {
			t.Errorf("%s: got %+v, want %+v", rel, got, want)
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

// bwrap's --dev binds /dev/null and friends as writable mounts of a single file.
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
