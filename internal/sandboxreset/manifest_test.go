//go:build linux

package sandboxreset

import (
	"slices"
	"testing"
)

func TestDiffSeparatesExtraFromChanged(t *testing.T) {
	want := []Entry{
		{Rel: ".", Type: TypeDir},
		{Rel: "gone", Type: TypeFile},
		{Rel: "same", Type: TypeFile, SHA256: "a"},
		{Rel: "swapped", Type: TypeFile, SHA256: "a"},
	}
	got := []Entry{
		{Rel: ".", Type: TypeDir},
		{Rel: "new", Type: TypeFile},
		{Rel: "same", Type: TypeFile, SHA256: "a"},
		{Rel: "swapped", Type: TypeFile, SHA256: "b"},
	}
	extra, changed := diff(want, got)
	if !slices.Equal(extra, []string{"new"}) {
		t.Errorf("extra = %q", extra)
	}
	if !slices.Equal(changed, []string{"gone", "swapped"}) {
		t.Errorf("changed = %q", changed)
	}
	if extra, changed := diff(want, want); extra != nil || changed != nil {
		t.Errorf("identical listings: extra %q, changed %q", extra, changed)
	}
	walked := []Entry{{Rel: "d", Type: TypeDir, MtimeNs: 5, AtimeNs: 6}}
	if _, changed := diff(walked, []Entry{{Rel: "d", Type: TypeDir, MtimeNs: 5, AtimeNs: 7}}); changed != nil {
		t.Errorf("a walk moved a directory's access time: changed %q", changed)
	}
	if _, changed := diff(walked, []Entry{{Rel: "d", Type: TypeDir, MtimeNs: 8, AtimeNs: 6}}); !slices.Equal(changed, []string{"d"}) {
		t.Errorf("a directory's mtime moved: changed %q", changed)
	}
}
