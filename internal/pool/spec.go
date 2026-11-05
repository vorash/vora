// Package pool keeps warm sandboxes for one tenant and lends them one run at
// a time. It is pure policy over an injected Sandboxes adapter: no gRPC, no
// backend, no record store.
package pool

import (
	"errors"
	"fmt"
	"maps"
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
	// the isolation experiment's baseline arm, and that post-run reset is the
	// only way it differs from ReuseReset: its members get the same admission
	// Prepare, Alive probe, DenyIPC, max_runs and max_age. So it needs a
	// Reusable backend too. Parse refuses it unless the runner granted
	// Limits.AllowUnsafeReuse, or when every run would be fresh anyway (size 0
	// or max_runs 1), and every run on such a pool reports it in its Placement.
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
// sandbox holds a steady 10 or 11 tasks, so 12 is the first limit that always
// leaves one for the workload. This floor was measured for bwrap exec (design
// build log, v0.10.6). Steps 10 and 11 must re-measure it with reset running.
const minPids = 12

const (
	defaultSize          = 8
	defaultMaxRuns       = 100
	defaultMaxAge        = 30 * time.Minute
	defaultMaxRun        = 5 * time.Minute
	defaultBorrowTimeout = 2 * time.Second
	defaultIdleExpiry    = time.Hour
)

// Duration floors. Each keeps one bad spec from turning into a loop of member
// starts and destroys: max_age 1 ns measured 8543 cycles in 200 ms.
const (
	minMaxAge        = time.Minute
	minMaxRun        = time.Second
	minBorrowTimeout = time.Millisecond
	minIdleExpiry    = time.Minute
)

// maxDuration caps every duration, so MemberTTL's sum cannot overflow past
// Limits.MaxTTL.
const maxDuration = 30 * 24 * time.Hour

// What a zero cap in Limits means. The resource and TTL values are the ones
// the runner's enforceResourceLimits falls back to, and a runner test holds
// them equal. defaultMaxPoolSize is VORA_MAX_SANDBOXES' default.
const (
	defaultMaxTTL               = 24 * time.Hour
	defaultMaxMemoryBytes int64 = 8 << 30
	defaultMaxCPUMillis   int64 = 8000
	defaultMaxPids        int64 = 512
	defaultMaxDiskBytes   int64 = 8 << 30
	defaultMaxPoolSize          = 64
)

// The tenant and name make up a pool's metric label, so both are bounded. Series
// stay bounded too: each pool reserves at least one of the runner's sandbox slots,
// and the runner deletes a pool's series with it.
var namePattern = regexp.MustCompile(`^[a-z0-9-]{1,64}$`)

const maxTenantBytes = 128

// RawSpec is a pool definition as the wire delivers it, before validation.
// Zero means "default" for every field but Size, where 0 is a cold pool, so
// Size and MaxSize are pointers.
type RawSpec struct {
	ID       ID     `json:"id"`
	Name     string `json:"name"`
	TenantID string `json:"tenant_id"`
	AgentID  string `json:"agent_id,omitempty"`
	// Template is every member's sandbox spec. Its ID, TenantID, AgentID and
	// TTL must be empty: the pool owns identity and lifetime.
	Template      runtime.SandboxSpec `json:"template"`
	Size          *int                `json:"size,omitempty"`
	MaxSize       *int                `json:"max_size,omitempty"`
	MaxRuns       int                 `json:"max_runs,omitempty"`
	MaxAge        time.Duration       `json:"max_age_ns,omitempty"`
	MaxRun        time.Duration       `json:"max_run_ns,omitempty"`
	BorrowTimeout time.Duration       `json:"borrow_timeout_ns,omitempty"`
	IdleExpiry    time.Duration       `json:"idle_expiry_ns,omitempty"`
	Reuse         Reuse               `json:"reuse,omitempty"`
}

// Limits are the runner-wide rules a spec is checked against, so a pool that
// could only fail every start is refused when it is declared. A zero cap means
// the same as it does in the runner's enforceResourceLimits: the built-in
// default, not "no cap".
type Limits struct {
	// MaxTTL caps a member's record TTL (VORA_MAX_TTL).
	MaxTTL time.Duration
	// MaxMemoryBytes, MaxCPUMillis, MaxPids and MaxDiskBytes cap one member's
	// resources (VORA_MAX_MEMORY, VORA_MAX_CPU, VORA_MAX_PIDS, VORA_MAX_DISK).
	MaxMemoryBytes int64
	MaxCPUMillis   int64
	MaxPids        int64
	MaxDiskBytes   int64
	// MaxSize caps a pool's max_size.
	MaxSize int
	// AllowUnsafeReuse is VORA_POOL_UNSAFE_REUSE=allow.
	AllowUnsafeReuse bool
	// Reusable reports whether a backend implements runtime.Reusable.
	Reusable func(backend string) bool
	// HasBackend reports whether this runner has a backend at all. Nil skips
	// the check, so the runner must set it: a cold pool on a backend the runner
	// lacks otherwise parses and then fails every start.
	HasBackend func(backend string) bool
}

func (l Limits) withDefaults() Limits {
	if l.MaxTTL <= 0 {
		l.MaxTTL = defaultMaxTTL
	}
	if l.MaxMemoryBytes <= 0 {
		l.MaxMemoryBytes = defaultMaxMemoryBytes
	}
	if l.MaxCPUMillis <= 0 {
		l.MaxCPUMillis = defaultMaxCPUMillis
	}
	if l.MaxPids <= 0 {
		l.MaxPids = defaultMaxPids
	}
	if l.MaxDiskBytes <= 0 {
		l.MaxDiskBytes = defaultMaxDiskBytes
	}
	if l.MaxSize <= 0 {
		l.MaxSize = defaultMaxPoolSize
	}
	return l
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
	limits = limits.withDefaults()
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
	case raw.TenantID == "" || len(raw.TenantID) > maxTenantBytes:
		return Spec{}, fmt.Errorf("%w: tenant is required, at most %d bytes", ErrInvalidSpec, maxTenantBytes)
	case s.size < 0 || s.maxSize < 1 || s.size > s.maxSize:
		return Spec{}, fmt.Errorf("%w: need 0 <= size <= max_size and max_size >= 1", ErrInvalidSpec)
	case s.maxSize > limits.MaxSize:
		return Spec{}, fmt.Errorf("%w: max_size exceeds the runner's %d", ErrInvalidSpec, limits.MaxSize)
	case s.maxRuns < 1:
		return Spec{}, fmt.Errorf("%w: max_runs must be >= 1", ErrInvalidSpec)
	}
	for _, d := range []struct {
		name       string
		value, min time.Duration
	}{
		{"max_age", s.maxAge, minMaxAge},
		{"max_run", s.maxRun, minMaxRun},
		{"borrow_timeout", s.borrowTimeout, minBorrowTimeout},
		{"idle_expiry", s.idleExpiry, minIdleExpiry},
	} {
		if d.value < d.min || d.value > maxDuration {
			return Spec{}, fmt.Errorf("%w: %s must be between %s and %s", ErrInvalidSpec, d.name, d.min, maxDuration)
		}
	}
	if s.MemberTTL() > limits.MaxTTL {
		return Spec{}, fmt.Errorf("%w: max_age + max_run + 1m exceeds the runner's max TTL", ErrInvalidSpec)
	}
	if err := checkTemplate(raw.Template, limits); err != nil {
		return Spec{}, err
	}
	switch s.reuse {
	case ReuseReset:
	case ReuseUnsafeNoReset:
		if !limits.AllowUnsafeReuse {
			return Spec{}, ErrUnsafeRefused
		}
		if !s.reused() {
			return Spec{}, fmt.Errorf("%w: unsafe reuse needs size > 0 and max_runs > 1, or every run is fresh", ErrInvalidSpec)
		}
	default:
		return Spec{}, fmt.Errorf("%w: unknown reuse %v", ErrInvalidSpec, s.reuse)
	}
	if s.reused() && (limits.Reusable == nil || !limits.Reusable(raw.Template.Backend)) {
		return Spec{}, fmt.Errorf("%w: %q", ErrNotReusable, raw.Template.Backend)
	}
	return s, nil
}

func checkTemplate(t runtime.SandboxSpec, limits Limits) error {
	r := t.Resources
	switch {
	case t.ID != "" || t.TenantID != "" || t.AgentID != "" || t.TTL != 0:
		return fmt.Errorf("%w: template must not set id, tenant, agent or ttl", ErrInvalidSpec)
	case t.Backend == "":
		return fmt.Errorf("%w: template must name a backend", ErrInvalidSpec)
	case limits.HasBackend != nil && !limits.HasBackend(t.Backend):
		return fmt.Errorf("%w: this runner has no backend %q", ErrInvalidSpec, t.Backend)
	case t.Network.Mode != runtime.NetworkNone:
		return fmt.Errorf("%w: pools support network mode %q only", ErrInvalidSpec, runtime.NetworkNone)
	case r.Pids < minPids:
		return fmt.Errorf("%w: pids must be >= %d (the agent's own tasks)", ErrInvalidSpec, minPids)
	case r.MemoryBytes > limits.MaxMemoryBytes || r.CPUMillis > limits.MaxCPUMillis || r.Pids > limits.MaxPids || r.DiskBytes > limits.MaxDiskBytes:
		return fmt.Errorf("%w: template resources exceed the runner's limits", ErrInvalidSpec)
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

func (s Spec) ID() ID                       { return s.raw.ID }
func (s Spec) Name() string                 { return s.raw.Name }
func (s Spec) TenantID() string             { return s.raw.TenantID }
func (s Spec) AgentID() string              { return s.raw.AgentID }
func (s Spec) Size() int                    { return s.size }
func (s Spec) MaxSize() int                 { return s.maxSize }
func (s Spec) MaxRuns() int                 { return s.maxRuns }
func (s Spec) MaxAge() time.Duration        { return s.maxAge }
func (s Spec) MaxRun() time.Duration        { return s.maxRun }
func (s Spec) BorrowTimeout() time.Duration { return s.borrowTimeout }
func (s Spec) IdleExpiry() time.Duration    { return s.idleExpiry }
func (s Spec) Reuse() Reuse                 { return s.reuse }

// MemberTTL is the TTL stamped on every member's record. The pool lends no
// member past max_age and retires it at check-in, and the runner's fn holds a
// run to max_run, so the TTL reaper fires only if the pool itself is wedged or
// Sweep has stopped.
func (s Spec) MemberTTL() time.Duration { return s.maxAge + s.maxRun + time.Minute }

// reused reports whether a member can serve more than one run. Such a member
// is prepared at admission whether or not the pool resets it after a run.
func (s Spec) reused() bool { return s.size > 0 && s.maxRuns > 1 }

// MemberSpec derives one member's sandbox spec. Tenant, agent and pool come
// from the pool, so a member cannot belong to anyone else. The ID is left for
// the adapter to mint. A reused member denies IPC, because a reset cannot
// clean it; the unsafe arm gets the same so the arms differ only in the reset.
func (s Spec) MemberSpec() runtime.SandboxSpec {
	m := s.raw.Template
	m.Env = maps.Clone(m.Env)
	m.TenantID = s.raw.TenantID
	m.AgentID = s.raw.AgentID
	m.PoolID = string(s.raw.ID)
	m.TTL = s.MemberTTL()
	m.DenyIPC = s.reused()
	return m
}
