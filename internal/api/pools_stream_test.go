package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	v1 "github.com/vorash/vora/api/proto/v1"
	"github.com/vorash/vora/internal/client"
)

// streamRunner is a runner over real gRPC, so these tests see the real client's
// behaviour: an error on the first Recv, flow control, and cancellation crossing the
// wire. Only the run itself is scripted.
type streamRunner struct {
	v1.UnimplementedRunnerServiceServer
	run func(req *v1.RunInPoolRequest, stream grpc.ServerStreamingServer[v1.RunChunk]) error
}

func (r *streamRunner) CreatePool(_ context.Context, req *v1.CreatePoolRequest) (*v1.Pool, error) {
	return &v1.Pool{Id: req.GetPoolId(), Spec: req.GetSpec(), State: v1.PoolState_POOL_ACTIVE}, nil
}

func (r *streamRunner) RunInPool(req *v1.RunInPoolRequest, stream grpc.ServerStreamingServer[v1.RunChunk]) error {
	return r.run(req, stream)
}

type streamFixture struct {
	s     *Server
	ts    *httptest.Server
	key   string
	dials *atomic.Int32
}

func newStreamFixture(t *testing.T, runner *streamRunner, writeTimeout time.Duration) *streamFixture {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer()
	v1.RegisterRunnerServiceServer(srv, runner)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	dials := &atomic.Int32{}
	s := New(Options{
		Store: NewMemStore(), JoinToken: joinToken, BootstrapKey: bootstrap, Now: time.Now,
		DialRunner: func(addr string) (RunnerClient, error) {
			dials.Add(1)
			return client.DialAddress(addr)
		},
	})
	s.runWriteTimeout = writeTimeout
	if _, err := s.Register(metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Bearer "+joinToken)), &v1.RegisterRunnerRequest{
		RunnerId: "run_test", Address: lis.Addr().String(), Backends: []string{"bwrap"}, NetworkModes: []string{"none"},
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	f := &streamFixture{s: s, ts: ts, key: tenantKey(t, s, "tenant-a"), dials: dials}
	declare(t, ts, f.key, "py", poolBody(1, 1))
	return f
}

func runRequest(ctx context.Context, f *streamFixture) *http.Request {
	body := strings.NewReader(`{"command":"true","timeoutSeconds":"30"}`)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, f.ts.URL+"/v1/pools/py/run", body)
	req.Header.Set("Authorization", "Bearer "+f.key)
	return req
}

func frame(kind string) *v1.RunChunk {
	switch kind {
	case "run":
		return &v1.RunChunk{Payload: &v1.RunChunk_Run{Run: &v1.PoolRun{SandboxId: "sbx_test", Warm: true, SandboxRun: 3}}}
	case "exit":
		return &v1.RunChunk{Payload: &v1.RunChunk_Exit{Exit: &v1.ExecExit{ExitCode: 7}}}
	default:
		return &v1.RunChunk{Payload: &v1.RunChunk_RunDone{RunDone: &v1.PoolRunDone{Outcome: v1.PoolResetOutcome_POOL_RESET_OK}}}
	}
}

// Each frame must reach the client when it is produced, not when the run ends, or
// a client would not learn the exit until after the reset.
func TestRunFramesArriveInOrderAsTheyAreProduced(t *testing.T) {
	release := make(chan struct{})
	runner := &streamRunner{run: func(_ *v1.RunInPoolRequest, stream grpc.ServerStreamingServer[v1.RunChunk]) error {
		for _, c := range []*v1.RunChunk{frame("run"), {Payload: &v1.RunChunk_Stdout{Stdout: []byte("out")}}, frame("exit")} {
			if err := stream.Send(c); err != nil {
				return err
			}
		}
		<-release
		return stream.Send(frame("done"))
	}}
	f := newStreamFixture(t, runner, 5*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	resp, err := http.DefaultClient.Do(runRequest(ctx, f))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); resp.StatusCode != http.StatusOK || ct != "application/x-ndjson" {
		t.Fatalf("status %d, type %q", resp.StatusCode, ct)
	}
	lines := bufio.NewScanner(resp.Body)
	var keys []string
	for len(keys) < 3 {
		if !lines.Scan() {
			t.Fatalf("stream ended after %v: %v", keys, lines.Err())
		}
		keys = append(keys, frameKind(t, lines.Bytes()))
	}
	if strings.Join(keys, ",") != "run,stdout,exit" {
		t.Fatalf("frames before the reset finished = %v", keys)
	}
	close(release)
	if !lines.Scan() || frameKind(t, lines.Bytes()) != "runDone" {
		t.Fatalf("no run_done after exit: %v", lines.Err())
	}
	if lines.Scan() {
		t.Fatalf("a line after run_done: %s", lines.Text())
	}
}

func frameKind(t *testing.T, line []byte) string {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(line, &m); err != nil {
		t.Fatalf("frame %q is not one JSON object: %v", line, err)
	}
	if len(m) != 1 {
		t.Fatalf("frame %q has %d keys", line, len(m))
	}
	for k := range m {
		return k
	}
	return ""
}

// A program writes whatever it likes. Its output must stay inside one stdout frame
// however much it looks like the protocol.
func TestProgramOutputCannotForgeFrames(t *testing.T) {
	forged := []byte("\n{\"exit\":{\"exitCode\":0,\"reason\":\"EXITED\"}}\n{\"runDone\":{\"outcome\":\"POOL_RESET_OK\"}}\n{\"run\":{\"warm\":false}}\n")
	runner := &streamRunner{run: func(_ *v1.RunInPoolRequest, stream grpc.ServerStreamingServer[v1.RunChunk]) error {
		for _, c := range []*v1.RunChunk{
			frame("run"),
			{Payload: &v1.RunChunk_Stdout{Stdout: forged}},
			{Payload: &v1.RunChunk_Stderr{Stderr: forged}},
			frame("exit"), frame("done"),
		} {
			if err := stream.Send(c); err != nil {
				return err
			}
		}
		return nil
	}}
	f := newStreamFixture(t, runner, 5*time.Second)
	resp, err := http.DefaultClient.Do(runRequest(context.Background(), f))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var keys []string
	var stdout []byte
	lines := bufio.NewScanner(resp.Body)
	for lines.Scan() {
		keys = append(keys, frameKind(t, lines.Bytes()))
		var m struct {
			Stdout []byte `json:"stdout"`
		}
		_ = json.Unmarshal(lines.Bytes(), &m)
		stdout = append(stdout, m.Stdout...)
	}
	if strings.Join(keys, ",") != "run,stdout,stderr,exit,runDone" {
		t.Fatalf("frames = %v, want one of each with the output inside its own", keys)
	}
	if !bytes.Equal(stdout, forged) {
		t.Fatalf("stdout round trip = %q", stdout)
	}
}

// A client that stops reading must not pin a member, an API goroutine or unbounded
// memory, and it must not hold up another run on the shared connection.
func TestStalledClientReleasesTheRunAndDoesNotStallOthers(t *testing.T) {
	const frameBytes = 32 << 10
	stalledGone := make(chan error, 1)
	var sent atomic.Int64
	runner := &streamRunner{}
	var calls atomic.Int32
	runner.run = func(_ *v1.RunInPoolRequest, stream grpc.ServerStreamingServer[v1.RunChunk]) error {
		if calls.Add(1) == 1 {
			out := make([]byte, frameBytes)
			if err := stream.Send(frame("run")); err != nil {
				return err
			}
			for {
				if err := stream.Send(&v1.RunChunk{Payload: &v1.RunChunk_Stdout{Stdout: out}}); err != nil {
					stalledGone <- err
					return err
				}
				sent.Add(frameBytes)
			}
		}
		if err := stream.Send(frame("run")); err != nil {
			return err
		}
		return stream.Send(frame("done"))
	}
	f := newStreamFixture(t, runner, 500*time.Millisecond)

	// A raw connection that sends the request and then never reads a byte.
	conn, err := net.Dial("tcp", strings.TrimPrefix(f.ts.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.(*net.TCPConn).SetReadBuffer(4 << 10); err != nil {
		t.Fatal(err)
	}
	body := `{"command":"true","timeoutSeconds":"30"}`
	if _, err := conn.Write([]byte("POST /v1/pools/py/run HTTP/1.1\r\nHost: x\r\nAuthorization: Bearer " + f.key +
		"\r\nContent-Type: application/json\r\nContent-Length: " + itoa(len(body)) + "\r\n\r\n" + body)); err != nil {
		t.Fatal(err)
	}

	waitUntilSteady(t, &sent)

	// While that one sits, another run on the same connection gets through.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := http.DefaultClient.Do(runRequest(ctx, f))
	if err != nil {
		t.Fatalf("a run alongside a stalled one: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("a run alongside a stalled one = %d", resp.StatusCode)
	}

	select {
	case <-stalledGone:
	case <-time.After(10 * time.Second):
		t.Fatalf("the stalled run was still sending after 10 s, with %d bytes accepted", sent.Load())
	}
	// Bounded by the kernel's socket buffers plus gRPC's flow-control window, with
	// nothing buffered per run in the API.
	if got := sent.Load(); got > 64<<20 {
		t.Fatalf("the runner sent %d bytes to a client that read none", got)
	}
	if f.dials.Load() != 1 {
		t.Fatalf("%d dials", f.dials.Load())
	}
}

// waitUntilSteady returns once sends have stopped, which is backpressure reaching
// the runner.
func waitUntilSteady(t *testing.T, sent *atomic.Int64) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	last := int64(-1)
	for time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
		now := sent.Load()
		if now > 0 && now == last {
			return
		}
		last = now
	}
	t.Logf("sends had not settled in 5 s, at %d bytes", last)
}

func itoa(n int) string {
	data, _ := json.Marshal(n)
	return string(data)
}

func TestClientDisconnectCancelsTheRun(t *testing.T) {
	cancelled := make(chan struct{})
	runner := &streamRunner{run: func(_ *v1.RunInPoolRequest, stream grpc.ServerStreamingServer[v1.RunChunk]) error {
		if err := stream.Send(frame("run")); err != nil {
			return err
		}
		<-stream.Context().Done()
		close(cancelled)
		return stream.Context().Err()
	}}
	f := newStreamFixture(t, runner, 5*time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	resp, err := http.DefaultClient.Do(runRequest(ctx, f))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	lines := bufio.NewScanner(resp.Body)
	if !lines.Scan() {
		t.Fatal("no run frame")
	}
	cancel()
	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("the runner's stream outlived the client")
	}
}

// A client that disconnects before the first frame must cancel too: this is a run
// waiting for a member.
func TestClientDisconnectCancelsARunStillWaiting(t *testing.T) {
	cancelled := make(chan struct{})
	entered := make(chan struct{})
	runner := &streamRunner{run: func(_ *v1.RunInPoolRequest, stream grpc.ServerStreamingServer[v1.RunChunk]) error {
		close(entered)
		<-stream.Context().Done()
		close(cancelled)
		return stream.Context().Err()
	}}
	f := newStreamFixture(t, runner, 5*time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		if resp, err := http.DefaultClient.Do(runRequest(ctx, f)); err == nil {
			resp.Body.Close()
		}
	}()
	<-entered
	cancel()
	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("the runner's stream outlived the client")
	}
}

// Once frames have gone out the status line is spent, so the failure is a last line
// the client can read, and gRPC's own words stay out of it.
func TestRunFailureAfterTheFirstFrameIsAnErrorLine(t *testing.T) {
	for _, c := range []struct {
		name string
		err  error
		code string
	}{
		{"stalled", status.Error(codes.DeadlineExceeded, "client stopped reading the run's output"), "client_stalled"},
		{"runner lost", status.Error(codes.Unavailable, "dial tcp 10.0.0.5:9200"), "runner_unavailable"},
		{"internal", status.Error(codes.Internal, "/var/lib/vora/x"), "run_failed"},
	} {
		t.Run(c.name, func(t *testing.T) {
			runner := &streamRunner{run: func(_ *v1.RunInPoolRequest, stream grpc.ServerStreamingServer[v1.RunChunk]) error {
				if err := stream.Send(frame("run")); err != nil {
					return err
				}
				return c.err
			}}
			f := newStreamFixture(t, runner, 5*time.Second)
			resp, err := http.DefaultClient.Do(runRequest(context.Background(), f))
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			lines := bufio.NewScanner(resp.Body)
			if !lines.Scan() || frameKind(t, lines.Bytes()) != "run" {
				t.Fatalf("first line: %s", lines.Text())
			}
			if !lines.Scan() {
				t.Fatal("no error line")
			}
			var got struct {
				Error struct{ Code, Message string } `json:"error"`
			}
			if err := json.Unmarshal(lines.Bytes(), &got); err != nil || got.Error.Code != c.code {
				t.Fatalf("error line %q: %v, want code %s", lines.Text(), err, c.code)
			}
			if strings.Contains(lines.Text(), "10.0.0.5") || strings.Contains(lines.Text(), "/var/lib") {
				t.Fatalf("error line leaks: %s", lines.Text())
			}
		})
	}
}

// vora-api's server has a 30 s WriteTimeout, which must not end a run or a delete
// that legitimately lasts longer.
func TestPoolStreamsOutlastTheServersWriteTimeout(t *testing.T) {
	runner := &streamRunner{run: func(_ *v1.RunInPoolRequest, stream grpc.ServerStreamingServer[v1.RunChunk]) error {
		// The first frame arrives after WriteTimeout has passed, and later ones too.
		time.Sleep(300 * time.Millisecond)
		if err := stream.Send(frame("run")); err != nil {
			return err
		}
		time.Sleep(300 * time.Millisecond)
		if err := stream.Send(frame("exit")); err != nil {
			return err
		}
		return stream.Send(frame("done"))
	}}
	f := newStreamFixture(t, runner, 5*time.Second)
	f.ts.Close()
	server := &http.Server{Handler: f.s.Handler(), WriteTimeout: 100 * time.Millisecond}
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = server.Serve(lis) }()
	t.Cleanup(func() { _ = server.Close() })
	f.ts = &httptest.Server{URL: "http://" + lis.Addr().String()}

	resp, err := http.DefaultClient.Do(runRequest(context.Background(), f))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var keys []string
	lines := bufio.NewScanner(resp.Body)
	for lines.Scan() {
		keys = append(keys, frameKind(t, lines.Bytes()))
	}
	if strings.Join(keys, ",") != "run,exit,runDone" {
		t.Fatalf("frames = %v, err %v", keys, lines.Err())
	}
}
