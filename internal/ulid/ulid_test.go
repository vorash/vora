// Package ulid contains tests for identifier generation and validation.
package ulid

import (
	"testing"
)

func TestNewPrefixedAndValid(t *testing.T) {
	id := NewSandbox()
	if !Valid(id) {
		t.Fatalf("generated id %q is not valid", id)
	}
	if id[:4] != "sbx_" {
		t.Fatalf("expected sbx_ prefix, got %q", id)
	}
}

func TestValidRejectsMalformed(t *testing.T) {
	cases := []string{
		"",
		"sbx_",
		"sbx_not-a-ulid",
		"bad_01ARZ3NDEKTSV4RRFFQ69G5FAV",
		"01ARZ3NDEKTSV4RRFFQ69G5FAV",
	}
	for _, c := range cases {
		if Valid(c) {
			t.Errorf("Valid(%q) = true, want false", c)
		}
	}
}

func TestIDsAreMonotonicByTime(t *testing.T) {
	a := NewExec()
	b := NewExec()
	if a >= b {
		t.Fatalf("expected ids to sort by creation order: %q >= %q", a, b)
	}
}
