package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	v1 "github.com/vorash/vora/api/proto/v1"
	"github.com/vorash/vora/internal/identity"
	"github.com/vorash/vora/internal/pool"
	"github.com/vorash/vora/internal/ulid"
)

const (
	poolCallTimeout = 10 * time.Second
	// poolDeleteDeadline bounds one DeletePool, and poolDeleteWait is how long the
	// runner may wait inside it, so a drain still in progress gets its answer back.
	poolDeleteDeadline = 30 * time.Second
	poolDeleteWait     = 25 * time.Second
	// runTailGrace is added to a run's timeout to bound its stream once it has
	// started. It covers the runner's hard deadline past the timeout (5 s), the
	// reset before run_done (10 s), and its last sends.
	runTailGrace = time.Minute
	// defaultRunWriteTimeout is how long one frame may wait for a client that has
	// stopped reading. With 32 KiB of output in a frame, that fails only a client
	// reading slower than about 1.5 KiB/s.
	defaultRunWriteTimeout = 30 * time.Second
)

var (
	errPoolNotFound = errors.New("pool not found")
	errRunnerGone   = errors.New("api: the pool's runner is no longer registered")
	errRunnerDown   = errors.New("api: the pool's runner is unavailable")
)

// poolRouteFor resolves the request's pool for its key. It answers 404, with one
// body, for a pool that does not exist, that another tenant owns, and that
// another agent of this tenant owns. A global admin names the tenant with
// ?tenant_id=, as it does in a create's body. Any other key that names one gets
// no further than canAccessPool.
func (s *Server) poolRouteFor(w http.ResponseWriter, r *http.Request) (PoolRoute, bool) {
	key := principalFrom(r.Context()).key
	tenant := r.URL.Query().Get("tenant_id")
	if tenant == "" {
		if isGlobalAdmin(key) {
			writeError(w, http.StatusBadRequest, "a global administrator must pass tenant_id")
			return PoolRoute{}, false
		}
		tenant = key.TenantID
	}
	route, err := s.store.PoolRoute(r.Context(), tenant, r.PathValue("name"))
	switch {
	case errors.Is(err, ErrNotFound), err == nil && !s.canAccessPool(key, route):
		writeError(w, http.StatusNotFound, errPoolNotFound.Error())
		return PoolRoute{}, false
	case err != nil:
		writeError(w, http.StatusInternalServerError, err.Error())
		return PoolRoute{}, false
	}
	return route, true
}

func (s *Server) canAccessPool(key APIKey, route PoolRoute) bool {
	return canAccessTenant(key, route.TenantID) && canAccessAgent(key, route.AgentID)
}

// poolRunner returns the runner's shared client, which the caller must not
// close. A runner past its heartbeat TTL answers as unavailable with no call: a
// silently dead host would otherwise hang a call on the reused connection until
// keepalive notices.
func (s *Server) poolRunner(ctx context.Context, runnerID string) (RunnerClient, error) {
	r, err := s.store.Runner(ctx, runnerID)
	if errors.Is(err, ErrNotFound) {
		return nil, errRunnerGone
	}
	if err != nil {
		s.logger.Error("read pool runner failed", "runner_id", runnerID, "error", err)
		return nil, errRunnerDown
	}
	if !r.Healthy(s.now(), s.opts.RunnerTTL) {
		return nil, errRunnerDown
	}
	rc, err := s.conns.get(r, s.dialRunner)
	if err != nil {
		s.logger.Error("dial pool runner failed", "runner_id", runnerID, "error", err)
		return nil, errRunnerDown
	}
	return rc, nil
}

func writeRunnerDown(w http.ResponseWriter) {
	w.Header().Set("Retry-After", "1")
	writeError(w, http.StatusServiceUnavailable, "the pool's runner is unavailable; retry shortly")
}

// writePoolError answers a failed pool call. NotFound's text is fixed, so no pool
// answer says more than "no such pool".
func writePoolError(w http.ResponseWriter, err error) {
	switch status.Code(err) {
	case codes.NotFound:
		writeError(w, http.StatusNotFound, errPoolNotFound.Error())
	case codes.Unavailable:
		slog.Warn("pool call unavailable", "error", err, "trace_id", w.Header().Get("X-Trace-Id"))
		w.Header().Set("Retry-After", "1")
		writeError(w, http.StatusServiceUnavailable, "no sandbox could start, or the runner is unavailable or still starting; retry shortly")
	case codes.DeadlineExceeded:
		w.Header().Set("Retry-After", "1")
		writeError(w, http.StatusGatewayTimeout, "the runner did not answer in time; retry")
	default:
		writeGRPCError(w, err)
	}
}

// dropPoolRoute removes a route the runner no longer backs, which releases its
// reservation. It runs detached, as the client that got the answer may be gone.
func (s *Server) dropPoolRoute(ctx context.Context, route PoolRoute, cause string) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), poolCallTimeout)
	defer cancel()
	if err := s.store.DeletePoolRoute(ctx, route.TenantID, route.Name, route.PoolID); err != nil {
		s.logger.Error("remove pool route failed", "pool_id", route.PoolID, "cause", cause, "error", err)
		return err
	}
	s.logger.Info("pool route removed", "pool_id", route.PoolID, "name", route.Name, "tenant_id", route.TenantID, "cause", cause)
	return nil
}

// createRefused reports whether the runner answered a CreatePool with a definite
// no. Any other failure may have created the pool, for instance when the answer
// was lost, so the route has to stay: it is the only way to reach that pool.
func createRefused(err error) bool {
	switch status.Code(err) {
	case codes.InvalidArgument, codes.AlreadyExists, codes.ResourceExhausted, codes.FailedPrecondition,
		codes.PermissionDenied, codes.Unauthenticated, codes.Unimplemented:
		return true
	default:
		return false
	}
}

// putPool declares a pool. The route goes in first, carrying an id the API
// minted, and the runner is asked second, so no failure can leave a pool on a
// runner that no route points at, and racing declares meet at the insert. A
// declare that finds a route goes to that route's runner, which is idempotent
// by (tenant, name) and answers with the pool it has, or 409 on a different spec.
func (s *Server) putPool(w http.ResponseWriter, r *http.Request) {
	spec := &v1.PoolSpec{}
	if err := decodeProto(r, spec); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	name := r.PathValue("name")
	if spec.GetName() != "" && spec.GetName() != name {
		writeError(w, http.StatusBadRequest, "name in the body must match the path")
		return
	}
	spec.Name = name
	key := principalFrom(r.Context()).key
	if spec.GetReuse() == v1.PoolReuse_POOL_REUSE_UNSAFE_NO_RESET && !key.HasScope(ScopeAdmin) {
		writeError(w, http.StatusForbidden, "unsafe reuse requires an admin key")
		return
	}
	if code, msg := bindPoolIdentity(key, spec, r.URL.Query().Get("tenant_id")); code != 0 {
		writeError(w, code, msg)
		return
	}
	maxSize := pool.MaxSizeOf(optionalInt(spec.Size), optionalInt(spec.MaxSize))
	if maxSize > math.MaxInt32 {
		writeError(w, http.StatusBadRequest, "max_size is too large")
		return
	}
	res := spec.GetSandbox().GetResources()

	// Detached: a client that leaves mid-declare must not abandon a half-done create.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), poolCallTimeout)
	defer cancel()

	route, err := s.store.PoolRoute(ctx, spec.GetTenantId(), name)
	inserted := false
	if errors.Is(err, ErrNotFound) {
		runner, reason, placeErr := s.chooseRunner(ctx, spec.GetSandbox(), needFor(int32(maxSize), res))
		if placeErr != nil {
			writeError(w, http.StatusServiceUnavailable, placeErr.Error())
			return
		}
		route = PoolRoute{
			TenantID:    spec.GetTenantId(),
			Name:        name,
			PoolID:      ulid.NewPool(),
			RunnerID:    runner.ID,
			AgentID:     spec.GetAgentId(),
			MaxSize:     int32(maxSize),
			CPUMillis:   res.GetCpuMillis(),
			MemoryBytes: res.GetMemoryBytes(),
		}
		err = s.store.InsertPoolRoute(ctx, route)
		switch {
		case err == nil:
			inserted = true
			s.logger.Info("pool placed", "pool_id", route.PoolID, "runner_id", runner.ID, "reason", reason)
		case errors.Is(err, ErrPoolRouteExists):
			route, err = s.store.PoolRoute(ctx, spec.GetTenantId(), name)
		}
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	rc, err := s.poolRunner(ctx, route.RunnerID)
	if err != nil {
		if inserted {
			_ = s.dropPoolRoute(ctx, route, "runner unreachable before create")
		}
		writeRunnerDown(w)
		return
	}
	// A route that was already here may name a pool its runner lost, so the runner
	// gets a fresh id to use if it has to create one. The old id stays retired, and
	// a delete of it that is still in flight cannot remove the new route.
	mint := route.PoolID
	if !inserted {
		mint = ulid.NewPool()
	}
	created, err := rc.CreatePool(ctx, &v1.CreatePoolRequest{Spec: spec, Actor: actorFromKey(key), PoolId: mint})
	if err != nil {
		if inserted && createRefused(err) {
			_ = s.dropPoolRoute(ctx, route, "create refused")
		}
		writePoolError(w, err)
		return
	}
	// Always written: besides the runner's filled-in defaults and id, this restores
	// the route if a read saw NotFound and dropped it while this create was in flight.
	route.PoolID = created.GetId()
	route.MaxSize = int32(created.GetSpec().GetMaxSize())
	route.CPUMillis = created.GetSpec().GetSandbox().GetResources().GetCpuMillis()
	route.MemoryBytes = created.GetSpec().GetSandbox().GetResources().GetMemoryBytes()
	if err := s.store.SetPoolRoute(ctx, route); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	hidePoolRunner(key, created)
	code := http.StatusOK
	if inserted {
		code = http.StatusCreated
	}
	writeProto(w, code, created)
}

// bindPoolIdentity forces tenant and agent from the key, as createSandbox does. A
// nonzero code refuses the request. A pool always has a tenant, so a global admin
// must name one.
func bindPoolIdentity(key APIKey, spec *v1.PoolSpec, queryTenant string) (int, string) {
	if isGlobalAdmin(key) {
		if spec.GetTenantId() == "" {
			spec.TenantId = queryTenant
		}
		if spec.GetTenantId() == "" {
			return http.StatusBadRequest, "a global administrator must pass tenant_id"
		}
	} else {
		if spec.GetTenantId() != "" && spec.GetTenantId() != key.TenantID {
			return http.StatusForbidden, "cannot declare a pool for another tenant"
		}
		spec.TenantId = key.TenantID
	}
	if key.AgentID != "" {
		spec.AgentId = key.AgentID
	}
	if err := identity.ValidateName(spec.GetTenantId()); err != nil {
		return http.StatusBadRequest, err.Error()
	}
	if spec.GetAgentId() != "" {
		if err := identity.ValidateName(spec.GetAgentId()); err != nil {
			return http.StatusBadRequest, err.Error()
		}
	}
	return 0, ""
}

func optionalInt(v *uint32) *int {
	if v == nil {
		return nil
	}
	n := int(*v)
	return &n
}

func hidePoolRunner(key APIKey, p *v1.Pool) {
	if key.TenantID != "" {
		p.RunnerId = ""
	}
}

func (s *Server) getPool(w http.ResponseWriter, r *http.Request) {
	route, ok := s.poolRouteFor(w, r)
	if !ok {
		return
	}
	rc, err := s.poolRunner(r.Context(), route.RunnerID)
	if err != nil {
		writeRunnerDown(w)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), poolCallTimeout)
	defer cancel()
	p, err := rc.GetPool(ctx, route.PoolID, route.TenantID)
	if err != nil {
		if status.Code(err) == codes.NotFound {
			_ = s.dropPoolRoute(ctx, route, "runner has no such pool")
		}
		writePoolError(w, err)
		return
	}
	hidePoolRunner(principalFrom(r.Context()).key, p)
	writeProto(w, http.StatusOK, p)
}

// listPools answers from the routes, so a pool whose runner is down still shows,
// with no state, and goes on showing that it holds a reservation.
func (s *Server) listPools(w http.ResponseWriter, r *http.Request) {
	key := principalFrom(r.Context()).key
	tenant := key.TenantID
	if isGlobalAdmin(key) {
		tenant = r.URL.Query().Get("tenant_id")
	}
	routes, err := s.store.ListPoolRoutes(r.Context(), tenant)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := &v1.ListPoolsResponse{}
	for _, route := range routes {
		if !s.canAccessPool(key, route) {
			continue
		}
		p, gone := s.poolForListing(r.Context(), route)
		if gone {
			_ = s.dropPoolRoute(r.Context(), route, "runner has no such pool")
			continue
		}
		hidePoolRunner(key, p)
		out.Pools = append(out.Pools, p)
	}
	writeProto(w, http.StatusOK, out)
}

func (s *Server) poolForListing(ctx context.Context, route PoolRoute) (p *v1.Pool, gone bool) {
	unanswered := &v1.Pool{
		Id:       route.PoolID,
		RunnerId: route.RunnerID,
		Spec:     &v1.PoolSpec{Name: route.Name, TenantId: route.TenantID, AgentId: route.AgentID},
	}
	rc, err := s.poolRunner(ctx, route.RunnerID)
	if err != nil {
		return unanswered, false
	}
	ctx, cancel := context.WithTimeout(ctx, poolCallTimeout)
	defer cancel()
	p, err = rc.GetPool(ctx, route.PoolID, route.TenantID)
	switch {
	case status.Code(err) == codes.NotFound:
		return nil, true
	case err != nil:
		return unanswered, false
	}
	return p, false
}

// deletePool drains a pool. The route, and so the reservation, goes only once
// the runner says the pool is gone: 202 and 503 keep it, and repeating converges.
// A pool the runner no longer knows, or whose runner has no record left, has
// nothing to wait for. The whole call is detached, as a client that leaves must
// not strand a drained pool's route.
func (s *Server) deletePool(w http.ResponseWriter, r *http.Request) {
	route, ok := s.poolRouteFor(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), poolDeleteDeadline)
	defer cancel()
	// The server's own WriteTimeout is no longer than the deadline above, so a drain
	// that takes all of it would otherwise lose its answer.
	if err := http.NewResponseController(w).SetWriteDeadline(time.Now().Add(poolDeleteDeadline + 5*time.Second)); err != nil && !errors.Is(err, http.ErrNotSupported) {
		s.logger.Warn("pool delete cannot extend its write deadline", "error", err)
	}

	drained := false
	rc, err := s.poolRunner(ctx, route.RunnerID)
	switch {
	case errors.Is(err, errRunnerGone):
		drained = true
	case err != nil:
		writeRunnerDown(w)
		return
	default:
		drained, err = rc.DeletePool(ctx, &v1.DeletePoolRequest{
			PoolId:   route.PoolID,
			TenantId: route.TenantID,
			Actor:    actorFromKey(principalFrom(r.Context()).key),
			WaitMs:   poolDeleteWait.Milliseconds(),
		})
		if status.Code(err) == codes.NotFound {
			drained, err = true, nil
		}
		if err != nil {
			writePoolError(w, err)
			return
		}
	}
	if !drained {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	if err := s.dropPoolRoute(ctx, route, "deleted"); err != nil {
		writeError(w, http.StatusInternalServerError, "pool deleted but route removal failed; repeat the delete")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// runPool runs one command and streams it as NDJSON of v1.RunChunk: run, output,
// exit, run_done. Every frame is encoded here, with a program's output as base64
// inside its frame, so nothing a program prints can look like a frame.
//
// The first Recv comes before the headers. The runner sends the run frame only
// once the command started, so a full pool, a draining one or a dead runner
// arrives there and can still be a real status code. After that, a failure is an
// error line.
//
// One frame is held at a time. A client that stops reading stops this loop's
// Recv, which through gRPC's flow control stops the runner's sends, and each
// write has a deadline, so returning cancels the run and frees its member.
func (s *Server) runPool(w http.ResponseWriter, r *http.Request) {
	exec := &v1.ExecRequest{}
	if err := decodeProto(r, exec); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	route, ok := s.poolRouteFor(w, r)
	if !ok {
		return
	}
	rc, err := s.poolRunner(r.Context(), route.RunnerID)
	if err != nil {
		writeRunnerDown(w)
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	out := ndjsonWriter{w: w, rc: http.NewResponseController(w), timeout: s.runWriteTimeout}

	stream, err := rc.RunInPool(ctx, &v1.RunInPoolRequest{
		PoolId:   route.PoolID,
		TenantId: route.TenantID,
		Exec:     exec,
		Actor:    actorFromKey(principalFrom(r.Context()).key),
	})
	var first *v1.RunChunk
	if err == nil {
		first, err = stream.Recv()
	}
	if err != nil {
		if r.Context().Err() != nil {
			return
		}
		// The wait for the first frame may have outlasted the server's WriteTimeout.
		out.extendDeadline()
		if status.Code(err) == codes.NotFound {
			s.dropRouteIfGone(ctx, rc, route)
		}
		writePoolError(w, err)
		return
	}

	// The runner's timeout_seconds check has passed, so this cannot overflow. It
	// ends a stream whose runner went quiet without closing it.
	bound := time.AfterFunc(time.Duration(exec.GetTimeoutSeconds())*time.Second+runTailGrace, cancel)
	defer bound.Stop()

	out.extendDeadline()
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusOK)
	for chunk := first; ; {
		data, err := protoJSON.Marshal(chunk)
		if err != nil {
			s.logger.Error("encode run frame failed", "pool_id", route.PoolID, "error", err)
			return
		}
		if err := out.line(data); err != nil {
			return
		}
		chunk, err = stream.Recv()
		if errors.Is(err, io.EOF) {
			return
		}
		if err != nil {
			if r.Context().Err() == nil {
				s.logger.Warn("pool run ended early", "pool_id", route.PoolID, "code", status.Code(err).String(), "error", err)
				_ = out.line(runErrorLine(err))
			}
			return
		}
	}
}

// dropRouteIfGone asks before dropping: a NotFound out of a run could come from
// inside it, and only GetPool's says the pool itself is gone.
func (s *Server) dropRouteIfGone(ctx context.Context, rc RunnerClient, route PoolRoute) {
	ctx, cancel := context.WithTimeout(ctx, poolCallTimeout)
	defer cancel()
	if _, err := rc.GetPool(ctx, route.PoolID, route.TenantID); status.Code(err) == codes.NotFound {
		_ = s.dropPoolRoute(ctx, route, "runner has no such pool")
	}
}

// runErrorLine is the frame for a run that fails after its status line went out.
// It is not a RunChunk: it comes from the API, and only as the last line. Its text
// is fixed, as gRPC's own words for a broken connection can name the runner's address.
func runErrorLine(err error) []byte {
	code, message := "run_failed", "the run ended without a result"
	switch status.Code(err) {
	case codes.DeadlineExceeded:
		code, message = "client_stalled", "the client stopped reading the run's output"
	case codes.Unavailable:
		code, message = "runner_unavailable", "the connection to the runner was lost"
	}
	data, _ := json.Marshal(map[string]map[string]string{"error": {"code": code, "message": message}})
	return data
}

// ndjsonWriter writes one flushed line at a time, each under its own deadline.
type ndjsonWriter struct {
	w       http.ResponseWriter
	rc      *http.ResponseController
	timeout time.Duration
}

func (n ndjsonWriter) extendDeadline() {
	_ = n.rc.SetWriteDeadline(time.Now().Add(n.timeout))
}

func (n ndjsonWriter) line(data []byte) error {
	if err := n.rc.SetWriteDeadline(time.Now().Add(n.timeout)); err != nil && !errors.Is(err, http.ErrNotSupported) {
		return err
	}
	if _, err := n.w.Write(append(data, '\n')); err != nil {
		return err
	}
	if err := n.rc.Flush(); err != nil && !errors.Is(err, http.ErrNotSupported) {
		return err
	}
	return nil
}

// poolSandboxExecs is one member's audit trail. The runner refuses a sandbox whose
// record does not name this pool.
func (s *Server) poolSandboxExecs(w http.ResponseWriter, r *http.Request) {
	route, ok := s.poolRouteFor(w, r)
	if !ok {
		return
	}
	rc, err := s.poolRunner(r.Context(), route.RunnerID)
	if err != nil {
		writeRunnerDown(w)
		return
	}
	execs, err := rc.PoolExecs(r.Context(), r.PathValue("id"), route.PoolID)
	if err != nil {
		writeMemberError(w, err)
		return
	}
	writeProto(w, http.StatusOK, &v1.GetSandboxExecsResponse{Execs: execs})
}

// poolSandboxEvents is one member's lifecycle and reset trail. GetSandboxEvents
// has no pool check of its own, so membership is read from the record first.
func (s *Server) poolSandboxEvents(w http.ResponseWriter, r *http.Request) {
	route, ok := s.poolRouteFor(w, r)
	if !ok {
		return
	}
	rc, err := s.poolRunner(r.Context(), route.RunnerID)
	if err != nil {
		writeRunnerDown(w)
		return
	}
	id := r.PathValue("id")
	sb, err := rc.GetSandbox(r.Context(), id)
	if err != nil {
		writeMemberError(w, err)
		return
	}
	if sb.GetPoolId() != route.PoolID {
		writeError(w, http.StatusNotFound, errTenantAccess.Error())
		return
	}
	events, err := rc.Events(r.Context(), id)
	if err != nil {
		writeMemberError(w, err)
		return
	}
	if principalFrom(r.Context()).key.TenantID != "" {
		for _, e := range events {
			e.Reason = eventReasonForTenant(e.GetReason())
			e.RunnerId = ""
		}
	}
	writeProto(w, http.StatusOK, &v1.GetSandboxEventsResponse{Events: events})
}

func writeMemberError(w http.ResponseWriter, err error) {
	if c := status.Code(err); c == codes.NotFound || c == codes.InvalidArgument {
		writeError(w, http.StatusNotFound, errTenantAccess.Error())
		return
	}
	writeGRPCError(w, err)
}

// eventReasonForTenant keeps what a tenant may read of a member's event reason. A
// failed reset, a failed destroy and a failed liveness probe carry the backend's
// own words, which can name host paths, so they are reduced to what happened, as a
// plain create's failure is an opaque 500. It lists what passes, not what is cut,
// so a new kind of event says nothing until someone decides it may.
func eventReasonForTenant(reason string) string {
	kind, detail, found := strings.Cut(reason, ": ")
	if !found {
		return reason // an exit reason from a state transition: a fixed word
	}
	switch {
	case kind == "retire", kind == "reset" && strings.HasPrefix(detail, "ok: "):
		return reason
	case kind == "alive", strings.HasPrefix(detail, "failed"):
		return kind + ": failed"
	default:
		return kind
	}
}

// writeRouteError answers a plain sandbox route that found no route for its id.
// A pool member never has one, but a listing shows it, so a caller holding its id
// is told why it cannot be used. Anything else, including another tenant's member,
// stays the 404 it would be if the sandbox did not exist.
func (s *Server) writeRouteError(w http.ResponseWriter, r *http.Request, id string, err error) {
	if errors.Is(err, errTenantAccess) {
		if poolID := s.memberPool(r.Context(), id); poolID != "" {
			writeError(w, http.StatusConflict, fmt.Sprintf("sandbox %s belongs to pool %s; use the pool API", id, poolID))
			return
		}
	}
	writeError(w, http.StatusNotFound, err.Error())
}

// memberPool returns the pool id of a member this key may see. It asks only the
// runners holding this tenant's own pools, so the cost of a 404 is bounded by what
// the caller has declared, and a tenant with no pools pays none.
func (s *Server) memberPool(ctx context.Context, id string) string {
	if !ulid.ValidPrefix(id, ulid.PrefixSandbox) {
		return ""
	}
	key := principalFrom(ctx).key
	routes, err := s.store.ListPoolRoutes(ctx, key.TenantID)
	if err != nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, poolCallTimeout)
	defer cancel()
	asked := make(map[string]bool, 1)
	for _, route := range routes {
		if asked[route.RunnerID] || !s.canAccessPool(key, route) {
			continue
		}
		asked[route.RunnerID] = true
		rc, err := s.poolRunner(ctx, route.RunnerID)
		if err != nil {
			continue
		}
		sb, err := rc.GetSandbox(ctx, id)
		if err != nil || sb.GetPoolId() == "" {
			continue
		}
		if canAccessTenant(key, sb.GetSpec().GetTenantId()) && canAccessAgent(key, sb.GetSpec().GetAgentId()) {
			return sb.GetPoolId()
		}
	}
	return ""
}
