package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	v1 "github.com/vorash/vora/api/proto/v1"
	"github.com/vorash/vora/internal/pool"
)

// sdkWireGolden is what the Python and TypeScript SDK suites decode. It is written
// from the real handlers, so a proto rename, a new field or a changed status goes
// red here, and a decoder that drifts from it goes red in the SDK.
//
// To regenerate: VORA_UPDATE_SDK_WIRE=1 go test ./internal/api -run TestSDKWireGolden
const sdkWireGolden = "../../sdk/testdata/pool_wire.json"

// sdkRunBodyLimit is mirrored by each SDK's check on a run's encoded body.
const sdkRunBodyLimit = 1 << 20

const (
	wirePoolID = "pool_01M6M3D3FRG49JM5BK3MD9Q0WY"
	// wireRedeclaredID is the id a route gets when its runner lost the pool and a
	// declare creates it again.
	wireRedeclaredID = "pool_01M6M3D3FRG49JM5BK3MD9Q0X1"
	wireUnsafeID     = "pool_01M6M3D3FRG49JM5BK3MD9Q0X2"
)

// wireResources is what a declare must set, or the API refuses it before any runner.
const wireResources = `"resources":{"cpuMillis":"100","memoryBytes":"1048576"}`

type wireAnswer struct {
	Status     int      `json:"status"`
	RetryAfter string   `json:"retryAfter,omitempty"`
	Body       string   `json:"body,omitempty"`
	Lines      []string `json:"lines,omitempty"`
}

type wireGolden struct {
	// Limits are the API's own bounds, so an SDK's client timeout test fails when one
	// of them moves past what that SDK waits for.
	Limits   map[string]int64           `json:"limits"`
	Runs     map[string]wireAnswer      `json:"runs"`
	Answers  map[string]wireAnswer      `json:"answers"`
	Messages map[string]json.RawMessage `json:"messages"`
}

// wireRunner adds the plain calls a scenario needs to the scripted streamRunner.
type wireRunner struct {
	*streamRunner
	get       func() (*v1.Pool, error)
	drained   bool
	deleteErr error
	// create overrides CreatePool once the fixture's own declare has gone through.
	create func(*v1.CreatePoolRequest) (*v1.Pool, error)
}

func (r *wireRunner) GetPool(context.Context, *v1.GetPoolRequest) (*v1.Pool, error) {
	return r.get()
}

func (r *wireRunner) DeletePool(context.Context, *v1.DeletePoolRequest) (*v1.DeletePoolResponse, error) {
	return &v1.DeletePoolResponse{Drained: r.drained}, r.deleteErr
}

// CreatePool answers as the real runner does for a name it already has: the fixture
// declares vora/base, and any other image is a different spec. The id is fixed, as
// the golden shows it.
func (r *wireRunner) CreatePool(_ context.Context, req *v1.CreatePoolRequest) (*v1.Pool, error) {
	if r.create != nil {
		return r.create(req)
	}
	if req.GetSpec().GetSandbox().GetImage() != "vora/base" {
		return nil, status.Errorf(codes.AlreadyExists, "pool %s exists with a different spec", req.GetSpec().GetName())
	}
	return &v1.Pool{Id: wirePoolID, Spec: req.GetSpec(), State: v1.PoolState_POOL_ACTIVE}, nil
}

// fullRun, fullDone, fullUsage and fullExit set every field, which
// assertEveryFieldSet holds them to, so a field added to the proto must be added
// here, and then every SDK must decode it.
func fullRun() *v1.PoolRun {
	return &v1.PoolRun{
		PoolId: wirePoolID, SandboxId: "sbx_01M6M3D3FRG49JM5BK3MD9Q0WZ", ExecutionId: "exe_01M6M3D3FRG49JM5BK3MD9Q0X0",
		Warm: true, StartupMs: 12, ResetMs: 7, SandboxRun: 3, Reuse: v1.PoolReuse_POOL_REUSE_RESET,
	}
}

func fullDone() *v1.PoolRunDone {
	return &v1.PoolRunDone{Outcome: v1.PoolResetOutcome_POOL_RESET_FAILED, ResetMs: 9, RetireCause: "reset_failed"}
}

func fullExit() *v1.ExecExit {
	return &v1.ExecExit{
		ExitCode: 137, Reason: v1.ExitReason_OOM_KILLED, ExecutionId: "exe_01M6M3D3FRG49JM5BK3MD9Q0X0",
		Usage: &v1.ExecUsage{CpuMillis: 21, MemoryPeakBytes: 9437184, MemoryPeakScope: v1.PeakScope_PEAK_SCOPE_WINDOW, OomKills: 1, PidsLimitHits: 2},
	}
}

func fullPool() *v1.Pool {
	size, maxSize, maxRuns := uint32(8), uint32(16), uint32(100)
	maxAge, maxRun, borrow, idle := int64(1800), int64(300), int64(2000), int64(3600)
	return &v1.Pool{
		Id: wirePoolID,
		Spec: &v1.PoolSpec{
			Name: "py", TenantId: "tenant-a", AgentId: "agent-a",
			Sandbox: &v1.SandboxSpec{
				Image: "vora/python", Backend: "bwrap",
				Resources: &v1.ResourceLimits{CpuMillis: 1000, MemoryBytes: 268435456, Pids: 16},
				Network:   &v1.NetworkPolicy{Mode: v1.NetworkMode_NETWORK_NONE},
			},
			Size: &size, MaxSize: &maxSize, MaxRuns: &maxRuns,
			MaxAgeSeconds: &maxAge, MaxRunSeconds: &maxRun, BorrowTimeoutMs: &borrow, IdleExpirySeconds: &idle,
			Reuse: v1.PoolReuse_POOL_REUSE_RESET,
		},
		State:    v1.PoolState_POOL_ACTIVE,
		RunnerId: "run_test",
		Stats: &v1.PoolStats{
			Starting: 1, Idle: 2, Busy: 3, Resetting: 4, Retiring: 5,
			RunsTotal: 6, WarmRunsTotal: 7, ResetsTotal: 8, ResetFailuresTotal: 9, DestroyedTotal: 10,
			LastStartError: "start failed", ExhaustedTotal: 11, StartFailedTotal: 12, Dormant: true,
		},
		CreatedAtUnixMs: 1758000000000,
		NotLoadedReason: "backend_missing",
	}
}

// assertEveryFieldSet fails on any field of m that is unset. It does not go into
// other messages: SandboxSpec has its own SDK decoder and its own tests.
func assertEveryFieldSet(t *testing.T, m proto.Message) {
	t.Helper()
	r := m.ProtoReflect()
	fields := r.Descriptor().Fields()
	for i := range fields.Len() {
		if fd := fields.Get(i); !r.Has(fd) {
			t.Errorf("%s.%s is unset in the golden scenario: set it, regenerate, and decode it in both SDKs", r.Descriptor().Name(), fd.Name())
		}
	}
}

func scripted(chunks []*v1.RunChunk, end error) func(*v1.RunInPoolRequest, grpc.ServerStreamingServer[v1.RunChunk]) error {
	return func(_ *v1.RunInPoolRequest, stream grpc.ServerStreamingServer[v1.RunChunk]) error {
		for _, c := range chunks {
			if err := stream.Send(c); err != nil {
				return err
			}
		}
		return end
	}
}

func runFrame(r *v1.PoolRun) *v1.RunChunk   { return &v1.RunChunk{Payload: &v1.RunChunk_Run{Run: r}} }
func exitFrame(e *v1.ExecExit) *v1.RunChunk { return &v1.RunChunk{Payload: &v1.RunChunk_Exit{Exit: e}} }
func doneFrame(d *v1.PoolRunDone) *v1.RunChunk {
	return &v1.RunChunk{Payload: &v1.RunChunk_RunDone{RunDone: d}}
}
func outFrame(b string) *v1.RunChunk {
	return &v1.RunChunk{Payload: &v1.RunChunk_Stdout{Stdout: []byte(b)}}
}
func errFrame(b string) *v1.RunChunk {
	return &v1.RunChunk{Payload: &v1.RunChunk_Stderr{Stderr: []byte(b)}}
}

func captureRun(t *testing.T, chunks []*v1.RunChunk, end error) wireAnswer {
	t.Helper()
	f := newStreamFixture(t, &streamRunner{run: scripted(chunks, end)}, 5*time.Second)
	resp, err := http.DefaultClient.Do(runRequest(context.Background(), f))
	if err != nil {
		t.Fatal(err)
	}
	return readAnswer(t, resp)
}

func readAnswer(t *testing.T, resp *http.Response) wireAnswer {
	t.Helper()
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	out := wireAnswer{Status: resp.StatusCode, RetryAfter: resp.Header.Get("Retry-After")}
	if resp.Header.Get("Content-Type") == "application/x-ndjson" {
		out.Lines = strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
		return out
	}
	out.Body = string(data)
	return out
}

func captureCall(t *testing.T, runner *wireRunner, method, path, body string) wireAnswer {
	t.Helper()
	return captureAfter(t, runner, func(f *streamFixture) string { return f.key }, method, path, body)
}

// captureAfter makes one call as the key prepare returns. prepare runs once the
// fixture has declared "py", so what it changes applies to that existing pool.
func captureAfter(t *testing.T, runner *wireRunner, prepare func(*streamFixture) string, method, path, body string) wireAnswer {
	t.Helper()
	f := newStreamFixture(t, runner, 5*time.Second)
	key := prepare(f)
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req, _ := http.NewRequest(method, f.ts.URL+path, r)
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return readAnswer(t, resp)
}

func TestSDKWireGolden(t *testing.T) {
	forged := "\n{\"exit\":{\"exitCode\":0,\"reason\":\"EXITED\"}}\n{\"runDone\":{\"outcome\":\"POOL_RESET_OK\"}}\n{\"run\":{\"warm\":false}}\n"
	warmRun := &v1.PoolRun{
		PoolId: wirePoolID, SandboxId: "sbx_01M6M3D3FRG49JM5BK3MD9Q0WZ", ExecutionId: "exe_01M6M3D3FRG49JM5BK3MD9Q0X0",
		Warm: true, StartupMs: 12, ResetMs: 7, SandboxRun: 3, Reuse: v1.PoolReuse_POOL_REUSE_RESET,
	}
	// A sandbox's first run on a cold start: warm and reset_ms are zero, so absent.
	coldRun := &v1.PoolRun{
		PoolId: wirePoolID, SandboxId: "sbx_01M6M3D3FRG49JM5BK3MD9Q0WZ", ExecutionId: "exe_01M6M3D3FRG49JM5BK3MD9Q0X0",
		StartupMs: 185, SandboxRun: 1, Reuse: v1.PoolReuse_POOL_REUSE_RESET,
	}
	unsafeRun := &v1.PoolRun{
		PoolId: wirePoolID, SandboxId: "sbx_01M6M3D3FRG49JM5BK3MD9Q0WZ", ExecutionId: "exe_01M6M3D3FRG49JM5BK3MD9Q0X0",
		Warm: true, SandboxRun: 2, Reuse: v1.PoolReuse_POOL_REUSE_UNSAFE_NO_RESET,
	}
	okExit := &v1.ExecExit{
		Reason: v1.ExitReason_EXITED, ExecutionId: "exe_01M6M3D3FRG49JM5BK3MD9Q0X0",
		Usage: &v1.ExecUsage{CpuMillis: 21, MemoryPeakBytes: 9437184, MemoryPeakScope: v1.PeakScope_PEAK_SCOPE_WINDOW},
	}
	bareExit := &v1.ExecExit{Reason: v1.ExitReason_EXITED, ExecutionId: "exe_01M6M3D3FRG49JM5BK3MD9Q0X0"}

	for _, m := range []proto.Message{fullRun(), fullDone(), fullExit(), fullExit().GetUsage(), fullPool(), fullPool().GetSpec(), fullPool().GetStats()} {
		assertEveryFieldSet(t, m)
	}
	allKinds := []*v1.RunChunk{runFrame(fullRun()), outFrame("out\n"), errFrame("warn\n"), exitFrame(fullExit()), doneFrame(fullDone())}
	seen := map[protoreflect.Name]bool{}
	for _, c := range allKinds {
		c.ProtoReflect().Range(func(fd protoreflect.FieldDescriptor, _ protoreflect.Value) bool {
			seen[fd.Name()] = true
			return true
		})
	}
	kinds := (&v1.RunChunk{}).ProtoReflect().Descriptor().Fields()
	for i := range kinds.Len() {
		if name := kinds.Get(i).Name(); !seen[name] {
			t.Errorf("RunChunk.%s has no frame in the golden's every_field run: add it, and decode it in both SDKs", name)
		}
	}

	notOnRunner := func() (*v1.Pool, error) { return nil, status.Error(codes.NotFound, "pool not found") }
	silent := func() (*v1.Pool, error) { return nil, status.Error(codes.Internal, "boom") }
	wellFormed := `{"sandbox":{"image":"vora/base","backend":"bwrap",` + wireResources + `}}`
	createAnswers := func(answer func(*v1.CreatePoolRequest) (*v1.Pool, error)) func(*streamFixture, *wireRunner) string {
		return func(f *streamFixture, runner *wireRunner) string {
			runner.create = answer
			return f.key
		}
	}
	captureCreate := func(prepare func(*streamFixture, *wireRunner) string) wireAnswer {
		runner := &wireRunner{streamRunner: &streamRunner{}}
		return captureAfter(t, runner, func(f *streamFixture) string { return prepare(f, runner) }, http.MethodPut, "/v1/pools/py", wellFormed)
	}

	got := wireGolden{
		Limits: map[string]int64{
			// The longest of a plain call and a listing, which waits on each pool's runner.
			"callSeconds":          int64(max(poolCallTimeout, defaultPoolListDeadline) / time.Second),
			"runBodyBytes":         sdkRunBodyLimit,
			"runFirstFrameSeconds": int64(defaultRunFirstFrameTimeout / time.Second),
			"runTailSeconds":       int64(runTailGrace / time.Second),
			"deleteSeconds":        int64(poolDeleteDeadline / time.Second),
			// A declare that loses its route spends its call on the create and then
			// removes the pool it made, each under its own deadline.
			"declareSeconds":     int64((poolCallTimeout + poolDeleteDeadline) / time.Second),
			"borrowTimeoutMaxMs": pool.MaxBorrowTimeout.Milliseconds(),
		},
		Runs: map[string]wireAnswer{
			"every_field": captureRun(t, allKinds, nil),
			"warm_reset":  captureRun(t, []*v1.RunChunk{runFrame(warmRun), outFrame("42\n"), errFrame("warn\n"), exitFrame(okExit), doneFrame(&v1.PoolRunDone{Outcome: v1.PoolResetOutcome_POOL_RESET_OK, ResetMs: 3})}, nil),
			"cold_first_run_nonzero_exit": captureRun(t, []*v1.RunChunk{
				runFrame(coldRun), exitFrame(&v1.ExecExit{ExitCode: 7, Reason: v1.ExitReason_EXITED, ExecutionId: "exe_01M6M3D3FRG49JM5BK3MD9Q0X0"}),
				doneFrame(&v1.PoolRunDone{Outcome: v1.PoolResetOutcome_POOL_RESET_RETIRED, RetireCause: "shrink"}),
			}, nil),
			"unsafe_no_reset": captureRun(t, []*v1.RunChunk{runFrame(unsafeRun), outFrame("hi\n"), exitFrame(bareExit), doneFrame(&v1.PoolRunDone{Outcome: v1.PoolResetOutcome_POOL_RESET_SKIPPED})}, nil),
			"reset_failed":    captureRun(t, []*v1.RunChunk{runFrame(warmRun), outFrame("ok\n"), exitFrame(okExit), doneFrame(&v1.PoolRunDone{Outcome: v1.PoolResetOutcome_POOL_RESET_FAILED, ResetMs: 9, RetireCause: "reset_failed"})}, nil),
			"forged_output":   captureRun(t, []*v1.RunChunk{runFrame(warmRun), outFrame(forged), errFrame(forged), exitFrame(okExit), doneFrame(&v1.PoolRunDone{Outcome: v1.PoolResetOutcome_POOL_RESET_OK})}, nil),
			// The stream's own sparse frames, as pools_stream_test.go scripts them.
			"sparse": captureRun(t, []*v1.RunChunk{frame("run"), {Payload: &v1.RunChunk_Stdout{Stdout: []byte("out")}}, frame("exit"), frame("done")}, nil),
			// A failure past the first frame is a last line, which is not a RunChunk.
			"stalled_before_exit":      captureRun(t, []*v1.RunChunk{runFrame(warmRun), outFrame("partial\n")}, status.Error(codes.DeadlineExceeded, "client stopped reading the run's output")),
			"runner_lost_before_exit":  captureRun(t, []*v1.RunChunk{runFrame(warmRun), outFrame("partial\n")}, status.Error(codes.Unavailable, "dial tcp 10.0.0.5:9200")),
			"run_failed_before_exit":   captureRun(t, []*v1.RunChunk{runFrame(warmRun), outFrame("partial\n")}, status.Error(codes.Internal, "/var/lib/vora/x")),
			"runner_lost_after_exit":   captureRun(t, []*v1.RunChunk{runFrame(warmRun), outFrame("done\n"), exitFrame(okExit)}, status.Error(codes.Unavailable, "dial tcp 10.0.0.5:9200")),
			"ended_after_exit_no_done": captureRun(t, []*v1.RunChunk{runFrame(warmRun), exitFrame(okExit)}, nil),
			"ended_before_exit":        captureRun(t, []*v1.RunChunk{runFrame(warmRun), outFrame("partial\n")}, nil),
			// Refused before the first frame, so a real status. The API can't tell from
			// these that nothing ran: a runner lost after the program started also lands
			// here, so a 503 or 504 on a run doesn't prove it.
			"pool_full":       captureRun(t, nil, status.Error(codes.ResourceExhausted, "pool: no sandbox free within the borrow timeout")),
			"runner_loading":  captureRun(t, nil, status.Error(codes.Unavailable, "pools are not loaded yet")),
			"draining":        captureRun(t, nil, status.Error(codes.FailedPrecondition, "pool: draining")),
			"not_loaded":      captureRun(t, nil, status.Error(codes.FailedPrecondition, "pool pool_01M6M3D3FRG49JM5BK3MD9Q0WY is not loaded on this runner: backend_missing")),
			"runner_too_slow": captureRun(t, nil, status.Error(codes.DeadlineExceeded, "context deadline exceeded")),
			"no_first_frame":  captureNoFirstFrame(t),
		},
		Answers: map[string]wireAnswer{
			"delete_draining": captureCall(t, &wireRunner{streamRunner: &streamRunner{}, drained: false}, http.MethodDelete, "/v1/pools/py", ""),
			"delete_gone":     captureCall(t, &wireRunner{streamRunner: &streamRunner{}, drained: true}, http.MethodDelete, "/v1/pools/py", ""),
			"declare_different_spec": captureCall(t, &wireRunner{
				streamRunner: &streamRunner{},
			}, http.MethodPut, "/v1/pools/py", `{"sandbox":{"image":"vora/other","backend":"bwrap",`+wireResources+`}}`),
			"declare_no_resources": captureCall(t, &wireRunner{streamRunner: &streamRunner{}}, http.MethodPut, "/v1/pools/py", `{"sandbox":{"image":"vora/base","backend":"bwrap"}}`),
			"declare_borrow_too_long": captureCreate(createAnswers(func(*v1.CreatePoolRequest) (*v1.Pool, error) {
				return nil, status.Error(codes.InvalidArgument, "pool: invalid spec: borrow_timeout must be between 1ms and 5m0s")
			})),
			"declare_runner_down": captureCreate(createAnswers(func(*v1.CreatePoolRequest) (*v1.Pool, error) {
				return nil, status.Error(codes.Unavailable, "pools are not loaded yet")
			})),
			"declare_lost_pool": captureCreate(createAnswers(func(req *v1.CreatePoolRequest) (*v1.Pool, error) {
				return &v1.Pool{Id: wireRedeclaredID, Spec: req.GetSpec(), State: v1.PoolState_POOL_ACTIVE, Stats: &v1.PoolStats{}}, nil
			})),
			"declare_other_agent": captureAgentBoundary(t),
			"get_not_on_runner":   captureCall(t, &wireRunner{streamRunner: &streamRunner{}, get: notOnRunner}, http.MethodGet, "/v1/pools/py", ""),
			"run_not_on_runner": captureCall(t, &wireRunner{
				streamRunner: &streamRunner{run: scripted(nil, status.Error(codes.NotFound, "pool not found"))},
				get:          notOnRunner,
			}, http.MethodPost, "/v1/pools/py/run", `{"command":"true","timeoutSeconds":"30"}`),
			"delete_not_on_runner": captureCall(t, &wireRunner{
				streamRunner: &streamRunner{},
				deleteErr:    status.Error(codes.NotFound, "pool not found"),
			}, http.MethodDelete, "/v1/pools/py", ""),
			"delete_runner_row_gone": captureAfter(t, &wireRunner{streamRunner: &streamRunner{}}, func(f *streamFixture) string {
				mem := f.s.store.(*MemStore)
				mem.mu.Lock()
				defer mem.mu.Unlock()
				delete(mem.runners, "run_test")
				return f.key
			}, http.MethodDelete, "/v1/pools/py", ""),
			"listing_not_on_runner":       captureCall(t, &wireRunner{streamRunner: &streamRunner{}, get: notOnRunner}, http.MethodGet, "/v1/pools", ""),
			"listing_unsafe_unanswered":   captureUnsafeListing(t, silent),
			"global_admin_without_tenant": adminWithoutTenant(t),
			"listing_runner_silent": captureCall(t, &wireRunner{
				streamRunner: &streamRunner{},
				get:          silent,
			}, http.MethodGet, "/v1/pools", ""),
			"get_not_loaded": captureCall(t, &wireRunner{
				streamRunner: &streamRunner{},
				get: func() (*v1.Pool, error) {
					return &v1.Pool{
						Id:              wirePoolID,
						Spec:            &v1.PoolSpec{Name: "py", TenantId: "tenant-a"},
						State:           v1.PoolState_POOL_NOT_LOADED,
						Stats:           &v1.PoolStats{},
						NotLoadedReason: "backend_missing",
					}, nil
				},
			}, http.MethodGet, "/v1/pools/py", ""),
			"get_active": captureCall(t, &wireRunner{
				streamRunner: &streamRunner{},
				get: func() (*v1.Pool, error) {
					return &v1.Pool{Id: wirePoolID, Spec: &v1.PoolSpec{Name: "py", TenantId: "tenant-a"}, State: v1.PoolState_POOL_ACTIVE, Stats: &v1.PoolStats{Idle: 1}}, nil
				},
			}, http.MethodGet, "/v1/pools/py", ""),
			"get_draining": captureCall(t, &wireRunner{
				streamRunner: &streamRunner{},
				get: func() (*v1.Pool, error) {
					return &v1.Pool{Id: wirePoolID, Spec: &v1.PoolSpec{Name: "py", TenantId: "tenant-a"}, State: v1.PoolState_POOL_DRAINING, Stats: &v1.PoolStats{Busy: 1}}, nil
				},
			}, http.MethodGet, "/v1/pools/py", ""),
		},
		Messages: map[string]json.RawMessage{},
	}
	for name, m := range map[string]proto.Message{"pool": fullPool(), "listing": &v1.ListPoolsResponse{Pools: []*v1.Pool{fullPool()}}} {
		data, err := protoJSON.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		got.Messages[name] = data
	}

	assertBodyLimit(t)

	rendered, err := json.MarshalIndent(got, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	rendered = append(rendered, '\n')
	if os.Getenv("VORA_UPDATE_SDK_WIRE") != "" {
		if err := os.MkdirAll("../../sdk/testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(sdkWireGolden, rendered, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(sdkWireGolden)
	if err != nil {
		t.Fatalf("%v: regenerate with VORA_UPDATE_SDK_WIRE=1", err)
	}
	if !bytes.Equal(canonicalJSON(t, want), canonicalJSON(t, rendered)) {
		t.Fatalf("the API's wire format moved away from %s. If that is intended, regenerate with VORA_UPDATE_SDK_WIRE=1 and make both SDK suites pass. Got:\n%s", sdkWireGolden, rendered)
	}
}

// protojson deliberately varies its whitespace between builds, so the golden is
// compared by value, and each line of a stream as a JSON value of its own.
func canonicalJSON(t *testing.T, data []byte) []byte {
	t.Helper()
	var g wireGolden
	if err := json.Unmarshal(data, &g); err != nil {
		t.Fatal(err)
	}
	for _, set := range []map[string]wireAnswer{g.Runs, g.Answers} {
		for name, a := range set {
			for i, line := range a.Lines {
				a.Lines[i] = canonicalValue(t, line)
			}
			if a.Body != "" {
				a.Body = canonicalValue(t, a.Body)
			}
			set[name] = a
		}
	}
	for name, raw := range g.Messages {
		g.Messages[name] = json.RawMessage(canonicalValue(t, string(raw)))
	}
	out, err := json.Marshal(g)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func canonicalValue(t *testing.T, s string) string {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatalf("%q: %v", s, err)
	}
	out, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

// captureNoFirstFrame is a runner that never sends a run's first frame, so the API
// gives up on its own bound and the client still gets a status.
func captureNoFirstFrame(t *testing.T) wireAnswer {
	t.Helper()
	f := newStreamFixture(t, &streamRunner{run: func(_ *v1.RunInPoolRequest, stream grpc.ServerStreamingServer[v1.RunChunk]) error {
		<-stream.Context().Done()
		return stream.Context().Err()
	}}, 5*time.Second)
	f.s.runFirstFrameTimeout = 50 * time.Millisecond
	resp, err := http.DefaultClient.Do(runRequest(context.Background(), f))
	if err != nil {
		t.Fatal(err)
	}
	return readAnswer(t, resp)
}

// captureAgentBoundary redeclares, with another agent's key, a name one agent owns.
func captureAgentBoundary(t *testing.T) wireAnswer {
	t.Helper()
	return captureAfter(t, &wireRunner{streamRunner: &streamRunner{}}, func(f *streamFixture) string {
		declare(t, f.ts, mintKey(t, f.s, "tenant-a", "agent-1", ScopeSandbox), "owned", poolBody(1, 1))
		return mintKey(t, f.s, "tenant-a", "agent-2", ScopeSandbox)
	}, http.MethodPut, "/v1/pools/owned", `{"sandbox":{"image":"vora/base","backend":"bwrap",`+wireResources+`}}`)
}

// captureUnsafeListing lists a pool an admin declared as unsafe, with its runner not
// answering. The route alone has to say that nothing is reset between its runs.
func captureUnsafeListing(t *testing.T, get func() (*v1.Pool, error)) wireAnswer {
	t.Helper()
	runner := &wireRunner{streamRunner: &streamRunner{}, get: get}
	return captureAfter(t, runner, func(f *streamFixture) string {
		runner.create = func(req *v1.CreatePoolRequest) (*v1.Pool, error) {
			return &v1.Pool{Id: wireUnsafeID, Spec: req.GetSpec(), State: v1.PoolState_POOL_ACTIVE}, nil
		}
		body := poolBody(1, 1)
		body["reuse"] = "POOL_REUSE_UNSAFE_NO_RESET"
		body["tenantId"] = "tenant-a"
		if got := call(t, http.MethodPut, f.ts.URL+"/v1/pools/unsafe", bootstrap, body); got.code != http.StatusCreated {
			t.Fatalf("admin declare = %d %s", got.code, got.body)
		}
		return f.key
	}, http.MethodGet, "/v1/pools", "")
}

func adminWithoutTenant(t *testing.T) wireAnswer {
	t.Helper()
	f := newStreamFixture(t, &streamRunner{}, 5*time.Second)
	req, _ := http.NewRequest(http.MethodGet, f.ts.URL+"/v1/pools/py", nil)
	req.Header.Set("Authorization", "Bearer "+bootstrap)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return readAnswer(t, resp)
}

// assertBodyLimit pins the number the SDKs refuse a run's body at: the API takes a
// body of exactly the limit, and one byte more is cut off and fails to parse.
func assertBodyLimit(t *testing.T) {
	t.Helper()
	f := newStreamFixture(t, &streamRunner{run: scripted([]*v1.RunChunk{frame("run"), frame("exit"), frame("done")}, nil)}, 5*time.Second)
	head, tail := `{"command":"true","timeoutSeconds":"30","cwd":"`, `"}`
	for _, c := range []struct {
		size, want int
	}{{sdkRunBodyLimit, http.StatusOK}, {sdkRunBodyLimit + 1, http.StatusBadRequest}} {
		// Padding inside a value, so a body cut off at any point is broken JSON.
		body := head + strings.Repeat("a", c.size-len(head)-len(tail)) + tail
		req, _ := http.NewRequest(http.MethodPost, f.ts.URL+"/v1/pools/py/run", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+f.key)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != c.want {
			t.Fatalf("a run body of %d bytes = %d, want %d", c.size, resp.StatusCode, c.want)
		}
	}
}
