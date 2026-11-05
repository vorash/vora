package runner

import (
	"context"
	"errors"
	"fmt"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	v1 "github.com/vorash/vora/api/proto/v1"
	"github.com/vorash/vora/internal/pool"
)

// maxSeconds bounds a wire duration so the multiplication cannot overflow.
// pool.Parse rejects anything over 30 days anyway.
const maxSeconds = int64(31 * 24 * 60 * 60)

func secondsFromProto(name string, n int64) (time.Duration, error) {
	if n < 0 || n > maxSeconds {
		return 0, fmt.Errorf("%s must be between 0 and %d", name, maxSeconds)
	}
	return time.Duration(n) * time.Second, nil
}

// poolSpecFromProto builds the raw definition pool.Parse validates. The
// template is converted with no default TTL, because a template must not
// carry one: the pool sets every member's TTL.
func poolSpecFromProto(p *v1.PoolSpec) (pool.RawSpec, error) {
	if p == nil {
		return pool.RawSpec{}, fmt.Errorf("spec is required")
	}
	template, err := specFromProto(p.GetSandbox(), 0)
	if err != nil {
		return pool.RawSpec{}, fmt.Errorf("sandbox: %w", err)
	}
	raw := pool.RawSpec{
		Name:     p.GetName(),
		TenantID: p.GetTenantId(),
		AgentID:  p.GetAgentId(),
		Template: template,
		MaxRuns:  int(p.GetMaxRuns()),
	}
	if p.Size != nil {
		n := int(p.GetSize())
		raw.Size = &n
	}
	if p.MaxSize != nil {
		n := int(p.GetMaxSize())
		raw.MaxSize = &n
	}
	for _, d := range []struct {
		name  string
		value int64
		out   *time.Duration
	}{
		{"max_age_seconds", p.GetMaxAgeSeconds(), &raw.MaxAge},
		{"max_run_seconds", p.GetMaxRunSeconds(), &raw.MaxRun},
		{"idle_expiry_seconds", p.GetIdleExpirySeconds(), &raw.IdleExpiry},
	} {
		if *d.out, err = secondsFromProto(d.name, d.value); err != nil {
			return pool.RawSpec{}, err
		}
	}
	ms := p.GetBorrowTimeoutMs()
	if ms < 0 || ms > maxSeconds*1000 {
		return pool.RawSpec{}, fmt.Errorf("borrow_timeout_ms must be between 0 and %d", maxSeconds*1000)
	}
	raw.BorrowTimeout = time.Duration(ms) * time.Millisecond
	switch p.GetReuse() {
	case v1.PoolReuse_POOL_REUSE_UNSPECIFIED, v1.PoolReuse_POOL_REUSE_RESET:
		raw.Reuse = pool.ReuseReset
	case v1.PoolReuse_POOL_REUSE_UNSAFE_NO_RESET:
		raw.Reuse = pool.ReuseUnsafeNoReset
	default:
		return pool.RawSpec{}, fmt.Errorf("unknown reuse %v", p.GetReuse())
	}
	return raw, nil
}

// poolSpecToProto is the spec with every default filled in. Two declares
// are "the same spec" when these are equal, whether or not one of them
// spelled out a default.
func poolSpecToProto(spec pool.Spec) *v1.PoolSpec {
	raw := spec.Raw()
	size, maxSize, maxRuns := uint32(spec.Size()), uint32(spec.MaxSize()), uint32(spec.MaxRuns())
	maxAge, maxRun := int64(spec.MaxAge()/time.Second), int64(spec.MaxRun()/time.Second)
	borrow, idle := int64(spec.BorrowTimeout()/time.Millisecond), int64(spec.IdleExpiry()/time.Second)
	return &v1.PoolSpec{
		Name:              raw.Name,
		TenantId:          raw.TenantID,
		AgentId:           raw.AgentID,
		Sandbox:           specToProto(raw.Template),
		Size:              &size,
		MaxSize:           &maxSize,
		MaxRuns:           &maxRuns,
		MaxAgeSeconds:     &maxAge,
		MaxRunSeconds:     &maxRun,
		BorrowTimeoutMs:   &borrow,
		IdleExpirySeconds: &idle,
		Reuse:             reuseToProto(spec.Reuse()),
	}
}

func reuseToProto(r pool.Reuse) v1.PoolReuse {
	if r == pool.ReuseUnsafeNoReset {
		return v1.PoolReuse_POOL_REUSE_UNSAFE_NO_RESET
	}
	return v1.PoolReuse_POOL_REUSE_RESET
}

func poolStatsToProto(st pool.Stats) *v1.PoolStats {
	return &v1.PoolStats{
		Starting:           uint32(st.Starting),
		Idle:               uint32(st.Idle),
		Busy:               uint32(st.Busy),
		Resetting:          uint32(st.Resetting),
		Retiring:           uint32(st.Retiring),
		RunsTotal:          st.RunsTotal,
		WarmRunsTotal:      st.WarmRunsTotal,
		ResetsTotal:        st.ResetsTotal,
		ResetFailuresTotal: st.ResetFailuresTotal,
		DestroyedTotal:     st.DestroyedTotal,
		LastStartError:     st.LastStartError,
		ExhaustedTotal:     st.ExhaustedTotal,
		StartFailedTotal:   st.StartFailedTotal,
		Dormant:            st.Dormant,
	}
}

func (s *Server) poolToProto(e *poolEntry) *v1.Pool {
	state := v1.PoolState_POOL_ACTIVE
	if e.draining.Load() {
		state = v1.PoolState_POOL_DRAINING
	}
	return &v1.Pool{
		Id:              string(e.spec.ID()),
		Spec:            poolSpecToProto(e.spec),
		State:           state,
		RunnerId:        s.runnerID,
		Stats:           poolStatsToProto(e.pool.Stats()),
		CreatedAtUnixMs: e.createdAt.UnixMilli(),
	}
}

// notLoadedToProto reports a refused pool. Parse refused its definition, so the
// spec is as it was declared, with no defaults filled in.
func (s *Server) notLoadedToProto(n *notLoadedPool) *v1.Pool {
	raw := n.rec.Spec
	return &v1.Pool{
		Id: string(n.rec.ID),
		Spec: &v1.PoolSpec{
			Name:     raw.Name,
			TenantId: raw.TenantID,
			AgentId:  raw.AgentID,
			Sandbox:  specToProto(raw.Template),
			Reuse:    reuseToProto(raw.Reuse),
		},
		State:           v1.PoolState_POOL_NOT_LOADED,
		RunnerId:        s.runnerID,
		Stats:           &v1.PoolStats{},
		CreatedAtUnixMs: n.rec.CreatedAt.UnixMilli(),
		NotLoadedReason: n.reason,
	}
}

func runFrameToProto(poolID pool.ID, pl pool.Placement, executionID string) *v1.RunChunk {
	return &v1.RunChunk{Payload: &v1.RunChunk_Run{Run: &v1.PoolRun{
		PoolId:      string(poolID),
		SandboxId:   pl.SandboxID,
		ExecutionId: executionID,
		Warm:        pl.Warm,
		StartupMs:   pl.Startup.Milliseconds(),
		ResetMs:     pl.PriorReset.Milliseconds(),
		SandboxRun:  uint32(pl.SandboxRun),
		Reuse:       reuseToProto(pl.Reuse),
	}}}
}

func runDoneToProto(d pool.Done) *v1.RunChunk {
	outcome := v1.PoolResetOutcome_POOL_RESET_UNSPECIFIED
	switch d.Reset {
	case pool.ResetOK:
		outcome = v1.PoolResetOutcome_POOL_RESET_OK
	case pool.ResetFailed:
		outcome = v1.PoolResetOutcome_POOL_RESET_FAILED
	case pool.ResetSkipped:
		outcome = v1.PoolResetOutcome_POOL_RESET_SKIPPED
	case pool.ResetRetired:
		outcome = v1.PoolResetOutcome_POOL_RESET_RETIRED
	}
	return &v1.RunChunk{Payload: &v1.RunChunk_RunDone{RunDone: &v1.PoolRunDone{
		Outcome:     outcome,
		ResetMs:     d.Duration.Milliseconds(),
		RetireCause: string(d.Cause),
	}}}
}

// execToRunChunk re-frames an exec chunk for a run stream.
func execToRunChunk(c *v1.ExecChunk) *v1.RunChunk {
	switch p := c.GetPayload().(type) {
	case *v1.ExecChunk_Stdout:
		return &v1.RunChunk{Payload: &v1.RunChunk_Stdout{Stdout: p.Stdout}}
	case *v1.ExecChunk_Stderr:
		return &v1.RunChunk{Payload: &v1.RunChunk_Stderr{Stderr: p.Stderr}}
	case *v1.ExecChunk_Exit:
		return &v1.RunChunk{Payload: &v1.RunChunk_Exit{Exit: p.Exit}}
	default:
		return &v1.RunChunk{}
	}
}

// poolStatus maps a pool error to its gRPC status. An error that already is
// one (from runExec) passes through.
func poolStatus(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, pool.ErrExhausted):
		return status.Error(codes.ResourceExhausted, err.Error())
	case errors.Is(err, pool.ErrDraining):
		return status.Error(codes.FailedPrecondition, err.Error())
	case errors.Is(err, pool.ErrColdStart), errors.Is(err, pool.ErrNotStarted):
		return status.Error(codes.Unavailable, err.Error())
	case errors.Is(err, pool.ErrInvalidSpec):
		return status.Error(codes.InvalidArgument, err.Error())
	case errors.Is(err, pool.ErrUnsafeRefused), errors.Is(err, pool.ErrNotReusable):
		return status.Error(codes.FailedPrecondition, err.Error())
	case errors.Is(err, context.Canceled):
		return status.Error(codes.Canceled, err.Error())
	case errors.Is(err, context.DeadlineExceeded):
		return status.Error(codes.DeadlineExceeded, err.Error())
	}
	if _, ok := status.FromError(err); ok {
		return err
	}
	return status.Error(codes.Internal, err.Error())
}
