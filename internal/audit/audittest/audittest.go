// Package audittest is the contract every audit.Store must meet: what goes in
// comes back. Records are filled by reflection, so a field added to a record is
// covered the moment it exists, and a store that does not persist it fails here
// without anyone updating a list.
package audittest

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/vorash/vora/internal/audit"
	"github.com/vorash/vora/internal/runtime"
	"github.com/vorash/vora/internal/sandbox"
	"github.com/vorash/vora/internal/ulid"
)

var base = time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)

// Filled returns a T with every field set to a distinct non-zero value.
func Filled[T any](t *testing.T) T {
	t.Helper()
	var out T
	n := 0
	fill(t, reflect.ValueOf(&out).Elem(), &n, reflect.TypeOf(out).Name())
	return out
}

func fill(t *testing.T, v reflect.Value, n *int, path string) {
	t.Helper()
	*n++
	switch v.Interface().(type) {
	case time.Time:
		v.Set(reflect.ValueOf(base.Add(time.Duration(*n) * time.Second)))
		return
	case runtime.PeakScope:
		// A store may refuse a scope it does not know, and every value the kernel
		// can produce is one it must keep.
		v.Set(reflect.ValueOf(runtime.PeakLifetime))
		return
	}
	switch v.Kind() {
	case reflect.Struct:
		for i := range v.NumField() {
			fill(t, v.Field(i), n, path+"."+v.Type().Field(i).Name)
		}
	case reflect.Pointer:
		v.Set(reflect.New(v.Type().Elem()))
		fill(t, v.Elem(), n, path)
	case reflect.Slice:
		v.Set(reflect.MakeSlice(v.Type(), 2, 2))
		fill(t, v.Index(0), n, path+"[0]")
		fill(t, v.Index(1), n, path+"[1]")
	case reflect.String:
		v.SetString(fmt.Sprintf("v%d", *n))
	case reflect.Int, reflect.Int32, reflect.Int64:
		v.SetInt(int64(*n))
	case reflect.Bool:
		v.SetBool(true)
	default:
		t.Fatalf("audittest: %s has kind %s; teach fill to set it, so the field stays covered", path, v.Kind())
	}
}

// Diff walks want and got and names every field that differs. Times compare as
// instants, since a store may hand them back in another zone.
func Diff(want, got any) []string {
	var out []string
	diff(reflect.ValueOf(want), reflect.ValueOf(got), reflect.TypeOf(want).Name(), &out)
	return out
}

func diff(want, got reflect.Value, path string, out *[]string) {
	if w, ok := want.Interface().(time.Time); ok {
		if g := got.Interface().(time.Time); !w.Equal(g) {
			*out = append(*out, fmt.Sprintf("%s: want %v, got %v", path, w, g))
		}
		return
	}
	switch want.Kind() {
	case reflect.Struct:
		for i := range want.NumField() {
			diff(want.Field(i), got.Field(i), path+"."+want.Type().Field(i).Name, out)
		}
	case reflect.Pointer:
		switch {
		case want.IsNil() && got.IsNil():
		case want.IsNil():
			*out = append(*out, fmt.Sprintf("%s: want nil (not measured), got %v", path, got.Elem().Interface()))
		case got.IsNil():
			*out = append(*out, fmt.Sprintf("%s: want %v, got nil (not measured)", path, want.Elem().Interface()))
		default:
			diff(want.Elem(), got.Elem(), path, out)
		}
	case reflect.Slice:
		if want.Len() != got.Len() {
			*out = append(*out, fmt.Sprintf("%s: want %v, got %v", path, want.Interface(), got.Interface()))
			return
		}
		for i := range want.Len() {
			diff(want.Index(i), got.Index(i), fmt.Sprintf("%s[%d]", path, i), out)
		}
	default:
		if !want.Equal(got) {
			*out = append(*out, fmt.Sprintf("%s: want %v, got %v", path, want.Interface(), got.Interface()))
		}
	}
}

// setPointers replaces every pointer field of *rec: with a pointer to its zero
// value when zero is true, otherwise with nil.
func setPointers(rec any, zero bool) {
	v := reflect.ValueOf(rec).Elem()
	for i := range v.NumField() {
		f := v.Field(i)
		if f.Kind() != reflect.Pointer {
			continue
		}
		f.SetZero()
		if zero {
			f.Set(reflect.New(f.Type().Elem()))
		}
	}
}

// Run checks store against the contract. Each case uses its own sandbox id, so a
// store that already holds rows can run it.
func Run(t *testing.T, store audit.Store) {
	t.Helper()
	ctx := context.Background()

	execCase := func(name string, change func(*sandbox.ExecRecord)) {
		t.Run(name, func(t *testing.T) {
			want := Filled[sandbox.ExecRecord](t)
			want.SandboxID = ulid.NewSandbox()
			want.ExecutionID = ulid.NewExec()
			change(&want)
			if err := store.AppendExec(ctx, want.SandboxID, want); err != nil {
				t.Fatalf("AppendExec: %v", err)
			}
			got, err := store.Execs(ctx, want.SandboxID)
			if err != nil {
				t.Fatalf("Execs: %v", err)
			}
			if len(got) != 1 {
				t.Fatalf("Execs returned %d records, want 1", len(got))
			}
			for _, d := range Diff(want, got[0]) {
				t.Error(d)
			}
		})
	}
	execCase("every exec field survives", func(*sandbox.ExecRecord) {})
	// Zero and not measured are different answers: "this run used no CPU" against
	// "nobody knows", and a store that merges them turns a gap into a claim.
	execCase("a measured zero stays zero", func(r *sandbox.ExecRecord) { setPointers(r, true) })
	execCase("not measured stays not measured", func(r *sandbox.ExecRecord) {
		setPointers(r, false)
		r.MemoryPeakScope = runtime.PeakUnspecified
	})
	for _, scope := range []runtime.PeakScope{runtime.PeakWindow, runtime.PeakLifetime} {
		execCase(fmt.Sprintf("peak scope %d survives", scope), func(r *sandbox.ExecRecord) { r.MemoryPeakScope = scope })
	}

	t.Run("every event field survives", func(t *testing.T) {
		want := Filled[sandbox.Event](t)
		want.SandboxID = ulid.NewSandbox()
		if err := store.AppendEvent(ctx, want.SandboxID, want); err != nil {
			t.Fatalf("AppendEvent: %v", err)
		}
		got, err := store.Events(ctx, want.SandboxID)
		if err != nil {
			t.Fatalf("Events: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("Events returned %d records, want 1", len(got))
		}
		for _, d := range Diff(want, got[0]) {
			t.Error(d)
		}
	})

	t.Run("every egress field survives", func(t *testing.T) {
		want := Filled[sandbox.EgressRecord](t)
		want.SandboxID = ulid.NewSandbox()
		if err := store.AppendEgress(ctx, want.SandboxID, want); err != nil {
			t.Fatalf("AppendEgress: %v", err)
		}
		got, err := store.Egress(ctx, want.SandboxID)
		if err != nil {
			t.Fatalf("Egress: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("Egress returned %d records, want 1", len(got))
		}
		for _, d := range Diff(want, got[0]) {
			t.Error(d)
		}
	})
}
