// Package pool keeps warm sandboxes for one tenant and lends them one run at
// a time. It is pure policy over an injected Sandboxes adapter: no gRPC, no
// backend, no record store.
package pool

import (
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/vorash/vora/internal/runtime"
)

// ID is a runner-minted pool id.
type ID string

// Reuse is what happens to a member between runs.
type Reuse int

const (
	// ReuseReset is the default: a member comes back only after a reset that
	// proved it clean.
	ReuseReset Reuse = iota
	// ReuseUnsafeNoReset returns a member with nothing cleaned. It exists for
	// the isolation experiment's baseline arm. Parse refuses it unless the
	// runner granted Limits.AllowUnsafeReuse, and every run on such a pool
	// reports it in its Placement.
	ReuseUnsafeNoReset
)

func (r Reuse) String() string {
	switch r {
	case ReuseReset:
		return "reset"
	case ReuseUnsafeNoReset:
		return "unsafe_no_reset"
	default:
		return fmt.Sprintf("reuse(%d)", int(r))
	}
}

// Errors Parse returns, wrapped, so a caller can map them to a status code.
var (
	ErrInvalidSpec   = errors.New("pool: invalid spec")
	ErrUnsafeRefused = errors.New("pool: unsafe reuse is not allowed on this runner")
	ErrNotReusable   = errors.New("pool: backend cannot reset sandboxes")
)

// minPids is the smallest pids limit a member can run with. An idle bwrap
// sandbox holds a steady 10 or 11 tasks; at 10 it creates but cannot exec, and
// at 11 the exec loop passed 150/150 on vora-mini (design build log, v0.10.6).
const minPids = 11

const (
	defaultSize          = 8
	defaultMaxRuns       = 100
	defaultMaxAge        = 30 * time.Minute
	defaultMaxRun        = 5 * time.Minute
	defaultBorrowTimeout = 2 * time.Second
	defaultIdleExpiry    = time.Hour
)

var namePattern = regexp.MustCompile(`^[a-z0-9-]{1,64}$`)

// RawSpec is a pool definition as the wire delivers it, before validation.
// Zero means "default" for every field but Size, where 0 is a cold pool, so
// Size and MaxSize are pointers.
type RawSpec struct {
	ID       ID
	Name     string
	TenantID string
	AgentID  string
	// Template is every member's sandbox spec. Its ID, TenantID, AgentID and
	// TTL must be empty: the pool owns identity and lifetime.
	Template      runtime.SandboxSpec
	Size          *int
	MaxSize       *int
	MaxRuns       int
	MaxAge        time.Duration
	MaxRun        time.Duration
	BorrowTimeout time.Duration
	IdleExpiry    time.Duration
	Reuse         Reuse
}

// Limits are the runner-wide rules a spec is checked against.
type Limits struct {
	// MaxTTL caps a member's record TTL (VORA_MAX_TTL). Zero means no cap.
	MaxTTL time.Duration
	// AllowUnsafeReuse is VORA_POOL_UNSAFE_REUSE=allow.
	AllowUnsafeReuse bool
	// Reusable reports whether a backend implements runtime.Reusable.
	Reusable func(backend string) bool
}

// Spec is a validated pool definition. Only Parse builds one, so a Spec in
// hand is one the runner's limits accepted.
type Spec struct {
	raw           RawSpec
	size          int
	maxSize       int
	maxRuns       int
	maxAge        time.Duration
	maxRun        time.Duration
	borrowTimeout time.Duration
	idleExpiry    time.Duration
	reuse         Reuse
}

// Parse fills defaults and validates. Defaults: size 8, max_size max(size, 1),
// max_runs 100, max_age 30 min, max_run 5 min, borrow_timeout 2 s,
// idle_expiry 1 h, reuse reset.
func Parse(raw RawSpec, limits Limits) (Spec, error) {
	s := Spec{
		raw:           raw,
		size:          defaultSize,
		maxRuns:       raw.MaxRuns,
		maxAge:        raw.MaxAge,
		maxRun:        raw.MaxRun,
		borrowTimeout: raw.BorrowTimeout,
		idleExpiry:    raw.IdleExpiry,
		reuse:         raw.Reuse,
	}
	if raw.Size != nil {
		s.size = *raw.Size
	}
	s.maxSize = max(s.size, 1)
	if raw.MaxSize != nil {
		s.maxSize = *raw.MaxSize
	}
	if s.maxRuns == 0 {
		s.maxRuns = defaultMaxRuns
	}
	if s.maxAge == 0 {
		s.maxAge = defaultMaxAge
	}
	if s.maxRun == 0 {
		s.maxRun = defaultMaxRun
	}
	if s.borrowTimeout == 0 {
		s.borrowTimeout = defaultBorrowTimeout
	}
	if s.idleExpiry == 0 {
		s.idleExpiry = defaultIdleExpiry
	}

	switch {
	case raw.ID == "":
		return Spec{}, fmt.Errorf("%w: id is required", ErrInvalidSpec)
	case !namePattern.MatchString(raw.Name):
		return Spec{}, fmt.Errorf("%w: name must match %s", ErrInvalidSpec, namePattern)
	case raw.TenantID == "":
		return Spec{}, fmt.Errorf("%w: tenant is required", ErrInvalidSpec)
	case s.size < 0 || s.maxSize < 1 || s.size > s.maxSize:
		return Spec{}, fmt.Errorf("%w: need 0 <= size <= max_size and max_size >= 1", ErrInvalidSpec)
	case s.maxRuns < 1:
		return Spec{}, fmt.Errorf("%w: max_runs must be >= 1", ErrInvalidSpec)
	case s.maxAge < 0 || s.maxRun < 0 || s.borrowTimeout < 0 || s.idleExpiry < 0:
		return Spec{}, fmt.Errorf("%w: durations must not be negative", ErrInvalidSpec)
	case limits.MaxTTL > 0 && s.MemberTTL() > limits.MaxTTL:
		return Spec{}, fmt.Errorf("%w: max_age + max_run + 1m exceeds the runner's max TTL", ErrInvalidSpec)
	}
	if err := checkTemplate(raw.Template); err != nil {
		return Spec{}, err
	}
	switch s.reuse {
	case ReuseReset:
		if s.resets() && (limits.Reusable == nil || !limits.Reusable(raw.Template.Backend)) {
			return Spec{}, fmt.Errorf("%w: %q", ErrNotReusable, raw.Template.Backend)
		}
	case ReuseUnsafeNoReset:
		if !limits.AllowUnsafeReuse {
			return Spec{}, ErrUnsafeRefused
		}
	default:
		return Spec{}, fmt.Errorf("%w: unknown reuse %v", ErrInvalidSpec, s.reuse)
	}
	return s, nil
}

func checkTemplate(t runtime.SandboxSpec) error {
	switch {
	case t.ID != "" || t.TenantID != "" || t.AgentID != "" || t.TTL != 0:
		return fmt.Errorf("%w: template must not set id, tenant, agent or ttl", ErrInvalidSpec)
	case t.Backend == "":
		return fmt.Errorf("%w: template must name a backend", ErrInvalidSpec)
	case t.Network.Mode != runtime.NetworkNone:
		return fmt.Errorf("%w: pools support network mode %q only", ErrInvalidSpec, runtime.NetworkNone)
	case t.Resources.Pids < minPids:
		return fmt.Errorf("%w: pids must be >= %d (the agent's own tasks)", ErrInvalidSpec, minPids)
	}
	t.ID = "member"
	if err := t.Validate(); err != nil {
		return fmt.Errorf("%w: template: %w", ErrInvalidSpec, err)
	}
	return nil
}

// Raw is the definition Parse accepted, for persistence. Re-parse it on load,
// so a runner that lost its unsafe grant refuses the pool instead of running
// it.
func (s Spec) Raw() RawSpec { return s.raw }

func (s Spec) ID() ID                { return s.raw.ID }
func (s Spec) Name() string          { return s.raw.Name }
func (s Spec) TenantID() string      { return s.raw.TenantID }
func (s Spec) AgentID() string       { return s.raw.AgentID }
func (s Spec) Size() int             { return s.size }
func (s Spec) MaxSize() int          { return s.maxSize }
func (s Spec) MaxRuns() int          { return s.maxRuns }
func (s Spec) MaxRun() time.Duration { return s.maxRun }
func (s Spec) Reuse() Reuse          { return s.reuse }

// MemberTTL is the TTL stamped on every member's record. The pool retires a
// member at max_age and never lends one past it, so the runner's TTL reaper
// fires only if the pool itself is wedged.
func (s Spec) MemberTTL() time.Duration { return s.maxAge + s.maxRun + time.Minute }

// reused reports whether a member can serve more than one run.
func (s Spec) reused() bool { return s.size > 0 && s.maxRuns > 1 }

// resets reports whether members are reset between runs.
func (s Spec) resets() bool { return s.reused() && s.reuse == ReuseReset }

// MemberSpec derives one member's sandbox spec. Tenant, agent and pool come
// from the pool, so a member cannot belong to anyone else. The ID is left for
// the adapter to mint. A reused member denies IPC, because a reset cannot
// clean it; the unsafe arm gets the same so the arms differ only in the reset.
func (s Spec) MemberSpec() runtime.SandboxSpec {
	m := s.raw.Template
	m.TenantID = s.raw.TenantID
	m.AgentID = s.raw.AgentID
	m.PoolID = string(s.raw.ID)
	m.TTL = s.MemberTTL()
	m.DenyIPC = s.reused()
	return m
}
