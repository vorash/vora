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
}
