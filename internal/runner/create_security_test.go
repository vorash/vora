package runner

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	v1 "github.com/vorash/vora/api/proto/v1"
	"github.com/vorash/vora/internal/runtime"
	"github.com/vorash/vora/internal/sandbox"
	"github.com/vorash/vora/internal/ulid"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestCreateRejectsCallerIDsWithoutTouchingExistingWorkspace(t *testing.T) {
	s := testServer(t, testRegistry(t))
	id := ulid.NewSandbox()
	if err := s.store.Create(&sandbox.Record{ID: id, Spec: runtime.SandboxSpec{ID: id}}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(s.store.Dir(id), "owner-file")
	if err := os.WriteFile(path, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	ids := []string{id, ".hidden", "-flag", "a b", "x.slice", strings.Repeat("x", 300)}
	for _, requested := range ids {
		t.Run(requested, func(t *testing.T) {
			_, err := s.CreateSandbox(context.Background(), &v1.CreateSandboxRequest{Spec: &v1.SandboxSpec{SandboxId: requested}})
			if status.Code(err) != codes.InvalidArgument {
				t.Fatalf("caller ID %q: got %v, want InvalidArgument", requested, err)
			}
			if _, err := s.store.Load(id); err != nil {
				t.Fatalf("existing record lost: %v", err)
			}
			if data, err := os.ReadFile(path); err != nil || string(data) != "keep" {
				t.Fatalf("existing workspace file lost: %q, %v", data, err)
			}
		})
	}
}
