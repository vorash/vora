package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/encoding/protojson"

	v1 "github.com/vorash/vora/api/proto/v1"
)

const (
	bootstrap = "vora_bootstrap_test_key"
	joinToken = "join-token-test"
)

type fakeRunner struct {
	specs  []*v1.SandboxSpec
	chunks []*v1.ExecChunk
	closed int
}

func (f *fakeRunner) CreateSandbox(_ context.Context, spec *v1.SandboxSpec) (*v1.Sandbox, error) {
	f.specs = append(f.specs, spec)
	return &v1.Sandbox{Id: "sbx_test", State: v1.SandboxState_RUNNING}, nil
}

func (f *fakeRunner) StartSandbox(_ context.Context, id string) (*v1.Sandbox, error) {
	return &v1.Sandbox{Id: id, State: v1.SandboxState_RUNNING}, nil
}

func (f *fakeRunner) GetSandbox(_ context.Context, id string) (*v1.Sandbox, error) {
	return &v1.Sandbox{Id: id}, nil
}

func (f *fakeRunner) ListSandboxes(_ context.Context, _ bool) ([]*v1.Sandbox, error) {
	return []*v1.Sandbox{{Id: "sbx_test"}}, nil
}

func (f *fakeRunner) DestroySandbox(_ context.Context, _ string) error { return nil }

func (f *fakeRunner) StatsSandbox(_ context.Context, _ string) (*v1.Stats, error) {
	return &v1.Stats{MemoryBytes: 1}, nil
}

func (f *fakeRunner) Events(_ context.Context, _ string) ([]*v1.SandboxEvent, error) {
	return []*v1.SandboxEvent{{SandboxId: "sbx_test", To: "RUNNING"}}, nil
}

func (f *fakeRunner) Execs(_ context.Context, _ string) ([]*v1.SandboxExec, error) {
	return []*v1.SandboxExec{{ExecutionId: "exe_test"}}, nil
}

func (f *fakeRunner) Exec(_ context.Context, _ *v1.ExecSandboxRequest, _ ...grpc.CallOption) (grpc.ServerStreamingClient[v1.ExecChunk], error) {
	return &fakeStream{chunks: f.chunks}, nil
}

func (f *fakeRunner) Close() error { f.closed++; return nil }

type fakeStream struct {
	grpc.ClientStream
	chunks []*v1.ExecChunk
	i      int
}

func (s *fakeStream) Recv() (*v1.ExecChunk, error) {
	if s.i >= len(s.chunks) {
		return nil, io.EOF
	}
	c := s.chunks[s.i]
	s.i++
	return c, nil
}

func newTestServer(t *testing.T, fr *fakeRunner) (*Server, *httptest.Server) {
	t.Helper()
	s := New(Options{
		Store:        NewMemStore(),
		JoinToken:    joinToken,
		BootstrapKey: bootstrap,
		Now:          time.Now,
		DialRunner: func(string) (RunnerClient, error) {
			return fr, nil
		},
	})
	if _, err := s.Register(metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Bearer "+joinToken)), &v1.RegisterRunnerRequest{
		RunnerId: "run_test", Address: "127.0.0.1:9200", Backends: []string{"bwrap"},
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return s, ts
}

func do(t *testing.T, method, url, key string, body any) *http.Response {
	t.Helper()
	var r io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		r = bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, url, r)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	return resp
}

func TestAuthRejectsBadKey(t *testing.T) {
	_, ts := newTestServer(t, &fakeRunner{})
	for _, key := range []string{"", "wrong", "vora_nope"} {
		resp := do(t, http.MethodGet, ts.URL+"/v1/runners", key, nil)
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("key %q: status = %d, want 401", key, resp.StatusCode)
		}
	}
	resp := do(t, http.MethodGet, ts.URL+"/v1/runners", bootstrap, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("bootstrap key: status = %d, want 200", resp.StatusCode)
	}
}

func TestCreateSandboxRoutesToRunner(t *testing.T) {
	fr := &fakeRunner{}
	s, ts := newTestServer(t, fr)
	spec := &v1.SandboxSpec{Image: "vora/base", Network: &v1.NetworkPolicy{Mode: v1.NetworkMode_NETWORK_NONE}}
	resp := do(t, http.MethodPost, ts.URL+"/v1/sandboxes", bootstrap, spec)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}
	var sb v1.Sandbox
	if err := protojson.Unmarshal(readBody(t, resp), &sb); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if sb.GetId() != "sbx_test" || sb.GetState() != v1.SandboxState_RUNNING {
		t.Fatalf("sandbox = %v", &sb)
	}
	if len(fr.specs) != 1 || fr.specs[0].GetImage() != "vora/base" {
		t.Fatalf("runner did not receive the spec: %+v", fr.specs)
	}
	if route, err := s.store.Route(context.Background(), "sbx_test"); err != nil || route != "run_test" {
		t.Fatalf("route = %q, %v", route, err)
	}
}

func TestExecStreamsNDJSON(t *testing.T) {
	fr := &fakeRunner{chunks: []*v1.ExecChunk{
		{Payload: &v1.ExecChunk_Stdout{Stdout: []byte("hello\n")}},
		{Payload: &v1.ExecChunk_Exit{Exit: &v1.ExecExit{ExitCode: 0, Reason: v1.ExitReason_EXITED}}},
	}}
	_, ts := newTestServer(t, fr)
	// Route the sandbox first.
	_ = do(t, http.MethodPost, ts.URL+"/v1/sandboxes", bootstrap, &v1.SandboxSpec{Image: "vora/base"}).Body.Close()

	resp := do(t, http.MethodPost, ts.URL+"/v1/sandboxes/sbx_test/exec", bootstrap,
		&v1.ExecRequest{Command: "/bin/echo", Args: []string{"hello"}})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var got []*v1.ExecChunk
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		chunk := &v1.ExecChunk{}
		if err := protojson.Unmarshal([]byte(line), chunk); err != nil {
			t.Fatalf("chunk %q: %v", line, err)
		}
		got = append(got, chunk)
	}
	if len(got) != 2 || string(got[0].GetStdout()) != "hello\n" || got[1].GetExit().GetExitCode() != 0 {
		t.Fatalf("chunks = %+v", got)
	}
}

func TestKeyCreationRequiresAdmin(t *testing.T) {
	_, ts := newTestServer(t, &fakeRunner{})

	// Create an admin key.
	resp := do(t, http.MethodPost, ts.URL+"/v1/keys", bootstrap, map[string]any{
		"name": "operator", "scopes": []string{ScopeAdmin, ScopeSandbox},
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create key status = %d", resp.StatusCode)
	}
	var created struct {
		Key    string `json:"key"`
		APIKey APIKey `json:"api_key"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatalf("decode: %v", err)
	}
	resp.Body.Close()
	if !strings.HasPrefix(created.Key, KeyPrefix) || created.APIKey.ID == "" {
		t.Fatalf("created = %+v", created)
	}

	// The new key works for sandbox scope.
	if r := do(t, http.MethodGet, ts.URL+"/v1/runners", created.Key, nil); r.StatusCode != http.StatusOK {
		t.Fatalf("new key status = %d", r.StatusCode)
	} else {
		r.Body.Close()
	}

	// A sandbox-only key is forbidden from key management.
	resp = do(t, http.MethodPost, ts.URL+"/v1/keys", bootstrap, map[string]any{"name": "sandbox-only", "scopes": []string{ScopeSandbox}})
	var sandboxKey struct {
		Key string `json:"key"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&sandboxKey)
	resp.Body.Close()
	r := do(t, http.MethodPost, ts.URL+"/v1/keys", sandboxKey.Key, map[string]any{"name": "x"})
	r.Body.Close()
	if r.StatusCode != http.StatusForbidden {
		t.Fatalf("sandbox key managing keys: status = %d, want 403", r.StatusCode)
	}
}

func TestRegistryRequiresJoinToken(t *testing.T) {
	s := New(Options{Store: NewMemStore(), JoinToken: joinToken, Now: time.Now})
	req := &v1.RegisterRunnerRequest{RunnerId: "run_x", Address: "127.0.0.1:9200"}

	if _, err := s.Register(context.Background(), req); err == nil {
		t.Fatal("expected registration without a token to fail")
	}
	badCtx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Bearer nope"))
	if _, err := s.Register(badCtx, req); err == nil {
		t.Fatal("expected a bad token to fail")
	}
	goodCtx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Bearer "+joinToken))
	if _, err := s.Register(goodCtx, req); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := s.Heartbeat(goodCtx, &v1.HeartbeatRequest{RunnerId: "run_x", SandboxesRunning: 2}); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	r, err := s.store.Runner(context.Background(), "run_x")
	if err != nil || r.Running != 2 {
		t.Fatalf("runner = %+v, %v", r, err)
	}
}

func readBody(t *testing.T, resp *http.Response) []byte {
	t.Helper()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return data
}