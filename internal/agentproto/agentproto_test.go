package agentproto

import (
	"context"
	"errors"
	"io"
	"net"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"

	v1 "github.com/vorash/vora/api/proto/v1"
	"github.com/vorash/vora/internal/runtime"
)

type fakeHandler struct {
	mu       sync.Mutex
	signals  []int32
	shutdown bool
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
