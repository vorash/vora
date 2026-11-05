//go:build linux

package sandboxreset

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"
)

func TestParseMountinfoReadsTheFieldsThatDecideAReset(t *testing.T) {
	const info = "27 1 0:23 / /tmp rw,nosuid,nodev shared:5 master:1 - tmpfs tmpfs rw,size=64k\n" +
		"1 0 8:1 /rootfs / ro,relatime - ext4 /dev/sda1 rw\n" +
		"30 27 8:1 /ipc/vora-init /vora/vora-init ro,nosuid - ext4 /dev/sda1 rw\n"
	got, err := parseMountinfo(strings.NewReader(info))
	if err != nil {
		t.Fatal(err)
	}
	want := []Mount{
		{ID: 1, ParentID: 0, Path: "/", FSType: "ext4", Writable: false},
		{ID: 27, ParentID: 1, Path: "/tmp", FSType: "tmpfs", Writable: true},
		{ID: 30, ParentID: 27, Path: "/vora/vora-init", FSType: "ext4", Writable: false},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

// A read-only bind shows ro only in the mount options, and a read-only superblock
// shows it only in the superblock options. Either one stops every write.
func TestWritableNeedsBothOptionFieldsToSayRW(t *testing.T) {
	for _, tc := range []struct {
		mountOpts, superOpts string
		want                 bool
	}{
		{"rw,nosuid", "rw,size=64k", true},
		{"ro,nosuid", "rw,size=64k", false},
		{"rw,nosuid", "ro,size=64k", false},
		{"ro", "ro", false},
	} {
		line := "5 1 0:1 / /x " + tc.mountOpts + " - tmpfs tmpfs " + tc.superOpts + "\n"
		got, err := parseMountinfo(strings.NewReader(line))
		if err != nil {
			t.Fatal(err)
		}
		if got[0].Writable != tc.want {
			t.Errorf("%s / %s: writable = %v, want %v", tc.mountOpts, tc.superOpts, got[0].Writable, tc.want)
		}
	}
}

func TestParseMountinfoDecodesKernelEscapes(t *testing.T) {
	for _, tc := range []struct{ raw, want string }{
		{`/a\040b`, "/a b"},
		{`/a\011b`, "/a\tb"},
		{`/a\012b`, "/a\nb"},
		{`/a\134b`, `/a\b`},
		{`/\040\040`, "/  "},
		{"/plain", "/plain"},
	} {
		got, err := parseMountinfo(strings.NewReader("5 1 0:1 / " + tc.raw + " rw - tmpfs tmpfs rw\n"))
		if err != nil {
			t.Errorf("%s: %v", tc.raw, err)
			continue
		}
		if got[0].Path != tc.want {
			t.Errorf("%s decoded to %q, want %q", tc.raw, got[0].Path, tc.want)
		}
	}
}

// The tables are mountinfo captured from real sandboxes on the test host, with
// the sandbox ID replaced. Their paths, such as /tmp and /root, are real paths on
// this machine too, so these tests only parse and select: nothing here may walk
// or restore them.
func realPlan(t *testing.T, file string) ([]Mount, map[string][]string) {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", file))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	table, err := parseMountinfo(f)
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}
	plan, err := selectMounts(table)
	if err != nil {
		t.Fatalf("select from %s: %v", file, err)
	}
	got := map[string][]string{}
	m := Manifest{Table: table}
	for _, p := range plan {
		nested := []string{}
		for rel := range p.Nested {
			nested = append(nested, rel)
		}
		sort.Strings(nested)
		got[p.Path] = nested

		l := MountListing{Path: p.Path, Entries: []Entry{{Rel: ".", Type: TypeDir}}}
		for _, rel := range nested {
			l.Entries = append(l.Entries, Entry{Rel: rel, Type: TypeMountpoint})
		}
		m.Mounts = append(m.Mounts, l)
	}
	// Capture ends in this check, so a real table's shape has to pass it as well.
	if err := m.validate(); err != nil {
		t.Fatalf("a listing per selected mount of %s does not validate: %v", file, err)
	}
	return table, got
}

// bwrap's root is read-only only in the mount options, its superblock being rw.
// /dev/shm has no mount, so it is an ordinary directory in /dev's listing. Each
// device is a writable bind of the host's devtmpfs and needs its own listing, and
// the read-only agent bind is only a name in /vora's.
func TestSelectMountsFromARealBwrapTable(t *testing.T) {
	table, got := realPlan(t, "mountinfo-bwrap.txt")
	want := map[string][]string{
		"/dev":         {"full", "null", "pts", "random", "tty", "urandom", "zero"},
		"/dev/full":    {},
		"/dev/null":    {},
		"/dev/pts":     {},
		"/dev/random":  {},
		"/dev/tty":     {},
		"/dev/urandom": {},
		"/dev/zero":    {},
		"/root":        {},
		"/tmp":         {},
		"/vora":        {"vora-init"},
		"/workspace":   {},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("selected %+v\nwant %+v", got, want)
	}
	if root := table[slices.IndexFunc(table, func(m Mount) bool { return m.Path == "/" })]; root.Writable || root.ParentID != 101 {
		t.Fatalf("root %+v, want read-only with parent 101 from outside the table", root)
	}
}

// gVisor has no bind per device and no /dev/shm mount. /vora is read-only 9p here,
// so unlike bwrap it gets no listing, and /sys is read-only as well as excluded.
func TestSelectMountsFromARealGVisorTable(t *testing.T) {
	table, got := realPlan(t, "mountinfo-gvisor.txt")
	want := map[string][]string{
		"/dev":       {"pts"},
		"/dev/pts":   {},
		"/root":      {},
		"/tmp":       {},
		"/workspace": {},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("selected %+v\nwant %+v", got, want)
	}
	if root := table[slices.IndexFunc(table, func(m Mount) bool { return m.Path == "/" })]; root.Writable || root.ParentID != 15 {
		t.Fatalf("root %+v, want read-only with parent 15 from outside the table", root)
	}
}

func planFor(t *testing.T, table []Mount) []plannedMount {
	t.Helper()
	plan, err := selectMounts(table)
	if err != nil {
		t.Fatalf("selectMounts: %v", err)
	}
	return plan
}

func planPaths(plan []plannedMount) []string {
	out := make([]string, 0, len(plan))
	for _, p := range plan {
		out = append(out, p.Path)
	}
	return out
}

func TestSelectMountsRejectsWritableRoot(t *testing.T) {
	_, err := selectMounts([]Mount{
		{ID: 1, Path: "/", Writable: true},
		{ID: 2, ParentID: 1, Path: "/tmp", Writable: true},
	})
	if !errors.Is(err, ErrRootWritable) {
		t.Fatalf("err = %v, want ErrRootWritable", err)
	}
}

func TestSelectMountsExcludesProcAndSysButNotLookalikes(t *testing.T) {
	plan := planFor(t, []Mount{
		{ID: 1, Path: "/"},
		{ID: 2, ParentID: 1, Path: "/proc", Writable: true},
		{ID: 3, ParentID: 2, Path: "/proc/sys/fs/binfmt_misc", Writable: true},
		{ID: 4, ParentID: 1, Path: "/sys", Writable: true},
		{ID: 5, ParentID: 4, Path: "/sys/fs/cgroup", Writable: true},
		{ID: 6, ParentID: 1, Path: "/procfoo", Writable: true},
		{ID: 7, ParentID: 1, Path: "/system", Writable: true},
		{ID: 8, ParentID: 1, Path: "/tmp", Writable: true},
	})
	if got, want := planPaths(plan), []string{"/procfoo", "/system", "/tmp"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("selected %q, want %q", got, want)
	}
}

func TestSelectMountsGivesANestedMountToItsNearestMount(t *testing.T) {
	plan := planFor(t, []Mount{
		{ID: 1, Path: "/"},
		{ID: 2, ParentID: 1, Path: "/dev", Writable: true},
		{ID: 3, ParentID: 2, Path: "/dev/pts", Writable: true},
		{ID: 4, ParentID: 3, Path: "/dev/pts/deep/sub", Writable: true},
		{ID: 5, ParentID: 2, Path: "/dev/null", Writable: true},
		{ID: 6, ParentID: 1, Path: "/vora", Writable: true},
		{ID: 7, ParentID: 6, Path: "/vora/vora-init"},
		{ID: 8, ParentID: 7, Path: "/etc"},
	})
	got := map[string]map[string]bool{}
	for _, p := range plan {
		got[p.Path] = p.Nested
	}
	want := map[string]map[string]bool{
		"/dev":              {"pts": true, "null": true},
		"/dev/null":         {},
		"/dev/pts":          {"deep/sub": true},
		"/dev/pts/deep/sub": {},
		"/vora":             {"vora-init": true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

// bwrap may leave its first root mount under the image's read-only bind. What a
// lookup reaches decides: a writable mount underneath cannot be written, and a
// writable mount on top can.
func TestSelectMountsJudgesStackedMountsByTheTopOne(t *testing.T) {
	tmp := Mount{ID: 9, ParentID: 2, Path: "/tmp", Writable: true}
	for _, table := range [][]Mount{
		{{ID: 1, ParentID: 0, Path: "/", Writable: true}, {ID: 2, ParentID: 1, Path: "/"}, tmp},
		{{ID: 2, ParentID: 1, Path: "/"}, {ID: 1, ParentID: 0, Path: "/", Writable: true}, tmp},
		{{ID: 1, ParentID: 0, Path: "/"}, {ID: 7, ParentID: 1, Path: "/", Writable: true}, {ID: 2, ParentID: 7, Path: "/"}, tmp},
	} {
		if got := planPaths(planFor(t, table)); !reflect.DeepEqual(got, []string{"/tmp"}) {
			t.Fatalf("selected %q from %+v", got, table)
		}
	}
	_, err := selectMounts([]Mount{{ID: 1, ParentID: 0, Path: "/"}, {ID: 2, ParentID: 1, Path: "/", Writable: true}, tmp})
	if !errors.Is(err, ErrRootWritable) {
		t.Fatalf("writable mount on top of a read-only root: err = %v, want ErrRootWritable", err)
	}
}

func TestSelectMountsRefusesAStackWithNoSingleTop(t *testing.T) {
	_, err := selectMounts([]Mount{
		{ID: 1, Path: "/"},
		{ID: 2, ParentID: 1, Path: "/tmp", Writable: true},
		{ID: 3, ParentID: 1, Path: "/tmp"},
	})
	if err == nil {
		t.Fatal("selected a mount from an ambiguous stack")
	}
}

// gVisor's generator writes a parentless mount's own ID as its parent. Read as a
// stack, that would hide the root. A real sandbox's root does have a parent, which
// is outside the table, so this shape is from the source and has not been seen.
func TestSelectMountsAcceptsARootThatIsItsOwnParent(t *testing.T) {
	plan := planFor(t, []Mount{
		{ID: 1, ParentID: 1, Path: "/"},
		{ID: 2, ParentID: 1, Path: "/tmp", Writable: true},
	})
	if got := planPaths(plan); !reflect.DeepEqual(got, []string{"/tmp"}) {
		t.Fatalf("selected %q", got)
	}
}
