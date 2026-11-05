package agentproto

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	v1 "github.com/vorash/vora/api/proto/v1"
	"github.com/vorash/vora/internal/runtime"
)

type fakeHandler struct {
	mu       sync.Mutex
	signals  []int32
	shutdown bool

	manifest   []byte
	captureErr error
	reset      []byte
	removed    int
	resetErr   error
}

func (f *fakeHandler) PID() int { return 1 }

func (f *fakeHandler) Exec(ctx context.Context, req *v1.ExecRequest, send func(*v1.ExecChunk) error) error {
	if req.GetCommand() == "fail" {
		return errors.New("boom")
	}
	if err := send(&v1.ExecChunk{Payload: &v1.ExecChunk_Stdout{Stdout: []byte("hello\n")}}); err != nil {
		return err
	}
	if err := send(&v1.ExecChunk{Payload: &v1.ExecChunk_Stderr{Stderr: []byte("warn\n")}}); err != nil {
		return err
	}
	return send(&v1.ExecChunk{Payload: &v1.ExecChunk_Exit{Exit: &v1.ExecExit{ExitCode: 0, Reason: v1.ExitReason_EXITED}}})
}

func (f *fakeHandler) Signal(_ context.Context, sig int32) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.signals = append(f.signals, sig)
	return nil
}

func (f *fakeHandler) Shutdown(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.shutdown = true
	return nil
}

func (f *fakeHandler) Capture(context.Context) ([]byte, error) {
	return f.manifest, f.captureErr
}

func (f *fakeHandler) Reset(_ context.Context, manifest []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reset = manifest
	return f.removed, f.resetErr
}

func startAgent(t *testing.T) (*Client, *fakeHandler) {
	t.Helper()
	socket := filepath.Join(t.TempDir(), "control.sock")
	lis, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	h := &fakeHandler{}
	grpcServer := grpc.NewServer()
	v1.RegisterAgentServiceServer(grpcServer, NewServer(h))
	go func() { _ = grpcServer.Serve(lis) }()
	t.Cleanup(func() {
		grpcServer.Stop()
		lis.Close()
	})

	client, err := Dial(socket)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { client.Close() })
	return client, h
}

// replacementSocketEnv tells a re-executed test binary to act as the forged
// agent in TestAgentRejectsReplacedControlSocketOnReconnect.
const replacementSocketEnv = "VORA_AGENTPROTO_REPLACEMENT_SOCKET"

// TestHelperReplacementAgent is not a test: it is the forged agent, run in its
// own process. It serves a valid AgentService on the socket path, reports
// ready, and exits when the parent closes its stdin.
func TestHelperReplacementAgent(t *testing.T) {
	socket := os.Getenv(replacementSocketEnv)
	if socket == "" {
		t.Skip("helper process for TestAgentRejectsReplacedControlSocketOnReconnect")
	}
	l, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer()
	v1.RegisterAgentServiceServer(server, NewServer(&fakeHandler{}))
	go func() { _ = server.Serve(l) }()
	fmt.Println("ready")
	_, _ = io.Copy(io.Discard, os.Stdin)
	server.Stop()
}

// A workload can replace /vora/control.sock with its own listener. The runner
// must refuse to talk to it on reconnect, or the workload could forge exec
// results and exit codes in the audit trail. The replacement is served from a
// separate process, as a workload's would be: the socket inode alone is not a
// reliable identity (filesystems reuse a freed inode number for the next
// socket), so this pins the peer-credential check that actually holds.
func TestAgentRejectsReplacedControlSocketOnReconnect(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "control.sock")
	original, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer()
	v1.RegisterAgentServiceServer(server, NewServer(&fakeHandler{}))
	go func() { _ = server.Serve(original) }()

	client, err := Dial(socket)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	if err := client.WaitReady(ctx, time.Second); err != nil {
		t.Fatal(err)
	}
	cancel()
	server.Stop()
	original.Close()
	if err := os.Remove(socket); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}

	forged := exec.Command(os.Args[0], "-test.run=^TestHelperReplacementAgent$")
	forged.Env = append(os.Environ(), replacementSocketEnv+"="+socket)
	stdin, err := forged.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := forged.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := forged.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		stdin.Close()
		_ = forged.Wait()
	}()
	ready := make(chan bool, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			if scanner.Text() == "ready" {
				ready <- true
				_, _ = io.Copy(io.Discard, stdout)
				return
			}
		}
		ready <- false
	}()
	select {
	case ok := <-ready:
		if !ok {
			t.Fatal("replacement agent exited before serving")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("replacement agent did not start")
	}

	ctx, cancel = context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := client.Ping(ctx); err == nil {
		t.Fatal("agent accepted a replacement socket and forged a response")
	}
}

func TestAgentRoundTrip(t *testing.T) {
	client, h := startAgent(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := client.WaitReady(ctx, 2*time.Second); err != nil {
		t.Fatalf("WaitReady: %v", err)
	}
	pid, err := client.Ping(ctx)
	if err != nil || pid != 1 {
		t.Fatalf("Ping = %d, %v", pid, err)
	}

	stream, err := client.Exec(ctx, &v1.ExecRequest{Command: "echo", Args: []string{"hi"}})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	defer stream.Close()

	var stdout, stderr []byte
	var exit *runtime.ExecResult
	for {
		chunk, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
		stdout = append(stdout, chunk.Stdout...)
		stderr = append(stderr, chunk.Stderr...)
		if chunk.Exit != nil {
			exit = chunk.Exit
		}
	}
	if string(stdout) != "hello\n" || string(stderr) != "warn\n" {
		t.Fatalf("stdout=%q stderr=%q", stdout, stderr)
	}
	if exit == nil || exit.ExitCode != 0 || exit.Reason != runtime.ReasonExited {
		t.Fatalf("exit = %+v", exit)
	}

	if err := client.Signal(ctx, 15); err != nil {
		t.Fatalf("Signal: %v", err)
	}
	if err := client.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.signals) != 1 || h.signals[0] != 15 {
		t.Fatalf("signals = %v", h.signals)
	}
	if !h.shutdown {
		t.Fatal("handler did not receive Shutdown")
	}
}

// The runner dials before vora-init has bound the control socket, so its first
// Ping always fails. The agent must be reachable within a poll interval of the
// socket appearing, not after gRPC's default 1 s reconnect backoff (#114).
func TestAgentReadySoonAfterSocketAppears(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "control.sock")
	client, err := Dial(socket)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := client.Ping(ctx); err == nil {
		t.Fatal("Ping succeeded before the agent was listening")
	}

	time.Sleep(50 * time.Millisecond)
	lis, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer()
	v1.RegisterAgentServiceServer(server, NewServer(&fakeHandler{}))
	go func() { _ = server.Serve(lis) }()
	defer server.Stop()

	if err := client.WaitReady(ctx, 3*time.Second); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("agent ready %s after the first dial; the socket appeared after 50ms", elapsed)
	}
}

func TestCaptureReturnsTheManifest(t *testing.T) {
	client, h := startAgent(t)
	h.manifest = []byte(`{"mounts":[]}`)
	got, err := client.Capture(t.Context())
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if !bytes.Equal(got, h.manifest) {
		t.Fatalf("manifest = %q, want %q", got, h.manifest)
	}
}

// The runner reuses a sandbox whenever Reset returns nil, so the agent's failure must
// arrive as an error and not as a response it could read past.
func TestAgentFailuresArriveAsErrors(t *testing.T) {
	client, h := startAgent(t)
	h.captureErr, h.resetErr, h.removed = errors.New("capture failed"), errors.New("reset failed"), 7

	if manifest, err := client.Capture(t.Context()); err == nil || !strings.Contains(err.Error(), "capture failed") || manifest != nil {
		t.Fatalf("Capture = %q, %v", manifest, err)
	}
	if removed, err := client.Reset(t.Context(), []byte("{}")); err == nil || !strings.Contains(err.Error(), "reset failed") || removed != 0 {
		t.Fatalf("Reset = %d, %v", removed, err)
	}
}

func TestResetCarriesTheManifestAndTheCount(t *testing.T) {
	client, h := startAgent(t)
	h.removed = 7
	manifest := []byte(`{"mounts":[]}`)
	removed, err := client.Reset(t.Context(), manifest)
	if err != nil {
		t.Fatalf("Reset: %v", err)
	}
	if removed != 7 {
		t.Fatalf("removed = %d, want 7", removed)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if !bytes.Equal(h.reset, manifest) {
		t.Fatalf("agent got %q, want %q", h.reset, manifest)
	}
}

// MaxManifestBytes claims to be what the transport carries. At the limit both RPCs must
// work through real gRPC limits, so a gRPC upgrade that lowers them fails here, and one
// byte over must name the sizes instead of leaving a template's owner to read a transport
// error from whichever end hit it.
func TestManifestSizeLimit(t *testing.T) {
	client, h := startAgent(t)
	atLimit := bytes.Repeat([]byte("x"), MaxManifestBytes)
	over := bytes.Repeat([]byte("x"), MaxManifestBytes+1)

	t.Run("capture at the limit", func(t *testing.T) {
		h.manifest = atLimit
		got, err := client.Capture(t.Context())
		if err != nil {
			t.Fatalf("Capture: %v", err)
		}
		if len(got) != MaxManifestBytes {
			t.Fatalf("got %d bytes", len(got))
		}
	})
	t.Run("reset at the limit", func(t *testing.T) {
		if _, err := client.Reset(t.Context(), atLimit); err != nil {
			t.Fatalf("Reset: %v", err)
		}
		h.mu.Lock()
		defer h.mu.Unlock()
		if len(h.reset) != MaxManifestBytes {
			t.Fatalf("agent got %d bytes", len(h.reset))
		}
	})
	for name, call := range map[string]func() error{
		"capture over the limit": func() error {
			h.manifest = over
			_, err := client.Capture(t.Context())
			return err
		},
		"reset over the limit": func() error {
			h.mu.Lock()
			h.reset = nil
			h.mu.Unlock()
			_, err := client.Reset(t.Context(), over)
			h.mu.Lock()
			defer h.mu.Unlock()
			if h.reset != nil {
				t.Error("the agent received a manifest over the limit")
			}
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			err := call()
			if status.Code(err) != codes.ResourceExhausted {
				t.Fatalf("code = %v (%v), want ResourceExhausted", status.Code(err), err)
			}
			for _, want := range []string{"manifest", strconv.Itoa(MaxManifestBytes + 1), strconv.Itoa(MaxManifestBytes)} {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("error %q does not name %q", err, want)
				}
			}
		})
	}
}

// A forged call can send anything. Past the transport limit the agent must not even see
// it, so the size of a request stays bounded no matter who sends it.
func TestAgentRefusesAnOversizedRequestItself(t *testing.T) {
	client, h := startAgent(t)
	_, err := client.stub.Reset(t.Context(), &v1.ResetRequest{Manifest: make([]byte, 4<<20+1)})
	if status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("code = %v (%v), want ResourceExhausted", status.Code(err), err)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.reset != nil {
		t.Fatal("the handler saw the request")
	}
}
