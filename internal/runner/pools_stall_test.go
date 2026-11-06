package runner

import (
	"bytes"
	"context"
	"errors"
	"io"
	"runtime/pprof"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	v1 "github.com/vorash/vora/api/proto/v1"
	"github.com/vorash/vora/internal/runtime"
)

// floodStream is a program that writes until it is killed. Close is the kill.
type floodStream struct {
	chunk  []byte
	closed chan struct{}
	killed atomic.Bool
}

func newFloodStream() *floodStream {
	return &floodStream{chunk: make([]byte, 64<<10), closed: make(chan struct{})}
}

func (f *floodStream) Recv() (runtime.ExecChunk, error) {
	select {
	case <-f.closed:
		return runtime.ExecChunk{}, errors.New("exec killed")
	default:
		return runtime.ExecChunk{Stdout: f.chunk}, nil
	}
}

func (f *floodStream) Close() error {
	if !f.killed.Swap(true) {
		close(f.closed)
	}
	return nil
}

// goroutinesIn counts the live goroutines with a frame whose name contains fn.
func goroutinesIn(fn string) int {
	var buf bytes.Buffer
	_ = pprof.Lookup("goroutine").WriteTo(&buf, 2)
	n := 0
	for _, stack := range strings.Split(buf.String(), "\n\n") {
		if strings.Contains(stack, fn) {
			n++
		}
	}
	return n
}

// A client that reads the run frame and stops holds no member: TTL, reaper and
// DeletePool all wait on check-in, so a member pinned here would be pinned for
// as long as the client stays connected.
func TestClientThatStopsReadingCannotHoldAMember(t *testing.T) {
	h := newWarmHarness(t)
	flood := newFloodStream()
	h.backend.set(func(b *poolBackend) {
		b.execHook = func(context.Context, string, runtime.ExecRequest) (runtime.ExecStream, error) {
			return flood, nil
		}
	})
	p := h.create(poolSpec(1, 1))
	h.waitIdle(p.GetId(), 1)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	began := time.Now()
	stream, err := h.client.RunInPool(ctx, &v1.RunInPoolRequest{PoolId: p.GetId(), TenantId: h.tenant, Exec: &v1.ExecRequest{Command: "true", TimeoutSeconds: 1}})
	if err != nil {
		t.Fatal(err)
	}
	first, err := stream.Recv()
	if err != nil || first.GetRun() == nil {
		t.Fatalf("first frame = %v, %v; want the run frame", first, err)
	}
	id := first.GetRun().GetSandboxId()

	h.waitIdle(p.GetId(), 1)
	if bound := time.Second + h.s.execGrace + sendGrace + 3*time.Second; time.Since(began) > bound {
		t.Fatalf("member came back after %s; the bound is the hard deadline plus sendGrace, %s with slack", time.Since(began), bound)
	}
	if !flood.killed.Load() {
		t.Fatal("the member is back but the program's exec was never closed")
	}
	if box := h.backend.box(id); box.resets != 1 || h.backend.isGone(id) {
		t.Fatalf("resets = %d, gone = %v; want the member reset once and kept", box.resets, h.backend.isGone(id))
	}
	execs, err := h.s.audit.Execs(context.Background(), id)
	if err != nil || len(execs) != 1 || execs[0].Reason != runtime.ReasonTimeout || execs[0].ExitCode != timedOutExitCode {
		t.Fatalf("exec records = %+v (%v), want one TIMEOUT_EXCEEDED", execs, err)
	}
	if execs[0].StdoutBytes == 0 {
		t.Fatal("the record lost the bytes that were forwarded before the client stalled")
	}
	// The client is still connected: the stalled Send ended with the handler,
	// not with the client.
	waitFor(t, func() bool { return goroutinesIn("runSender") == 0 }, "the stalled send to return with the handler")

	resp, err := h.client.DeletePool(h.ctx(), &v1.DeletePoolRequest{PoolId: p.GetId(), TenantId: h.tenant, WaitMs: 5000})
	if err != nil || !resp.GetDrained() {
		t.Fatalf("DeletePool = %v, %v; want drained", resp, err)
	}

	// Should the client ever read again, what it gets ends in the handler's status.
	for {
		if _, err = stream.Recv(); err != nil {
			break
		}
	}
	if status.Code(err) != codes.DeadlineExceeded {
		t.Fatalf("stream end for the stalled client: %v, want DeadlineExceeded", err)
	}
}

// A hard deadline must not cost a client that keeps reading its trailing frames,
// however much output it was behind on.
func TestClientThatKeepsReadingGetsEveryFrameInOrder(t *testing.T) {
	h := newWarmHarness(t)
	const n = 128 // 8 MiB, far past gRPC's initial flow-control windows
	h.backend.set(func(b *poolBackend) { b.execHook = bigOutput(n) })
	p := h.create(poolSpec(1, 1))
	h.waitIdle(p.GetId(), 1)
	frames, err := h.run(h.ctx(), p.GetId(), 30)
	if err != nil {
		t.Fatalf("RunInPool: %v", err)
	}
	got := kinds(frames)
	if len(got) != n+3 || got[0] != "run" || got[n+1] != "exit" || got[n+2] != "run_done" {
		t.Fatalf("%d frames, ends %v; want run, %d stdout, exit, run_done", len(got), got[max(0, len(got)-2):], n)
	}
	for i, k := range got[1 : n+1] {
		if k != "stdout" {
			t.Fatalf("frame %d is %s, want stdout", i+1, k)
		}
	}
}

// A plain exec has no hard deadline: a client that pauses past timeout + grace
// still gets every byte and the exit, which is the output-tail guarantee.
func TestPlainExecWaitsForAClientThatPauses(t *testing.T) {
	h := newWarmHarness(t)
	const n = 64
	h.backend.set(func(b *poolBackend) { b.execHook = bigOutput(n) })
	id := h.startPlain()
	stream, err := h.client.ExecSandbox(h.ctx(), &v1.ExecSandboxRequest{SandboxId: id, Exec: &v1.ExecRequest{Command: "true", TimeoutSeconds: 1}})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Second + h.s.execGrace + sendGrace + 200*time.Millisecond)
	var got int
	var exit *v1.ExecExit
	for {
		c, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Recv after %d bytes: %v", got, err)
		}
		got += len(c.GetStdout())
		if c.GetExit() != nil {
			exit = c.GetExit()
		}
	}
	if got != n*64<<10 || exit.GetReason() != v1.ExitReason_EXITED {
		t.Fatalf("%d bytes, exit %v; want %d and EXITED", got, exit, n*64<<10)
	}
}

// recvAsync's goroutine is still reading when the exec ends at its deadline, so
// it has to be told nobody will take its last answer.
func TestExecEndingAtItsDeadlineLeaksNoReader(t *testing.T) {
	h := newWarmHarness(t)
	h.backend.set(func(b *poolBackend) {
		b.execHook = func(ctx context.Context, _ string, _ runtime.ExecRequest) (runtime.ExecStream, error) {
			return newCtxStream(ctx, true), nil
		}
	})
	p := h.create(poolSpec(1, 1))
	h.waitIdle(p.GetId(), 1)
	// An earlier test's reader may still be on its way out, and counting it would
	// make its exit look like this one's.
	waitFor(t, func() bool { return goroutinesIn("recvAsync") == 0 }, "earlier tests' readers to end")
	if _, err := h.run(h.ctx(), p.GetId(), 1); err != nil {
		t.Fatalf("RunInPool: %v", err)
	}
	waitFor(t, func() bool { return goroutinesIn("recvAsync") == 0 }, "the exec's reader to end")
}

// A client that goes away before the exit still gets its run on the audit trail.
func TestSendFailureBeforeExitIsRecorded(t *testing.T) {
	h := newWarmHarness(t)
	id := h.startPlain()
	backend, handle, err := h.s.handle(id)
	if err != nil {
		t.Fatal(err)
	}
	refused := errors.New("client gone")
	reason, err := h.s.runExec(context.Background(), id, backend, handle, runtime.ExecRequest{Command: "true"}, "", execOptions{},
		func(*v1.ExecChunk) error { return refused })
	if !errors.Is(err, refused) || reason != runtime.ReasonRuntimeFailure {
		t.Fatalf("runExec = %s, %v; want RuntimeFailure and the send's error", reason, err)
	}
	execs, err := h.s.audit.Execs(context.Background(), id)
	if err != nil || len(execs) != 1 || execs[0].Reason != runtime.ReasonRuntimeFailure {
		t.Fatalf("exec records = %+v (%v), want one RuntimeFailure", execs, err)
	}
}

// bigOutput is a program that writes n 64 KiB chunks and exits.
func bigOutput(n int) func(context.Context, string, runtime.ExecRequest) (runtime.ExecStream, error) {
	chunk := make([]byte, 64<<10)
	return func(ctx context.Context, _ string, _ runtime.ExecRequest) (runtime.ExecStream, error) {
		chunks := make([]runtime.ExecChunk, 0, n+1)
		for range n {
			chunks = append(chunks, runtime.ExecChunk{Stdout: chunk})
		}
		chunks = append(chunks, runtime.ExecChunk{Exit: &runtime.ExecResult{Reason: runtime.ReasonExited}})
		return newCtxStream(ctx, false, chunks...), nil
	}
}
