package runner

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	v1 "github.com/vorash/vora/api/proto/v1"
	"github.com/vorash/vora/internal/runtime"
	"github.com/vorash/vora/internal/sandbox"
	"github.com/vorash/vora/internal/ulid"
)

func fileTestSandbox(t *testing.T, s *Server, id string, state sandbox.State) {
	t.Helper()
	root := t.TempDir()
	ws := runtime.WorkspaceSpec{
		RootDir:      root,
		WorkspaceDir: filepath.Join(root, "workspace"),
		HomeDir:      filepath.Join(root, "home"),
	}
	for _, d := range []string{ws.WorkspaceDir, ws.HomeDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	rec := &sandbox.Record{
		ID:    id,
		State: state,
		Spec:  runtime.SandboxSpec{ID: id, Workspace: ws},
	}
	if err := s.store.Create(rec); err != nil {
		t.Fatal(err)
	}
}

func TestSandboxFileRoundTrip(t *testing.T) {
	s := testServer(t, runtime.NewRegistry())
	id := ulid.NewSandbox()
	fileTestSandbox(t, s, id, sandbox.StateRunning)
	ctx := context.Background()

	put, err := s.PutSandboxFile(ctx, &v1.PutSandboxFileRequest{
		SandboxId:     id,
		Path:          "/workspace/nested/a.txt",
		Content:       []byte("payload"),
		CreateParents: true,
	})
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	if put.GetBytesWritten() != int64(len("payload")) {
		t.Fatalf("bytes_written = %d", put.GetBytesWritten())
	}

	get, err := s.GetSandboxFile(ctx, &v1.GetSandboxFileRequest{SandboxId: id, Path: "/workspace/nested/a.txt"})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if string(get.GetContent()) != "payload" {
		t.Fatalf("content = %q", get.GetContent())
	}
}

func TestSandboxFileRequiresRunning(t *testing.T) {
	s := testServer(t, runtime.NewRegistry())
	id := ulid.NewSandbox()
	fileTestSandbox(t, s, id, sandbox.StateStopped)
	_, err := s.GetSandboxFile(context.Background(), &v1.GetSandboxFileRequest{SandboxId: id, Path: "/workspace/a"})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("code = %v, want FailedPrecondition (err=%v)", status.Code(err), err)
	}
}

func TestSandboxFileRejectsPathOutsideRoots(t *testing.T) {
	s := testServer(t, runtime.NewRegistry())
	id := ulid.NewSandbox()
	fileTestSandbox(t, s, id, sandbox.StateRunning)
	_, err := s.GetSandboxFile(context.Background(), &v1.GetSandboxFileRequest{SandboxId: id, Path: "/etc/passwd"})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("code = %v, want InvalidArgument (err=%v)", status.Code(err), err)
	}
}

func TestSandboxFileMissingFileIsNotFound(t *testing.T) {
	s := testServer(t, runtime.NewRegistry())
	id := ulid.NewSandbox()
	fileTestSandbox(t, s, id, sandbox.StateRunning)
	_, err := s.GetSandboxFile(context.Background(), &v1.GetSandboxFileRequest{SandboxId: id, Path: "/workspace/nope"})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("code = %v, want NotFound (err=%v)", status.Code(err), err)
	}
}
