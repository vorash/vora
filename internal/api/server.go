package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	v1 "github.com/vorash/vora/api/proto/v1"
	"github.com/vorash/vora/internal/client"
	"github.com/vorash/vora/internal/sandbox"
	"github.com/vorash/vora/internal/secret"
	"github.com/vorash/vora/internal/telemetry"
	"github.com/vorash/vora/internal/tlsutil"
	"github.com/vorash/vora/internal/tunnel"
	"github.com/vorash/vora/internal/ulid"
)

// ErrNoRunner means no healthy runner is registered.
var ErrNoRunner = errors.New("api: no healthy runner registered")

var errTenantAccess = errors.New("api: sandbox not found")

// RunnerClient is the subset of the runner gRPC client the API uses.
type RunnerClient interface {
	CreateSandboxWithCredentials(ctx context.Context, spec *v1.SandboxSpec, creds []*v1.BrokerCredential) (*v1.Sandbox, error)
	StartSandbox(ctx context.Context, id string) (*v1.Sandbox, error)
	GetSandbox(ctx context.Context, id string) (*v1.Sandbox, error)
	ListSandboxes(ctx context.Context, includeDestroyed bool) ([]*v1.Sandbox, error)
	DestroySandbox(ctx context.Context, id string) error
	StatsSandbox(ctx context.Context, id string) (*v1.Stats, error)
	Events(ctx context.Context, id string) ([]*v1.SandboxEvent, error)
	Execs(ctx context.Context, id string) ([]*v1.SandboxExec, error)
	Egress(ctx context.Context, id string) ([]*v1.SandboxEgress, error)
	GetFile(ctx context.Context, id, path string) ([]byte, error)
	PutFile(ctx context.Context, id, path string, content []byte, createParents bool) (int64, error)
	Exec(ctx context.Context, req *v1.ExecSandboxRequest, opts ...grpc.CallOption) (grpc.ServerStreamingClient[v1.ExecChunk], error)
	Close() error
}

// Options configures the control plane.
type Options struct {
	Store             Store
	Logger            *slog.Logger
	DialRunner        func(addr string) (RunnerClient, error)
	HeartbeatInterval time.Duration
	RunnerTTL         time.Duration
	// JoinToken, when set, must be presented by runners when registering.
	JoinToken string
	// RunnerTLS enables TLS (mTLS when a CA is set) when dialing runners.
	RunnerTLS tlsutil.Config
	// Secrets holds brokered credentials. When nil, the secret endpoints
	// report that credential brokering is not configured.
	Secrets secret.Store
	// AllowInsecureBroker permits sending resolved credentials to a runner
	// over a channel without transport encryption. Off by default.
	AllowInsecureBroker bool
	// Tunnel, when set, lets the API reach runners it cannot dial (NAT) over a
	// runner-initiated reverse tunnel.
	Tunnel *tunnel.Server
	// BootstrapKey, when set, is accepted as an admin API key.
	BootstrapKey string
	Now          func() time.Time
}

// Server implements the HTTP/JSON API and the gRPC runner registry.
type Server struct {
	v1.UnimplementedRunnerRegistryServer

	opts   Options
	store  Store
	logger *slog.Logger
	now    func() time.Time
}

// New builds a control-plane server.
func New(opts Options) *Server {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Store == nil {
		opts.Store = NewMemStore()
	}
	if opts.DialRunner == nil {
		if opts.RunnerTLS.Enabled() {
			rt := opts.RunnerTLS
			opts.DialRunner = func(addr string) (RunnerClient, error) {
				cfg, err := rt.Client()
				if err != nil {
					return nil, err
				}
				return client.DialAddressTLS(addr, cfg)
			}
		} else {
			opts.DialRunner = func(addr string) (RunnerClient, error) { return client.DialAddress(addr) }
		}
	}
	if opts.HeartbeatInterval <= 0 {
		opts.HeartbeatInterval = 10 * time.Second
	}
	if opts.RunnerTTL <= 0 {
		opts.RunnerTTL = 3 * opts.HeartbeatInterval
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Server{opts: opts, store: opts.Store, logger: opts.Logger, now: opts.Now}
}

// Close releases the store.
func (s *Server) Close() error { return s.store.Close() }

// ---------------------------------------------------------------------------
// Runner registry (gRPC)
// ---------------------------------------------------------------------------

func (s *Server) Register(ctx context.Context, req *v1.RegisterRunnerRequest) (*v1.RegisterRunnerResponse, error) {
	if err := s.checkJoinToken(ctx); err != nil {
		return nil, err
	}
	if req.GetRunnerId() == "" {
		return nil, status.Error(codes.InvalidArgument, "runner_id is required")
	}
	// An empty address means the runner is reached through the reverse tunnel.
	// Without a tunnel configured such a runner would be unroutable, so refuse.
	if req.GetAddress() == "" && s.opts.Tunnel == nil {
		return nil, status.Error(codes.InvalidArgument, "address is required when no tunnel is configured")
	}
	now := s.now()
	r := Runner{
		ID:            req.GetRunnerId(),
		Address:       req.GetAddress(),
		Tunneled:      req.GetAddress() == "",
		Backends:      req.GetBackends(),
		NetworkModes:  req.GetNetworkModes(),
		CPUMillis:     req.GetCapacity().GetCpuMillis(),
		MemoryBytes:   req.GetCapacity().GetMemoryBytes(),
		MaxSandboxes:  req.GetCapacity().GetMaxSandboxes(),
		Version:       req.GetVersion(),
		RegisteredAt:  now,
		LastHeartbeat: now,
	}
	if err := s.store.UpsertRunner(ctx, r); err != nil {
		return nil, status.Errorf(codes.Internal, "register runner: %v", err)
	}
	s.logger.Info("runner registered", "runner_id", r.ID, "address", r.Address, "backends", r.Backends)
	return &v1.RegisterRunnerResponse{HeartbeatIntervalSeconds: int64(s.opts.HeartbeatInterval / time.Second)}, nil
}

func (s *Server) Heartbeat(ctx context.Context, req *v1.HeartbeatRequest) (*v1.HeartbeatResponse, error) {
	if err := s.checkJoinToken(ctx); err != nil {
		return nil, err
	}
	existing, err := s.store.Runner(ctx, req.GetRunnerId())
	if err != nil {
		return nil, status.Error(codes.NotFound, "runner is not registered")
	}
	existing.LastHeartbeat = s.now()
	existing.Running = req.GetSandboxesRunning()
	if c := req.GetCapacity(); c != nil {
		existing.CPUMillis = c.GetCpuMillis()
		existing.MemoryBytes = c.GetMemoryBytes()
		existing.MaxSandboxes = c.GetMaxSandboxes()
	}
	if err := s.store.UpsertRunner(ctx, existing); err != nil {
		return nil, status.Errorf(codes.Internal, "heartbeat: %v", err)
	}
	return &v1.HeartbeatResponse{}, nil
}

func (s *Server) checkJoinToken(ctx context.Context) error {
	if s.opts.JoinToken == "" {
		return nil
	}
	md, _ := metadata.FromIncomingContext(ctx)
	auth := md.Get("authorization")
	if len(auth) == 0 || strings.TrimSpace(auth[0]) != "Bearer "+s.opts.JoinToken {
		return status.Error(codes.Unauthenticated, "invalid runner join token")
	}
	return nil
}

// ---------------------------------------------------------------------------
// HTTP/JSON API
// ---------------------------------------------------------------------------

// Handler returns the authenticated HTTP API.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/sandboxes", s.auth(ScopeSandbox, s.createSandbox))
	mux.HandleFunc("GET /v1/sandboxes", s.auth(ScopeSandbox, s.listSandboxes))
	mux.HandleFunc("GET /v1/sandboxes/{id}", s.auth(ScopeSandbox, s.getSandbox))
	mux.HandleFunc("DELETE /v1/sandboxes/{id}", s.auth(ScopeSandbox, s.destroySandbox))
	mux.HandleFunc("POST /v1/sandboxes/{id}/exec", s.auth(ScopeSandbox, s.execSandbox))
	mux.HandleFunc("GET /v1/sandboxes/{id}/stats", s.auth(ScopeSandbox, s.statsSandbox))
	mux.HandleFunc("GET /v1/sandboxes/{id}/events", s.auth(ScopeSandbox, s.eventsSandbox))
	mux.HandleFunc("GET /v1/sandboxes/{id}/execs", s.auth(ScopeSandbox, s.execsSandbox))
	mux.HandleFunc("GET /v1/sandboxes/{id}/egress", s.auth(ScopeSandbox, s.egressSandbox))
	mux.HandleFunc("GET /v1/sandboxes/{id}/files", s.auth(ScopeSandbox, s.getFile))
	mux.HandleFunc("PUT /v1/sandboxes/{id}/files", s.auth(ScopeSandbox, s.putFile))

	mux.HandleFunc("GET /v1/runners", s.auth(ScopeAdmin, s.listRunners))
	mux.HandleFunc("POST /v1/keys", s.auth(ScopeAdmin, s.createKey))
	mux.HandleFunc("GET /v1/keys", s.auth(ScopeAdmin, s.listKeys))
	mux.HandleFunc("DELETE /v1/keys/{id}", s.auth(ScopeAdmin, s.revokeKey))
	mux.HandleFunc("POST /v1/secrets", s.auth(ScopeAdmin, s.createSecret))
	mux.HandleFunc("GET /v1/secrets", s.auth(ScopeAdmin, s.listSecrets))
	mux.HandleFunc("DELETE /v1/secrets/{alias}", s.auth(ScopeAdmin, s.deleteSecret))
	// One server span per request; the inner handler echoes the trace id on
	// every response (including errors) so a caller can correlate a failure.
	return otelhttp.NewHandler(traceHeader(mux), "vora-api")
}

// traceHeader sets X-Trace-Id from the request's span, when tracing is on.
func traceHeader(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if id := telemetry.TraceID(r.Context()); id != "" {
			w.Header().Set("X-Trace-Id", id)
		}
		next.ServeHTTP(w, r)
	})
}

type principal struct {
	key APIKey
}

type principalKey struct{}

func (s *Server) auth(scope string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key, ok := s.authenticate(r)
		if !ok {
			writeError(w, http.StatusUnauthorized, "invalid or missing API key")
			return
		}
		if scope != "" && !authorizes(key, scope) {
			writeError(w, http.StatusForbidden, "API key lacks scope "+scope)
			return
		}
		if key.ID != "" {
			_ = s.store.TouchKey(r.Context(), key.ID, s.now())
		}
		next(w, r.WithContext(context.WithValue(r.Context(), principalKey{}, principal{key: key})))
	}
}

// authenticate resolves the bearer token to a key. The bootstrap key maps to a
// synthetic admin key and is never stored.
func (s *Server) authenticate(r *http.Request) (APIKey, bool) {
	const bearer = "Bearer "
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, bearer) {
		return APIKey{}, false
	}
	token := strings.TrimSpace(strings.TrimPrefix(h, bearer))
	if token == "" {
		return APIKey{}, false
	}
	if s.opts.BootstrapKey != "" && subtle.ConstantTimeCompare([]byte(HashKey(token)), []byte(HashKey(s.opts.BootstrapKey))) == 1 {
		return APIKey{Name: "bootstrap", Scopes: []string{ScopeAdmin, ScopeSandbox}}, true
	}
	key, err := s.store.KeyByHash(r.Context(), HashKey(token))
	if err != nil || key.Revoked {
		return APIKey{}, false
	}
	return key, true
}

func principalFrom(ctx context.Context) principal {
	p, _ := ctx.Value(principalKey{}).(principal)
	return p
}

// ---------------------------------------------------------------------------
// Sandbox handlers
// ---------------------------------------------------------------------------

func (s *Server) createSandbox(w http.ResponseWriter, r *http.Request) {
	spec := &v1.SandboxSpec{}
	if err := decodeProto(r, spec); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	key := principalFrom(r.Context()).key
	if !isGlobalAdmin(key) {
		if spec.GetTenantId() != "" && spec.GetTenantId() != key.TenantID {
			writeError(w, http.StatusForbidden, "cannot create a sandbox for another tenant")
			return
		}
		spec.TenantId = key.TenantID
	}
	pl, err := s.placeRunner(r.Context(), spec)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	runner, rc := pl.runner, pl.client
	defer rc.Close()
	creds, err := s.resolveBrokerSecrets(r.Context(), spec)
	if err != nil {
		if errors.Is(err, errSecretsNotConfigured) {
			writeError(w, http.StatusServiceUnavailable, err.Error())
			return
		}
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	defer zeroBrokerSecrets(creds)

	created, err := rc.CreateSandboxWithCredentials(r.Context(), spec, creds)
	if err != nil {
		writeGRPCError(w, err)
		return
	}
	started, err := rc.StartSandbox(r.Context(), created.GetId())
	if err != nil {
		s.cleanupRunnerSandbox(rc, created.GetId(), "start failed")
		writeGRPCError(w, err)
		return
	}
	route := SandboxRoute{
		SandboxID:   started.GetId(),
		RunnerID:    runner.ID,
		TenantID:    spec.GetTenantId(),
		CPUMillis:   spec.GetResources().GetCpuMillis(),
		MemoryBytes: spec.GetResources().GetMemoryBytes(),
	}
	if err := s.store.SetRoute(r.Context(), route); err != nil {
		s.cleanupRunnerSandbox(rc, started.GetId(), "route persistence failed")
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		if deleteErr := s.store.DeleteRoute(cleanupCtx, started.GetId()); deleteErr != nil {
			s.logger.Error("remove partial route failed", "sandbox_id", started.GetId(), "error", deleteErr)
		}
		cancel()
		writeError(w, http.StatusInternalServerError, "failed to persist sandbox route")
		return
	}
	s.logger.Info("sandbox placed", "sandbox_id", started.GetId(), "runner_id", runner.ID, "reason", pl.reason)
	writeProto(w, http.StatusCreated, started)
}

func (s *Server) cleanupRunnerSandbox(rc RunnerClient, sandboxID, cause string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := rc.DestroySandbox(ctx, sandboxID); err != nil {
		s.logger.Error("compensating sandbox cleanup failed", "sandbox_id", sandboxID, "cause", cause, "error", err)
	}
}

func (s *Server) listSandboxes(w http.ResponseWriter, r *http.Request) {
	include := r.URL.Query().Get("include_destroyed") == "true"
	all := &v1.ListSandboxesResponse{}
	for _, runner := range s.runners(r.Context()) {
		rc, err := s.dialRunner(runner)
		if err != nil {
			continue
		}
		items, err := rc.ListSandboxes(r.Context(), include)
		rc.Close()
		if err != nil {
			continue
		}
		key := principalFrom(r.Context()).key
		for _, sb := range items {
			if canAccessTenant(key, sb.GetSpec().GetTenantId()) {
				all.Sandboxes = append(all.Sandboxes, sb)
			}
		}
	}
	writeProto(w, http.StatusOK, all)
}

func (s *Server) getSandbox(w http.ResponseWriter, r *http.Request) {
	rc, err := s.runnerFor(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	defer rc.Close()
	sb, err := rc.GetSandbox(r.Context(), r.PathValue("id"))
	if err != nil {
		writeGRPCError(w, err)
		return
	}
	writeProto(w, http.StatusOK, sb)
}

func (s *Server) destroySandbox(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	rc, err := s.runnerFor(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	defer rc.Close()
	if err := rc.DestroySandbox(r.Context(), id); err != nil {
		writeGRPCError(w, err)
		return
	}
	_ = s.store.DeleteRoute(r.Context(), id)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) statsSandbox(w http.ResponseWriter, r *http.Request) {
	rc, err := s.runnerFor(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	defer rc.Close()
	st, err := rc.StatsSandbox(r.Context(), r.PathValue("id"))
	if err != nil {
		writeGRPCError(w, err)
		return
	}
	writeProto(w, http.StatusOK, st)
}

func (s *Server) eventsSandbox(w http.ResponseWriter, r *http.Request) {
	rc, err := s.runnerFor(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	defer rc.Close()
	events, err := rc.Events(r.Context(), r.PathValue("id"))
	if err != nil {
		writeGRPCError(w, err)
		return
	}
	writeProto(w, http.StatusOK, &v1.GetSandboxEventsResponse{Events: events})
}

func (s *Server) execsSandbox(w http.ResponseWriter, r *http.Request) {
	rc, err := s.runnerFor(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	defer rc.Close()
	execs, err := rc.Execs(r.Context(), r.PathValue("id"))
	if err != nil {
		writeGRPCError(w, err)
		return
	}
	writeProto(w, http.StatusOK, &v1.GetSandboxExecsResponse{Execs: execs})
}

func (s *Server) egressSandbox(w http.ResponseWriter, r *http.Request) {
	rc, err := s.runnerFor(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	defer rc.Close()
	records, err := rc.Egress(r.Context(), r.PathValue("id"))
	if err != nil {
		writeGRPCError(w, err)
		return
	}
	writeProto(w, http.StatusOK, &v1.GetSandboxEgressResponse{Records: records})
}

// getFile streams one file from a sandbox's writable roots. The path travels
// as a query parameter and is validated by the runner, which owns the mapping
// from sandbox paths to host directories.
func (s *Server) getFile(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	path := r.URL.Query().Get("path")
	if path == "" {
		writeError(w, http.StatusBadRequest, "path query parameter is required")
		return
	}
	rc, err := s.runnerFor(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	defer rc.Close()

	content, err := rc.GetFile(r.Context(), id, path)
	if err != nil {
		writeGRPCError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(content)
}

// putFile writes one file into a sandbox's writable roots. The body is the raw
// file content, capped before it is forwarded to the runner.
func (s *Server) putFile(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	path := r.URL.Query().Get("path")
	if path == "" {
		writeError(w, http.StatusBadRequest, "path query parameter is required")
		return
	}
	content, err := io.ReadAll(io.LimitReader(r.Body, sandbox.MaxTransferBytes+1))
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed to read request body")
		return
	}
	if len(content) > sandbox.MaxTransferBytes {
		writeError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("file exceeds %d bytes", sandbox.MaxTransferBytes))
		return
	}
	rc, err := s.runnerFor(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	defer rc.Close()

	createParents := r.URL.Query().Get("create_parents") == "true"
	n, err := rc.PutFile(r.Context(), id, path, content, createParents)
	if err != nil {
		writeGRPCError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"path": path, "bytes_written": n})
}

// execSandbox streams one execution as NDJSON of v1.ExecChunk.
func (s *Server) execSandbox(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	req := &v1.ExecRequest{}
	if err := decodeProto(r, req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	rc, err := s.runnerFor(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	defer rc.Close()

	stream, err := rc.Exec(r.Context(), &v1.ExecSandboxRequest{SandboxId: id, Exec: req})
	if err != nil {
		writeGRPCError(w, err)
		return
	}

	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	for {
		chunk, err := stream.Recv()
		if errors.Is(err, context.Canceled) {
			return
		}
		if err != nil {
			return
		}
		data, err := protoJSON.Marshal(chunk)
		if err != nil {
			return
		}
		if _, err := w.Write(append(data, '\n')); err != nil {
			return
		}
		if flusher != nil {
			flusher.Flush()
		}
		if chunk.GetExit() != nil {
			return
		}
	}
}

// ---------------------------------------------------------------------------
// Runner + key handlers
// ---------------------------------------------------------------------------

func (s *Server) listRunners(w http.ResponseWriter, r *http.Request) {
	if !isGlobalAdmin(principalFrom(r.Context()).key) {
		writeError(w, http.StatusForbidden, "runner inventory requires a global administrator")
		return
	}
	runners, err := s.store.ListRunners(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	now := s.now()
	committed := s.committedByRunner(r.Context())
	type runnerView struct {
		Runner
		Healthy              bool  `json:"healthy"`
		CommittedCPUMillis   int64 `json:"committed_cpu_millis"`
		CommittedMemoryBytes int64 `json:"committed_memory_bytes"`
		CommittedSandboxes   int32 `json:"committed_sandboxes"`
	}
	out := make([]runnerView, 0, len(runners))
	for _, rn := range runners {
		c := committed[rn.ID]
		out = append(out, runnerView{
			Runner:               rn,
			Healthy:              rn.Healthy(now, s.opts.RunnerTTL),
			CommittedCPUMillis:   c.cpu,
			CommittedMemoryBytes: c.mem,
			CommittedSandboxes:   c.count,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"runners": out})
}

func (s *Server) createKey(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name     string   `json:"name"`
		TenantID string   `json:"tenant_id"`
		Scopes   []string `json:"scopes"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	body.Name = strings.TrimSpace(body.Name)
	if body.Name == "" || len(body.Name) > 128 {
		writeError(w, http.StatusBadRequest, "name must be between 1 and 128 characters")
		return
	}
	if len(body.Scopes) == 0 {
		body.Scopes = []string{ScopeSandbox}
	}
	if err := validateScopes(body.Scopes); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	caller := principalFrom(r.Context()).key
	if !isGlobalAdmin(caller) {
		if body.TenantID != "" && body.TenantID != caller.TenantID {
			writeError(w, http.StatusForbidden, "cannot create a key for another tenant")
			return
		}
		body.TenantID = caller.TenantID
	}
	if body.TenantID == "" && !hasScope(body.Scopes, ScopeAdmin) {
		writeError(w, http.StatusBadRequest, "non-admin keys require a tenant_id")
		return
	}
	plaintext, hash, err := NewAPIKey()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "key generation failed")
		return
	}
	key := APIKey{
		ID:        ulid.NewAPIKeyID(),
		Name:      body.Name,
		TenantID:  body.TenantID,
		KeyHash:   hash,
		Scopes:    body.Scopes,
		CreatedAt: s.now(),
	}
	if err := s.store.CreateKey(r.Context(), key); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	key.KeyHash = ""
	writeJSON(w, http.StatusCreated, map[string]any{"key": plaintext, "api_key": key})
}

func (s *Server) listKeys(w http.ResponseWriter, r *http.Request) {
	caller := principalFrom(r.Context()).key
	tenant := r.URL.Query().Get("tenant_id")
	if !isGlobalAdmin(caller) {
		if tenant != "" && tenant != caller.TenantID {
			writeError(w, http.StatusForbidden, "cannot list keys for another tenant")
			return
		}
		tenant = caller.TenantID
	}
	keys, err := s.store.ListKeys(r.Context(), tenant)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"keys": keys})
}

func (s *Server) revokeKey(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	target, err := s.store.Key(r.Context(), id)
	if err != nil || !canAccessTenant(principalFrom(r.Context()).key, target.TenantID) {
		writeError(w, http.StatusNotFound, "key not found")
		return
	}
	if err := s.store.RevokeKey(r.Context(), id); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func validateScopes(scopes []string) error {
	seen := make(map[string]struct{}, len(scopes))
	for _, scope := range scopes {
		if scope != ScopeAdmin && scope != ScopeSandbox {
			return fmt.Errorf("unknown scope %q", scope)
		}
		if _, ok := seen[scope]; ok {
			return fmt.Errorf("duplicate scope %q", scope)
		}
		seen[scope] = struct{}{}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Runner selection
// ---------------------------------------------------------------------------

func (s *Server) runners(ctx context.Context) []Runner {
	runners, err := s.store.ListRunners(ctx)
	if err != nil {
		return nil
	}
	now := s.now()
	var healthy []Runner
	for _, r := range runners {
		if r.Healthy(now, s.opts.RunnerTTL) {
			healthy = append(healthy, r)
		}
	}
	return healthy
}

// sandboxResources is a runner's committed load, derived from the routes the
// API created rather than from runner self-reports.
type sandboxResources struct {
	cpu   int64
	mem   int64
	count int32
}

// placement is the chosen runner plus why it was chosen.
type placement struct {
	runner Runner
	client RunnerClient
	reason string
}

// placeRunner filters healthy runners by backend/network capability and by
// committed capacity, then picks the least-loaded candidate. The reason is
// logged so placement is inspectable.
func (s *Server) placeRunner(ctx context.Context, spec *v1.SandboxSpec) (*placement, error) {
	runners := s.runners(ctx)
	if len(runners) == 0 {
		return nil, ErrNoRunner
	}
	committed := s.committedByRunner(ctx)
	backend := spec.GetBackend()
	network := networkModeName(spec.GetNetwork().GetMode())
	res := spec.GetResources()

	candidates := make([]Runner, 0, len(runners))
	rejections := make(map[string]string, len(runners))
	for _, r := range runners {
		if reason := rejectReason(r, backend, network, res, committed[r.ID]); reason != "" {
			rejections[r.ID] = reason
			continue
		}
		candidates = append(candidates, r)
	}
	if len(candidates) == 0 {
		s.logger.Warn("no runner can take the sandbox", "backend", backend, "network", network, "rejections", rejections)
		return nil, fmt.Errorf("%w: no runner matches the request (backend %q, network %q)", ErrNoRunner, backend, network)
	}

	sort.Slice(candidates, func(i, j int) bool {
		a, b := committed[candidates[i].ID], committed[candidates[j].ID]
		if a.cpu != b.cpu {
			return a.cpu < b.cpu
		}
		if a.mem != b.mem {
			return a.mem < b.mem
		}
		if a.count != b.count {
			return a.count < b.count
		}
		return candidates[i].ID < candidates[j].ID
	})
	chosen := candidates[0]
	reason := "least-loaded"
	if len(candidates) == 1 {
		reason = "only-runner"
	}
	rc, err := s.dialRunner(chosen)
	if err != nil {
		return nil, fmt.Errorf("api: runner %s unreachable: %w", chosen.ID, err)
	}
	return &placement{runner: chosen, client: rc, reason: reason}, nil
}

// rejectReason returns a non-empty reason when the runner cannot host the
// request, empty when it can.
func rejectReason(r Runner, backend, network string, res *v1.ResourceLimits, used sandboxResources) string {
	if !r.SupportsBackend(backend) {
		return "backend"
	}
	if !r.SupportsNetwork(network) {
		return "network"
	}
	// The API knows the sandboxes it created; take the larger of that and the
	// runner's own count so a directly-created sandbox still counts.
	running := used.count
	if r.Running > running {
		running = r.Running
	}
	if r.MaxSandboxes > 0 && running >= r.MaxSandboxes {
		return "max_sandboxes"
	}
	if r.MemoryBytes > 0 && used.mem+res.GetMemoryBytes() > r.MemoryBytes {
		return "memory"
	}
	if r.CPUMillis > 0 && used.cpu+res.GetCpuMillis() > r.CPUMillis {
		return "cpu"
	}
	return ""
}

// committedByRunner sums the resources of the routes the API created, per
// runner.
func (s *Server) committedByRunner(ctx context.Context) map[string]sandboxResources {
	routes, err := s.store.ListRoutes(ctx)
	if err != nil {
		return nil
	}
	out := make(map[string]sandboxResources, len(routes))
	for _, r := range routes {
		c := out[r.RunnerID]
		c.cpu += r.CPUMillis
		c.mem += r.MemoryBytes
		c.count++
		out[r.RunnerID] = c
	}
	return out
}

// networkModeName maps the API's network mode to the runtime name the runner
// advertises in NetworkModes.
func networkModeName(m v1.NetworkMode) string {
	switch m {
	case v1.NetworkMode_NETWORK_NONE:
		return "none"
	case v1.NetworkMode_NETWORK_RESTRICTED:
		return "restricted"
	case v1.NetworkMode_NETWORK_FULL:
		return "full"
	case v1.NetworkMode_NETWORK_PRIVATE:
		return "private"
	default:
		return ""
	}
}

func isGlobalAdmin(key APIKey) bool {
	return key.TenantID == "" && key.HasScope(ScopeAdmin)
}

// authorizes reports whether a key may use a route that requires scope. Admin
// implies sandbox: the operator key that manages keys, secrets and runner
// inventory is also expected to run sandboxes, and admin is strictly the more
// privileged scope, so the implication grants nothing a sandbox key lacked.
func authorizes(key APIKey, scope string) bool {
	if key.HasScope(scope) {
		return true
	}
	return scope == ScopeSandbox && key.HasScope(ScopeAdmin)
}

func canAccessTenant(key APIKey, tenantID string) bool {
	if isGlobalAdmin(key) {
		return true
	}
	return key.TenantID != "" && key.TenantID == tenantID
}

func hasScope(scopes []string, scope string) bool {
	for _, s := range scopes {
		if s == scope {
			return true
		}
	}
	return false
}

func (s *Server) authorizedRoute(ctx context.Context, sandboxID string) (SandboxRoute, error) {
	route, err := s.store.Route(ctx, sandboxID)
	if err != nil || !canAccessTenant(principalFrom(ctx).key, route.TenantID) {
		return SandboxRoute{}, errTenantAccess
	}
	return route, nil
}

// dialRunner returns a client for a runner, over the reverse tunnel when the
// runner is tunnelled and a tunnel is configured.
func (s *Server) dialRunner(r Runner) (RunnerClient, error) {
	if r.Tunneled {
		if s.opts.Tunnel == nil {
			return nil, fmt.Errorf("api: runner %s is tunnelled but no tunnel is configured", r.ID)
		}
		return client.DialTunnel(r.ID, s.opts.Tunnel)
	}
	return s.opts.DialRunner(r.Address)
}

// runnerFor returns a client for the runner that owns an authorized sandbox.
func (s *Server) runnerFor(ctx context.Context, sandboxID string) (RunnerClient, error) {
	route, err := s.authorizedRoute(ctx, sandboxID)
	if err != nil {
		return nil, err
	}
	r, err := s.store.Runner(ctx, route.RunnerID)
	if err != nil {
		return nil, fmt.Errorf("api: runner %s unknown", route.RunnerID)
	}
	rc, err := s.dialRunner(r)
	if err != nil {
		return nil, fmt.Errorf("api: runner %s unreachable: %w", route.RunnerID, err)
	}
	return rc, nil
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

var protoJSON = protojson.MarshalOptions{}

func writeProto(w http.ResponseWriter, code int, msg proto.Message) {
	data, err := protoJSON.Marshal(msg)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write(data)
}

func decodeProto(r *http.Request, msg proto.Message) error {
	data, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return err
	}
	if len(data) == 0 {
		return errors.New("empty request body")
	}
	return protojson.UnmarshalOptions{DiscardUnknown: true}.Unmarshal(data, msg)
}

func decodeJSON(r *http.Request, v any) error {
	return json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(v)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func writeGRPCError(w http.ResponseWriter, err error) {
	st, ok := status.FromError(err)
	if !ok {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	code := http.StatusInternalServerError
	switch st.Code() {
	case codes.InvalidArgument:
		code = http.StatusBadRequest
	case codes.NotFound:
		code = http.StatusNotFound
	case codes.AlreadyExists:
		code = http.StatusConflict
	case codes.FailedPrecondition, codes.Unavailable:
		code = http.StatusServiceUnavailable
	case codes.Unauthenticated:
		code = http.StatusUnauthorized
	case codes.PermissionDenied:
		code = http.StatusForbidden
	}
	writeError(w, code, st.Message())
}
