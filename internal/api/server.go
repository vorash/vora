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
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	v1 "github.com/vorash/vora/api/proto/v1"
	"github.com/vorash/vora/internal/client"
	"github.com/vorash/vora/internal/tlsutil"
	"github.com/vorash/vora/internal/ulid"
)

// ErrNoRunner means no healthy runner is registered.
var ErrNoRunner = errors.New("api: no healthy runner registered")

var errTenantAccess = errors.New("api: sandbox not found")

// RunnerClient is the subset of the runner gRPC client the API uses.
type RunnerClient interface {
	CreateSandbox(ctx context.Context, spec *v1.SandboxSpec) (*v1.Sandbox, error)
	StartSandbox(ctx context.Context, id string) (*v1.Sandbox, error)
	GetSandbox(ctx context.Context, id string) (*v1.Sandbox, error)
	ListSandboxes(ctx context.Context, includeDestroyed bool) ([]*v1.Sandbox, error)
	DestroySandbox(ctx context.Context, id string) error
	StatsSandbox(ctx context.Context, id string) (*v1.Stats, error)
	Events(ctx context.Context, id string) ([]*v1.SandboxEvent, error)
	Execs(ctx context.Context, id string) ([]*v1.SandboxExec, error)
	Egress(ctx context.Context, id string) ([]*v1.SandboxEgress, error)
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
	if req.GetRunnerId() == "" || req.GetAddress() == "" {
		return nil, status.Error(codes.InvalidArgument, "runner_id and address are required")
	}
	now := s.now()
	r := Runner{
		ID:            req.GetRunnerId(),
		Address:       req.GetAddress(),
		Backends:      req.GetBackends(),
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

	mux.HandleFunc("GET /v1/runners", s.auth(ScopeAdmin, s.listRunners))
	mux.HandleFunc("POST /v1/keys", s.auth(ScopeAdmin, s.createKey))
	mux.HandleFunc("GET /v1/keys", s.auth(ScopeAdmin, s.listKeys))
	mux.HandleFunc("DELETE /v1/keys/{id}", s.auth(ScopeAdmin, s.revokeKey))
	return mux
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
		if scope != "" && !key.HasScope(scope) {
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
	runner, rc, err := s.pickRunner(r.Context())
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	defer rc.Close()

	created, err := rc.CreateSandbox(r.Context(), spec)
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
	route := SandboxRoute{SandboxID: started.GetId(), RunnerID: runner.ID, TenantID: spec.GetTenantId()}
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
		rc, err := s.opts.DialRunner(runner.Address)
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
	type runnerView struct {
		Runner
		Healthy bool `json:"healthy"`
	}
	out := make([]runnerView, 0, len(runners))
	for _, rn := range runners {
		out = append(out, runnerView{Runner: rn, Healthy: rn.Healthy(now, s.opts.RunnerTTL)})
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

// pickRunner returns the first healthy runner (scheduling is deferred).
func (s *Server) pickRunner(ctx context.Context) (Runner, RunnerClient, error) {
	for _, r := range s.runners(ctx) {
		rc, err := s.opts.DialRunner(r.Address)
		if err != nil {
			s.logger.Warn("runner unreachable", "runner_id", r.ID, "address", r.Address, "error", err)
			continue
		}
		return r, rc, nil
	}
	return Runner{}, nil, ErrNoRunner
}

func isGlobalAdmin(key APIKey) bool {
	return key.TenantID == "" && key.HasScope(ScopeAdmin)
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
	rc, err := s.opts.DialRunner(r.Address)
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
