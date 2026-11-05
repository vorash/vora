package pool

import (
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/vorash/vora/internal/runtime"
)

func TestParseDefaultsAndDerivedMemberSpec(t *testing.T) {
	raw := rawSpec(0, 0)
	raw.Size, raw.MaxSize, raw.BorrowTimeout = nil, nil, 0
	spec := mustParse(t, raw, testLimits)
	if spec.Size() != 8 || spec.MaxSize() != 8 || spec.MaxRuns() != 100 || spec.MaxRun() != 5*time.Minute {
		t.Fatalf("defaults: size %d max_size %d max_runs %d max_run %s", spec.Size(), spec.MaxSize(), spec.MaxRuns(), spec.MaxRun())
	}
	if spec.borrowTimeout != 2*time.Second || spec.maxAge != 30*time.Minute || spec.idleExpiry != time.Hour {
		t.Fatalf("defaults: borrow %s max_age %s idle_expiry %s", spec.borrowTimeout, spec.maxAge, spec.idleExpiry)
	}
	if spec.Reuse() != ReuseReset {
		t.Fatalf("zero reuse parsed as %v, want reset", spec.Reuse())
	}
	m := spec.MemberSpec()
	if m.TenantID != "t1" || m.AgentID != "a1" || m.PoolID != "pool_1" || m.ID != "" {
		t.Fatalf("member spec tenancy: %+v", m)
	}
	if !m.DenyIPC {
		t.Fatal("a reused member must deny IPC")
	}
	if m.TTL != spec.MemberTTL() || m.TTL != 36*time.Minute {
		t.Fatalf("member ttl %s", m.TTL)
	}
	if spec.Raw().Name != "py" {
		t.Fatal("Raw must return the accepted definition")
	}

	cold := mustParse(t, rawSpec(0, 4), Limits{})
	if cold.MemberSpec().DenyIPC {
		t.Fatal("a single-use member keeps IPC")
	}
}

// A workload in one member must not be able to reach the next member's
// environment through a map the two share.
func TestMemberSpecsDoNotShareEnv(t *testing.T) {
	raw := rawSpec(2, 4)
	raw.Template.Env = map[string]string{"A": "1"}
	spec := mustParse(t, raw, testLimits)
	spec.MemberSpec().Env["LEAK"] = "x"
	next := spec.MemberSpec().Env
	if _, leaked := next["LEAK"]; leaked || next["A"] != "1" {
		t.Fatalf("next member's env %v", next)
	}
}

func TestParseRejects(t *testing.T) {
	huge := time.Duration(math.MaxInt64)
	noBackends := func(string) bool { return false }
	cases := []struct {
		name   string
		mutate func(*RawSpec)
		limits Limits
		want   error
	}{
		{"unsafe without the runner's grant", func(r *RawSpec) { r.Reuse = ReuseUnsafeNoReset }, testLimits, ErrUnsafeRefused},
		{"resetting pool on a backend without Reusable", nil, Limits{Reusable: noBackends}, ErrNotReusable},
		{"unsafe pool on a backend without Reusable", func(r *RawSpec) { r.Reuse = ReuseUnsafeNoReset }, Limits{AllowUnsafeReuse: true, Reusable: noBackends}, ErrNotReusable},
		{"unsafe with size 0 would label fresh runs unsafe", func(r *RawSpec) { r.Reuse, *r.Size = ReuseUnsafeNoReset, 0 }, unsafeLimits, ErrInvalidSpec},
		{"unsafe with max_runs 1 would label fresh runs unsafe", func(r *RawSpec) { r.Reuse, r.MaxRuns = ReuseUnsafeNoReset, 1 }, unsafeLimits, ErrInvalidSpec},
		{"unknown reuse", func(r *RawSpec) { r.Reuse = Reuse(7) }, testLimits, ErrInvalidSpec},
		{"pids 10 cannot exec", func(r *RawSpec) { r.Template.Resources.Pids = 10 }, testLimits, ErrInvalidSpec},
		{"pids 11 leaves the workload nothing", func(r *RawSpec) { r.Template.Resources.Pids = 11 }, testLimits, ErrInvalidSpec},
		{"template names an id", func(r *RawSpec) { r.Template.ID = "sbx_x" }, testLimits, ErrInvalidSpec},
		{"template names a tenant", func(r *RawSpec) { r.Template.TenantID = "other" }, testLimits, ErrInvalidSpec},
		{"template names a ttl", func(r *RawSpec) { r.Template.TTL = time.Hour }, testLimits, ErrInvalidSpec},
		{"template without a backend", func(r *RawSpec) { r.Template.Backend = "" }, testLimits, ErrInvalidSpec},
		{"backend the runner lacks", nil, Limits{Reusable: testLimits.Reusable, HasBackend: noBackends}, ErrInvalidSpec},
		{"networked template", func(r *RawSpec) { r.Template.Network.Mode = runtime.NetworkRestricted }, testLimits, ErrInvalidSpec},
		{"template without an image", func(r *RawSpec) { r.Template.Image = "" }, testLimits, ErrInvalidSpec},
		{"memory over the runner cap", func(r *RawSpec) { r.Template.Resources.MemoryBytes = 2 << 20 }, Limits{MaxMemoryBytes: 1 << 20, Reusable: testLimits.Reusable}, ErrInvalidSpec},
		{"cpu over the runner cap", func(r *RawSpec) { r.Template.Resources.CPUMillis = 2000 }, Limits{MaxCPUMillis: 1000, Reusable: testLimits.Reusable}, ErrInvalidSpec},
		{"pids over the runner cap", func(r *RawSpec) { r.Template.Resources.Pids = 17 }, Limits{MaxPids: 16, Reusable: testLimits.Reusable}, ErrInvalidSpec},
		{"disk over the runner cap", func(r *RawSpec) { r.Template.Resources.DiskBytes = 2 << 20 }, Limits{MaxDiskBytes: 1 << 20, Reusable: testLimits.Reusable}, ErrInvalidSpec},
		{"negative size", func(r *RawSpec) { *r.Size = -1 }, testLimits, ErrInvalidSpec},
		{"size above max_size", func(r *RawSpec) { *r.Size = 5 }, testLimits, ErrInvalidSpec},
		{"max_size 0", func(r *RawSpec) { *r.MaxSize = 0 }, testLimits, ErrInvalidSpec},
		{"max_size over the runner cap", func(r *RawSpec) { *r.MaxSize = 5 }, Limits{MaxSize: 4, Reusable: testLimits.Reusable}, ErrInvalidSpec},
		{"max_size over the default cap", func(r *RawSpec) { *r.MaxSize = 1 << 40 }, testLimits, ErrInvalidSpec},
		{"negative max_runs", func(r *RawSpec) { r.MaxRuns = -1 }, testLimits, ErrInvalidSpec},
		{"member ttl over the runner cap", nil, Limits{MaxTTL: 10 * time.Minute, Reusable: testLimits.Reusable}, ErrInvalidSpec},
		{"member ttl over the default cap", func(r *RawSpec) { r.MaxAge = 24 * time.Hour }, testLimits, ErrInvalidSpec},
		{"max_age so large the ttl would overflow", func(r *RawSpec) { r.MaxAge = huge }, Limits{MaxTTL: time.Hour, Reusable: testLimits.Reusable}, ErrInvalidSpec},
		{"max_run so large the ttl would overflow", func(r *RawSpec) { r.MaxRun = huge - time.Minute }, Limits{MaxTTL: time.Hour, Reusable: testLimits.Reusable}, ErrInvalidSpec},
		{"borrow_timeout above the ceiling", func(r *RawSpec) { r.BorrowTimeout = huge }, testLimits, ErrInvalidSpec},
		{"idle_expiry above the ceiling", func(r *RawSpec) { r.IdleExpiry = huge }, testLimits, ErrInvalidSpec},
		{"negative max_age", func(r *RawSpec) { r.MaxAge = -time.Hour }, testLimits, ErrInvalidSpec},
		{"negative max_run", func(r *RawSpec) { r.MaxRun = -time.Hour }, testLimits, ErrInvalidSpec},
		{"negative borrow_timeout", func(r *RawSpec) { r.BorrowTimeout = -time.Hour }, testLimits, ErrInvalidSpec},
		{"negative idle_expiry", func(r *RawSpec) { r.IdleExpiry = -time.Hour }, testLimits, ErrInvalidSpec},
		{"max_age below its floor", func(r *RawSpec) { r.MaxAge = time.Nanosecond }, testLimits, ErrInvalidSpec},
		{"max_run below its floor", func(r *RawSpec) { r.MaxRun = time.Nanosecond }, testLimits, ErrInvalidSpec},
		{"borrow_timeout below its floor", func(r *RawSpec) { r.BorrowTimeout = time.Nanosecond }, testLimits, ErrInvalidSpec},
		{"idle_expiry below its floor", func(r *RawSpec) { r.IdleExpiry = time.Nanosecond }, testLimits, ErrInvalidSpec},
		{"bad name", func(r *RawSpec) { r.Name = "Py Pool" }, testLimits, ErrInvalidSpec},
		{"name too long", func(r *RawSpec) { r.Name = strings.Repeat("a", 65) }, testLimits, ErrInvalidSpec},
		{"tenant too long", func(r *RawSpec) { r.TenantID = strings.Repeat("a", 129) }, testLimits, ErrInvalidSpec},
		{"no tenant", func(r *RawSpec) { r.TenantID = "" }, testLimits, ErrInvalidSpec},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			raw := rawSpec(2, 4)
			if c.mutate != nil {
				c.mutate(&raw)
			}
			_, err := Parse(raw, c.limits)
			if !errors.Is(err, c.want) {
				t.Fatalf("err = %v, want %v", err, c.want)
			}
		})
	}
	if _, err := Parse(rawSpec(0, 4), Limits{Reusable: noBackends}); err != nil {
		t.Fatalf("a cold pool needs no Reusable: %v", err)
	}
}

// Every rejection in TestParseRejects could be Parse refusing too much. These
// sit exactly on each boundary and must pass.
func TestParseAcceptsTheBoundaries(t *testing.T) {
	yes := func(string) bool { return true }
	cases := []struct {
		name   string
		mutate func(*RawSpec)
		limits Limits
	}{
		{"pids 12 is the first limit that leaves the workload a task", func(r *RawSpec) { r.Template.Resources.Pids = 12 }, testLimits},
		{"max_size at the cap", func(r *RawSpec) { *r.MaxSize = 4 }, Limits{MaxSize: 4, Reusable: yes}},
		{"resources at the caps", func(r *RawSpec) {
			r.Template.Resources = runtime.ResourceLimits{CPUMillis: 1000, MemoryBytes: 1 << 20, Pids: 16, DiskBytes: 1 << 20}
		}, Limits{MaxCPUMillis: 1000, MaxMemoryBytes: 1 << 20, MaxPids: 16, MaxDiskBytes: 1 << 20, Reusable: yes}},
		{"durations at the floors", func(r *RawSpec) {
			r.MaxAge, r.MaxRun, r.BorrowTimeout, r.IdleExpiry = minMaxAge, minMaxRun, minBorrowTimeout, minIdleExpiry
		}, testLimits},
		{"durations at the ceiling", func(r *RawSpec) {
			r.MaxAge, r.MaxRun, r.BorrowTimeout, r.IdleExpiry = maxDuration, maxDuration, maxDuration, maxDuration
		}, Limits{MaxTTL: 2*maxDuration + time.Minute, Reusable: yes}},
		{"member ttl at the cap", nil, Limits{MaxTTL: 36 * time.Minute, Reusable: yes}},
		{"a backend the runner has", nil, Limits{Reusable: yes, HasBackend: yes}},
		{"unsafe with reuse possible", func(r *RawSpec) { r.Reuse = ReuseUnsafeNoReset }, unsafeLimits},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			raw := rawSpec(2, 4)
			if c.mutate != nil {
				c.mutate(&raw)
			}
			if _, err := Parse(raw, c.limits); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// The two backends' pids limits count different things, and on gVisor reaching it kills
// the sandbox, so a limit that would be fine under bwrap makes a pool that can never start.
func TestPidsFloorDependsOnTheBackend(t *testing.T) {
	for _, tc := range []struct {
		backend string
		pids    int64
		ok      bool
	}{
		{"bwrap", 11, false},
		{"bwrap", 12, true},
		{"gvisor", 12, false},
		{"gvisor", 63, false},
		{"gvisor", 64, true},
	} {
		raw := rawSpec(4, 8)
		raw.Template.Backend = tc.backend
		raw.Template.Resources.Pids = tc.pids
		_, err := Parse(raw, testLimits)
		if tc.ok && err != nil {
			t.Errorf("%s pids %d: %v", tc.backend, tc.pids, err)
		}
		if !tc.ok && !errors.Is(err, ErrInvalidSpec) {
			t.Errorf("%s pids %d: err = %v, want ErrInvalidSpec", tc.backend, tc.pids, err)
		}
	}
}
