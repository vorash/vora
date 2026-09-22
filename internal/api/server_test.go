package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	v1 "github.com/vorash/vora/api/proto/v1"
	"github.com/vorash/vora/internal/sandbox"
	"github.com/vorash/vora/internal/ulid"
)

const (
	bootstrap = "vora_bootstrap_test_key"
	joinToken = "join-token-test"
)

type fakeRunner struct {
	specs          []*v1.SandboxSpec
	creds          [][]*v1.BrokerCredential
	sandboxes      []*v1.Sandbox
	chunks         []*v1.ExecChunk
	createErr      error
	startErr       error
	destroyErr     error
	destroyed      []string
	destroyCtxErr  error
	files          map[string][]byte
	snapshotted    []string
	deletedSnaps   []string
	forked         []string
	snapshotTenant string
	actors         []*v1.Actor
	closed         int
}

func (f *fakeRunner) CreateSandboxWithCredentials(_ context.Context, spec *v1.SandboxSpec, creds []*v1.BrokerCredential) (*v1.Sandbox, error) {
	f.specs = append(f.specs, spec)
	// Snapshot the credentials: the API zeroes its own copy as soon as the
	// call returns, and the test needs to see what the runner actually got.
	snapshot := make([]*v1.BrokerCredential, 0, len(creds))
	for _, c := range creds {
		snapshot = append(snapshot, proto.Clone(c).(*v1.BrokerCredential))
	}
	f.creds = append(f.creds, snapshot)
	if f.createErr != nil {
		return nil, f.createErr
	}
	return &v1.Sandbox{Id: "sbx_test", State: v1.SandboxState_RUNNING}, nil
}

func (f *fakeRunner) StartSandboxAs(_ context.Context, id string, actor *v1.Actor) (*v1.Sandbox, error) {
	f.actors = append(f.actors, actor)
	if f.startErr != nil {
		return nil, f.startErr
	}
	return &v1.Sandbox{Id: id, State: v1.SandboxState_RUNNING}, nil
}

func (f *fakeRunner) GetSandbox(_ context.Context, id string) (*v1.Sandbox, error) {
	return &v1.Sandbox{Id: id}, nil
}

func (f *fakeRunner) ListSandboxes(_ context.Context, _ bool) ([]*v1.Sandbox, error) {
	if f.sandboxes != nil {
		return f.sandboxes, nil
	}
	return []*v1.Sandbox{{Id: "sbx_test"}}, nil
}

func (f *fakeRunner) DestroySandboxAs(ctx context.Context, id string, actor *v1.Actor) error {
	f.destroyed = append(f.destroyed, id)
	f.actors = append(f.actors, actor)
	f.destroyCtxErr = ctx.Err()
	return f.destroyErr
}

func (f *fakeRunner) StatsSandbox(_ context.Context, _ string) (*v1.Stats, error) {
	return &v1.Stats{MemoryBytes: 1}, nil
}

func (f *fakeRunner) Events(_ context.Context, _ string) ([]*v1.SandboxEvent, error) {
	return []*v1.SandboxEvent{{SandboxId: "sbx_test", To: "RUNNING"}}, nil
}

func (f *fakeRunner) Execs(_ context.Context, _ string) ([]*v1.SandboxExec, error) {
	return []*v1.SandboxExec{{ExecutionId: "exe_test"}}, nil
}

func (f *fakeRunner) Egress(_ context.Context, _ string) ([]*v1.SandboxEgress, error) {
	return []*v1.SandboxEgress{{Host: "example.com", Allowed: true}}, nil
}

func (f *fakeRunner) GetFile(_ context.Context, _, path string) ([]byte, error) {
	content, ok := f.files[path]
	if !ok {
		return nil, errors.New("file not found")
	}
	return content, nil
}

func (f *fakeRunner) PutFile(_ context.Context, _, path string, content []byte, _ bool) (int64, error) {
	if f.files == nil {
		f.files = map[string][]byte{}
	}
	f.files[path] = content
	return int64(len(content)), nil
}

func (f *fakeRunner) Exec(_ context.Context, req *v1.ExecSandboxRequest, _ ...grpc.CallOption) (grpc.ServerStreamingClient[v1.ExecChunk], error) {
	f.actors = append(f.actors, req.GetActor())
	return &fakeStream{chunks: f.chunks}, nil
}

func (f *fakeRunner) Close() error { f.closed++; return nil }

func (f *fakeRunner) SnapshotSandbox(_ context.Context, id string) (*v1.Snapshot, error) {
	f.snapshotted = append(f.snapshotted, id)
	return &v1.Snapshot{
		Id:        "snap_test",
		SandboxId: id,
		TenantId:  f.snapshotTenant,
		RunnerId:  "run_test",
		Spec:      &v1.SandboxSpec{SandboxId: id, Resources: &v1.ResourceLimits{CpuMillis: 1, MemoryBytes: 1}},
	}, nil
}

func (f *fakeRunner) GetSnapshot(_ context.Context, id string) (*v1.Snapshot, error) {
	return &v1.Snapshot{
		Id:        id,
		SandboxId: "sbx_test",
		TenantId:  f.snapshotTenant,
		RunnerId:  "run_test",
		Spec:      &v1.SandboxSpec{SandboxId: "sbx_test", Resources: &v1.ResourceLimits{CpuMillis: 1, MemoryBytes: 1}},
	}, nil
}

func (f *fakeRunner) ListSnapshots(_ context.Context) ([]*v1.Snapshot, error) {
	return []*v1.Snapshot{{Id: "snap_test", SandboxId: "sbx_test", TenantId: f.snapshotTenant, RunnerId: "run_test", Spec: &v1.SandboxSpec{SandboxId: "sbx_test"}}}, nil
}

func (f *fakeRunner) DeleteSnapshot(_ context.Context, id string) error {
	f.deletedSnaps = append(f.deletedSnaps, id)
	return nil
}

func (f *fakeRunner) ForkSnapshotWithCredentials(_ context.Context, id string, _ []*v1.BrokerCredential, actor *v1.Actor) (*v1.Sandbox, error) {
	f.forked = append(f.forked, id)
	f.actors = append(f.actors, actor)
	return &v1.Sandbox{
		Id:    "sbx_fork",
		State: v1.SandboxState_RUNNING,
		Spec:  &v1.SandboxSpec{SandboxId: "sbx_fork", Resources: &v1.ResourceLimits{CpuMillis: 1, MemoryBytes: 1}},
	}, nil
}

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
	return newTestServerWithStore(t, fr, NewMemStore())
}

func newTestServerWithStore(t *testing.T, fr *fakeRunner, store Store) (*Server, *httptest.Server) {
	t.Helper()
	s := New(Options{
		Store:        store,
		JoinToken:    joinToken,
		BootstrapKey: bootstrap,
		Now:          time.Now,
		DialRunner: func(string) (RunnerClient, error) {
			return fr, nil
		},
	})
	if _, err := s.Register(metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Bearer "+joinToken)), &v1.RegisterRunnerRequest{
		RunnerId: "run_test", Address: "127.0.0.1:9200", Backends: []string{"bwrap"},
		NetworkModes: []string{"none", "restricted", "full", "private"},
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
	if route, err := s.store.Route(context.Background(), "sbx_test"); err != nil || route.RunnerID != "run_test" {
		t.Fatalf("route = %+v, %v", route, err)
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
	resp = do(t, http.MethodPost, ts.URL+"/v1/keys", bootstrap, map[string]any{
		"name": "sandbox-only", "tenant_id": "tenant-a", "scopes": []string{ScopeSandbox},
	})
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

func TestTenantSandboxIsolation(t *testing.T) {
	fr := &fakeRunner{sandboxes: []*v1.Sandbox{
		{Id: "sbx_a", Spec: &v1.SandboxSpec{TenantId: "tenant-a"}},
		{Id: "sbx_b", Spec: &v1.SandboxSpec{TenantId: "tenant-b"}},
		{Id: "sbx_global", Spec: &v1.SandboxSpec{}},
	}}
	s, ts := newTestServer(t, fr)
	tenantA := addTestKey(t, s, "tenant-a", ScopeSandbox)
	_ = s.store.SetRoute(context.Background(), SandboxRoute{SandboxID: "sbx_a", RunnerID: "run_test", TenantID: "tenant-a"})
	_ = s.store.SetRoute(context.Background(), SandboxRoute{SandboxID: "sbx_b", RunnerID: "run_test", TenantID: "tenant-b"})
	_ = s.store.SetRoute(context.Background(), SandboxRoute{SandboxID: "sbx_global", RunnerID: "run_test"})

	for _, path := range []string{
		"/v1/sandboxes/sbx_b",
		"/v1/sandboxes/sbx_b/stats",
		"/v1/sandboxes/sbx_b/events",
		"/v1/sandboxes/sbx_b/execs",
		"/v1/sandboxes/sbx_b/egress",
	} {
		resp := do(t, http.MethodGet, ts.URL+path, tenantA, nil)
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", path, resp.StatusCode)
		}
	}
	resp := do(t, http.MethodDelete, ts.URL+"/v1/sandboxes/sbx_b", tenantA, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("DELETE foreign sandbox = %d, want 404", resp.StatusCode)
	}
	resp = do(t, http.MethodPost, ts.URL+"/v1/sandboxes/sbx_b/exec", tenantA, &v1.ExecRequest{Command: "true"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("exec foreign sandbox = %d, want 404", resp.StatusCode)
	}

	resp = do(t, http.MethodGet, ts.URL+"/v1/sandboxes", tenantA, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list = %d", resp.StatusCode)
	}
	var listed v1.ListSandboxesResponse
	if err := protojson.Unmarshal(readBody(t, resp), &listed); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(listed.GetSandboxes()) != 1 || listed.GetSandboxes()[0].GetId() != "sbx_a" {
		t.Fatalf("tenant list = %+v", listed.GetSandboxes())
	}

	// Legacy/global routes fail closed for tenant keys but remain available to
	// the bootstrap global administrator.
	for key, want := range map[string]int{tenantA: http.StatusNotFound, bootstrap: http.StatusOK} {
		resp := do(t, http.MethodGet, ts.URL+"/v1/sandboxes/sbx_global", key, nil)
		resp.Body.Close()
		if resp.StatusCode != want {
			t.Errorf("global route with key %q = %d, want %d", key, resp.StatusCode, want)
		}
	}
}

func TestEmptyTenantSandboxKeysAreDenied(t *testing.T) {
	s, ts := newTestServer(t, &fakeRunner{})
	_ = s.store.SetRoute(context.Background(), SandboxRoute{SandboxID: "sbx_global", RunnerID: "run_test"})

	resp := do(t, http.MethodPost, ts.URL+"/v1/keys", bootstrap, map[string]any{
		"name": "untethered", "scopes": []string{ScopeSandbox},
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("create empty-tenant sandbox key = %d, want 400", resp.StatusCode)
	}

	untethered := addTestKey(t, s, "", ScopeSandbox)
	resp = do(t, http.MethodGet, ts.URL+"/v1/sandboxes/sbx_global", untethered, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("empty-tenant sandbox key = %d, want 404", resp.StatusCode)
	}
}

func TestTenantCreateCannotOverrideIdentity(t *testing.T) {
	fr := &fakeRunner{}
	s, ts := newTestServer(t, fr)
	key := addTestKey(t, s, "tenant-a", ScopeSandbox)

	resp := do(t, http.MethodPost, ts.URL+"/v1/sandboxes", key, &v1.SandboxSpec{Image: "vora/base", TenantId: "tenant-b"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-tenant create = %d, want 403", resp.StatusCode)
	}
	resp = do(t, http.MethodPost, ts.URL+"/v1/sandboxes", key, &v1.SandboxSpec{Image: "vora/base"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("own-tenant create = %d, want 201", resp.StatusCode)
	}
	if got := fr.specs[len(fr.specs)-1].GetTenantId(); got != "tenant-a" {
		t.Fatalf("runner tenant = %q, want tenant-a", got)
	}
}

func TestTenantAdminCannotManageOtherTenants(t *testing.T) {
	fr := &fakeRunner{}
	s, ts := newTestServer(t, fr)
	tenantAdmin := addTestKey(t, s, "tenant-a", ScopeAdmin, ScopeSandbox)
	foreign := addStoredTestKey(t, s, "tenant-b", ScopeSandbox)

	for _, body := range []map[string]any{
		{"name": "foreign", "tenant_id": "tenant-b"},
		{"name": "bad-scope", "scopes": []string{"root"}},
		{"name": "duplicate", "scopes": []string{ScopeSandbox, ScopeSandbox}},
	} {
		resp := do(t, http.MethodPost, ts.URL+"/v1/keys", tenantAdmin, body)
		resp.Body.Close()
		if resp.StatusCode < 400 {
			t.Errorf("create key %+v = %d, want rejection", body, resp.StatusCode)
		}
	}
	resp := do(t, http.MethodGet, ts.URL+"/v1/keys?tenant_id=tenant-b", tenantAdmin, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("list foreign keys = %d, want 403", resp.StatusCode)
	}
	resp = do(t, http.MethodDelete, ts.URL+"/v1/keys/"+foreign.ID, tenantAdmin, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("revoke foreign key = %d, want 404", resp.StatusCode)
	}
	resp = do(t, http.MethodGet, ts.URL+"/v1/runners", tenantAdmin, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("tenant runner inventory = %d, want 403", resp.StatusCode)
	}
	resp = do(t, http.MethodGet, ts.URL+"/v1/runners", bootstrap, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("global runner inventory = %d, want 200", resp.StatusCode)
	}
}

type failingRouteStore struct {
	*MemStore
	setErr    error
	deleteErr error
	partial   bool
}

func (s *failingRouteStore) SetRoute(ctx context.Context, route SandboxRoute) error {
	if s.partial {
		_ = s.MemStore.SetRoute(ctx, route)
	}
	return s.setErr
}

func (s *failingRouteStore) DeleteRoute(ctx context.Context, id string) error {
	if s.deleteErr != nil {
		return s.deleteErr
	}
	return s.MemStore.DeleteRoute(ctx, id)
}

func TestCreateSandboxCompensatesFailures(t *testing.T) {
	tests := []struct {
		name        string
		runner      *fakeRunner
		store       Store
		wantDestroy bool
	}{
		{name: "create failure", runner: &fakeRunner{createErr: errors.New("create failed")}, store: NewMemStore()},
		{name: "start failure", runner: &fakeRunner{startErr: errors.New("start failed")}, store: NewMemStore(), wantDestroy: true},
		{name: "cleanup failure", runner: &fakeRunner{startErr: errors.New("start failed"), destroyErr: errors.New("cleanup failed")}, store: NewMemStore(), wantDestroy: true},
		{name: "partial route failure", runner: &fakeRunner{}, store: &failingRouteStore{MemStore: NewMemStore(), setErr: errors.New("route failed"), partial: true}, wantDestroy: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, ts := newTestServerWithStore(t, tc.runner, tc.store)
			resp := do(t, http.MethodPost, ts.URL+"/v1/sandboxes", bootstrap, &v1.SandboxSpec{Image: "vora/base"})
			resp.Body.Close()
			if resp.StatusCode < 400 {
				t.Fatalf("status = %d, want failure", resp.StatusCode)
			}
			if got := len(tc.runner.destroyed) > 0; got != tc.wantDestroy {
				t.Fatalf("destroy attempted = %v, want %v", got, tc.wantDestroy)
			}
			if _, err := s.store.Route(context.Background(), "sbx_test"); !errors.Is(err, ErrNotFound) {
				t.Fatalf("route survived failed create: %v", err)
			}
		})
	}
}

func TestCreateCleanupIgnoresRequestCancellation(t *testing.T) {
	fr := &fakeRunner{startErr: context.Canceled}
	s, _ := newTestServer(t, fr)
	req := httptest.NewRequest(http.MethodPost, "/v1/sandboxes", bytes.NewReader([]byte(`{"image":"vora/base"}`)))
	req.Header.Set("Authorization", "Bearer "+bootstrap)
	ctx, cancel := context.WithCancel(req.Context())
	cancel()
	req = req.WithContext(ctx)
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code < 400 {
		t.Fatalf("status = %d, want failure", rr.Code)
	}
	if len(fr.destroyed) != 1 || fr.destroyCtxErr != nil {
		t.Fatalf("cleanup = %v, context error = %v", fr.destroyed, fr.destroyCtxErr)
	}
}

func addTestKey(t *testing.T, s *Server, tenant string, scopes ...string) string {
	t.Helper()
	plaintext, key := newStoredTestKey(t, tenant, scopes...)
	if err := s.store.CreateKey(context.Background(), key); err != nil {
		t.Fatal(err)
	}
	return plaintext
}

func addStoredTestKey(t *testing.T, s *Server, tenant string, scopes ...string) APIKey {
	t.Helper()
	_, key := newStoredTestKey(t, tenant, scopes...)
	if err := s.store.CreateKey(context.Background(), key); err != nil {
		t.Fatal(err)
	}
	return key
}

func newStoredTestKey(t *testing.T, tenant string, scopes ...string) (string, APIKey) {
	t.Helper()
	plaintext, hash, err := NewAPIKey()
	if err != nil {
		t.Fatal(err)
	}
	return plaintext, APIKey{ID: ulid.NewAPIKeyID(), Name: "test", TenantID: tenant, KeyHash: hash, Scopes: scopes, CreatedAt: time.Now()}
}

func readBody(t *testing.T, resp *http.Response) []byte {
	t.Helper()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return data
}

func TestFileRoutesRoundTrip(t *testing.T) {
	fr := &fakeRunner{}
	s, ts := newTestServer(t, fr)
	if err := s.store.SetRoute(context.Background(), SandboxRoute{SandboxID: "sbx_a", RunnerID: "run_test", TenantID: "tenant-a"}); err != nil {
		t.Fatal(err)
	}
	plaintext, key := newStoredTestKey(t, "tenant-a", ScopeSandbox)
	if err := s.store.CreateKey(context.Background(), key); err != nil {
		t.Fatal(err)
	}

	put, err := http.NewRequest(http.MethodPut, ts.URL+"/v1/sandboxes/sbx_a/files?path=/workspace/hello.txt", strings.NewReader("hi there"))
	if err != nil {
		t.Fatal(err)
	}
	put.Header.Set("Authorization", "Bearer "+plaintext)
	putResp, err := http.DefaultClient.Do(put)
	if err != nil {
		t.Fatal(err)
	}
	defer putResp.Body.Close()
	if putResp.StatusCode != http.StatusOK {
		t.Fatalf("PUT status = %d, want 200", putResp.StatusCode)
	}
	var putBody struct {
		BytesWritten int64 `json:"bytes_written"`
	}
	if err := json.Unmarshal(readBody(t, putResp), &putBody); err != nil {
		t.Fatal(err)
	}
	if putBody.BytesWritten != int64(len("hi there")) {
		t.Fatalf("bytes_written = %d, want %d", putBody.BytesWritten, len("hi there"))
	}
	if got := fr.files["/workspace/hello.txt"]; string(got) != "hi there" {
		t.Fatalf("runner got %q", got)
	}

	getResp := do(t, http.MethodGet, ts.URL+"/v1/sandboxes/sbx_a/files?path=/workspace/hello.txt", plaintext, nil)
	defer getResp.Body.Close()
	if getResp.StatusCode != http.StatusOK {
		t.Fatalf("GET status = %d, want 200", getResp.StatusCode)
	}
	if got := string(readBody(t, getResp)); got != "hi there" {
		t.Fatalf("GET body = %q", got)
	}
}

func TestFileRouteRequiresPath(t *testing.T) {
	_, ts := newTestServer(t, &fakeRunner{})
	resp := do(t, http.MethodGet, ts.URL+"/v1/sandboxes/sbx_a/files", bootstrap, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestFileRouteRejectsOversize(t *testing.T) {
	_, ts := newTestServer(t, &fakeRunner{})
	body := bytes.NewReader(make([]byte, sandbox.MaxTransferBytes+1))
	req, err := http.NewRequest(http.MethodPut, ts.URL+"/v1/sandboxes/sbx_a/files?path=/workspace/big", body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+bootstrap)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", resp.StatusCode)
	}
}

func TestFileRouteTenantIsolation(t *testing.T) {
	fr := &fakeRunner{files: map[string][]byte{"/workspace/secret": []byte("tenant-a only")}}
	s, ts := newTestServer(t, fr)
	if err := s.store.SetRoute(context.Background(), SandboxRoute{SandboxID: "sbx_a", RunnerID: "run_test", TenantID: "tenant-a"}); err != nil {
		t.Fatal(err)
	}
	plaintext, key := newStoredTestKey(t, "tenant-b", ScopeSandbox)
	if err := s.store.CreateKey(context.Background(), key); err != nil {
		t.Fatal(err)
	}
	resp := do(t, http.MethodGet, ts.URL+"/v1/sandboxes/sbx_a/files?path=/workspace/secret", plaintext, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

func TestFileRouteMissingSandbox(t *testing.T) {
	_, ts := newTestServer(t, &fakeRunner{})
	resp := do(t, http.MethodGet, ts.URL+"/v1/sandboxes/sbx_missing/files?path=/workspace/x", bootstrap, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

func TestTraceHeaderEchoesTraceID(t *testing.T) {
	sc := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    trace.TraceID{0x0a},
		SpanID:     trace.SpanID{0x0b},
		TraceFlags: trace.FlagsSampled,
	})
	req := httptest.NewRequest(http.MethodGet, "/v1/runners", nil)
	req = req.WithContext(trace.ContextWithSpanContext(req.Context(), sc))
	rec := httptest.NewRecorder()
	traceHeader(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).ServeHTTP(rec, req)
	if got := rec.Header().Get("X-Trace-Id"); got != sc.TraceID().String() {
		t.Fatalf("X-Trace-Id = %q, want %q", got, sc.TraceID().String())
	}
}

func TestTraceHeaderAbsentWithoutSpan(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/v1/runners", nil)
	rec := httptest.NewRecorder()
	traceHeader(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).ServeHTTP(rec, req)
	if got := rec.Header().Get("X-Trace-Id"); got != "" {
		t.Fatalf("X-Trace-Id = %q, want empty", got)
	}
}
