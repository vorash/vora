package cli

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"google.golang.org/grpc"

	v1 "github.com/vorash/vora/api/proto/v1"
)

func TestReadExecStdinForwardsPipedInput(t *testing.T) {
	got, err := readExecStdin(strings.NewReader("hello"), false)
	if err != nil {
		t.Fatalf("readExecStdin: %v", err)
	}
	if string(got) != "hello" {
		t.Fatalf("stdin = %q, want %q", got, "hello")
	}
}

func TestReadExecStdinSkipsTerminal(t *testing.T) {
	// /dev/null is a character device, the same mode a TTY reports, so it
	// stands in for an interactive stdin that must not be read.
	f, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	got, err := readExecStdin(f, false)
	if err != nil {
		t.Fatalf("readExecStdin: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("stdin = %q, want empty", got)
	}
}

func TestReadExecStdinForceReadsTerminal(t *testing.T) {
	f, err := os.OpenFile(os.DevNull, os.O_RDONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	got, err := readExecStdin(f, true)
	if err != nil {
		t.Fatalf("readExecStdin: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("stdin = %q, want empty", got)
	}
}

func TestReadExecStdinRejectsOversizedInput(t *testing.T) {
	_, err := readExecStdin(strings.NewReader(strings.Repeat("x", execStdinMax+1)), false)
	if err == nil {
		t.Fatal("readExecStdin: want error for oversized stdin, got nil")
	}
	if !strings.Contains(err.Error(), "stdin exceeds") {
		t.Fatalf("error = %q, want it to name the limit", err)
	}
}

func TestReadExecStdinAcceptsLimitExactly(t *testing.T) {
	got, err := readExecStdin(strings.NewReader(strings.Repeat("x", execStdinMax)), false)
	if err != nil {
		t.Fatalf("readExecStdin at the limit: %v", err)
	}
	if len(got) != execStdinMax {
		t.Fatalf("stdin length = %d, want %d", len(got), execStdinMax)
	}
}

// fakeRunner records the stdin a client sends and answers every exec with a
// clean exit, so a CLI run can be observed end to end over a real socket.
type fakeRunner struct {
	v1.UnimplementedRunnerServiceServer

	mu    sync.Mutex
	stdin []byte
	execs int
}

func (f *fakeRunner) ExecSandbox(req *v1.ExecSandboxRequest, stream grpc.ServerStreamingServer[v1.ExecChunk]) error {
	f.mu.Lock()
	f.stdin = append([]byte(nil), req.GetExec().GetStdin()...)
	f.execs++
	f.mu.Unlock()
	return stream.Send(&v1.ExecChunk{Payload: &v1.ExecChunk_Exit{Exit: &v1.ExecExit{ExitCode: 0}}})
}

func (f *fakeRunner) sentStdin() []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]byte(nil), f.stdin...)
}

func startFakeRunner(t *testing.T) (socket string, f *fakeRunner) {
	t.Helper()
	socket = filepath.Join(t.TempDir(), "runner.sock")
	lis, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := grpc.NewServer()
	f = &fakeRunner{}
	v1.RegisterRunnerServiceServer(srv, f)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	return socket, f
}

func TestExecForwardsStdinToSandbox(t *testing.T) {
	socket, f := startFakeRunner(t)
	silenceStdout(t)

	var out, errBuf strings.Builder
	code := run(
		[]string{"--socket", socket, "exec", "sbx_probe", "--", "cat"},
		strings.NewReader("STDIN-PROBE-1471"), &out, &errBuf,
	)
	if code != 0 {
		t.Fatalf("exit code = %d, stderr = %q", code, errBuf.String())
	}
	if got := string(f.sentStdin()); got != "STDIN-PROBE-1471" {
		t.Fatalf("stdin reaching the sandbox = %q, want %q", got, "STDIN-PROBE-1471")
	}
}

func TestExecForwardsEmptyStdinWhenNotATerminal(t *testing.T) {
	socket, f := startFakeRunner(t)
	silenceStdout(t)

	var out, errBuf strings.Builder
	if code := run(
		[]string{"--socket", socket, "exec", "sbx_probe", "--", "true"},
		strings.NewReader(""), &out, &errBuf,
	); code != 0 {
		t.Fatalf("exit code = %d, stderr = %q", code, errBuf.String())
	}
	if got := f.sentStdin(); len(got) != 0 {
		t.Fatalf("stdin = %q, want empty", got)
	}
}
