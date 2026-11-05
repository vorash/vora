package pool

import (
	"fmt"
	"time"

	"github.com/vorash/vora/internal/runtime"
)

// memberState is one member's place in the pool. Outside idle exactly one
// actor owns a member: the borrower (busy, and the cold start it runs),
// the resetter, the refill start, or the retirer. The pool's mutex guards
// the state; the owner alone talks to the backend.
type memberState int

const (
	starting memberState = iota + 1
	idle
	busy
	resetting
	retiring
)

func (s memberState) String() string {
	switch s {
	case starting:
		return "starting"
	case idle:
		return "idle"
	case busy:
		return "busy"
	case resetting:
		return "resetting"
	case retiring:
		return "retiring"
	default:
		return fmt.Sprintf("state(%d)", int(s))
	}
}

// transitions is the member state machine. A member leaves the table from
// starting (the start failed, nothing to destroy) or retiring (Destroy
// returned, the slot is free). idle is entered only through toIdle, which is
// the one place a reset's nil error is required.
var transitions = map[memberState][]memberState{
	starting:  {idle, busy, retiring},
	idle:      {busy, retiring},
	busy:      {resetting, idle, retiring},
	resetting: {idle, retiring},
	retiring:  {},
}

func canTransition(from, to memberState) bool {
	for _, allowed := range transitions[from] {
		if allowed == to {
			return true
		}
	}
	return false
}

// RetireCause labels why a member was destroyed.
type RetireCause string

const (
	RetireMaxRuns     RetireCause = "max_runs"
	RetireMaxAge      RetireCause = "max_age"
	RetireShrink      RetireCause = "shrink" // more live members than the target
	RetireIdleExpiry  RetireCause = "idle_expiry"
	RetireResetFailed RetireCause = "reset_failed"
	RetireUnhealthy   RetireCause = "unhealthy" // Alive failed or the exec did not start
	RetireDrain       RetireCause = "drain"
	RetireExpired     RetireCause = "expired" // the runner's TTL reaper
)

type member struct {
	id        string
	reset     runtime.Resettable // nil in a pool that never resets
	state     memberState
	cold      bool // a starting member owned by a borrower, not by refill
	runs      int
	createdAt time.Time
	idleSince time.Time
	lastReset time.Duration
	// retireWhen is set while a member is held by someone else; the next
	// settle applies it.
	retireWhen RetireCause
	destroying bool
	destroyErr error
}

// Fate is what check-in does with a member after its run.
type Fate struct {
	Retire RetireCause // non-empty: destroy
	Reset  bool        // otherwise: reset, or (false) straight back to idle
}

// fate is the whole reuse policy. The first matching row wins. The run's
// outcome is not an input: every surviving member is reset, and the reset is
// the proof. live counts members not retiring, this one included; target is
// the pool's current warm target.
func fate(spec Spec, m *member, now time.Time, live, target int, draining bool) Fate {
	switch {
	case draining:
		return Fate{Retire: RetireDrain}
	case m.retireWhen != "":
		return Fate{Retire: m.retireWhen}
	case m.runs >= spec.maxRuns:
		return Fate{Retire: RetireMaxRuns}
	case now.Sub(m.createdAt) >= spec.maxAge:
		return Fate{Retire: RetireMaxAge}
	case live > target:
		return Fate{Retire: RetireShrink}
	case spec.reuse == ReuseUnsafeNoReset:
		return Fate{}
	default:
		return Fate{Reset: true}
	}
}
