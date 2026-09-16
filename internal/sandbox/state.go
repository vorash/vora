// Package sandbox holds the sandbox state machine and its persisted store.
package sandbox

import (
	"fmt"

	"github.com/vorash/vora/internal/runtime"
)

// State is a sandbox lifecycle state.
type State string

const (
	StateRequested  State = "REQUESTED"
	StateScheduling State = "SCHEDULING"
	StateCreating   State = "CREATING"
	StateStarting   State = "STARTING"
	StateRunning    State = "RUNNING"
	StateStopping   State = "STOPPING"
	StateStopped    State = "STOPPED"
	StateFailed     State = "FAILED"
	StateExpired    State = "EXPIRED"
	StateDestroyed  State = "DESTROYED"
)

// Terminal reports whether no further forward transition is possible.
func (s State) Terminal() bool {
	switch s {
	case StateFailed, StateExpired, StateDestroyed:
		return true
	default:
		return false
	}
}

// Active reports whether the sandbox may consume resources.
func (s State) Active() bool {
	switch s {
	case StateRequested, StateScheduling, StateCreating, StateStarting,
		StateRunning, StateStopping:
		return true
	default:
		return false
	}
}

// String implements fmt.Stringer.
func (s State) String() string { return string(s) }

// transitions is the allowed forward state graph. FAILED and EXPIRED are
// reachable from any active state; DESTROYED from any non-destroyed state.
var transitions = map[State][]State{
	StateRequested:  {StateScheduling, StateCreating, StateFailed, StateExpired, StateDestroyed},
	StateScheduling: {StateCreating, StateFailed, StateExpired, StateDestroyed},
	StateCreating:   {StateStarting, StateFailed, StateExpired, StateDestroyed},
	StateStarting:   {StateRunning, StateFailed, StateExpired, StateDestroyed},
	StateRunning:    {StateStopping, StateFailed, StateExpired, StateDestroyed},
	StateStopping:   {StateStopped, StateFailed, StateDestroyed},
	StateStopped:    {StateDestroyed},
	StateFailed:     {StateDestroyed},
	StateExpired:    {StateDestroyed},
	StateDestroyed:  {},
}

// CanTransition reports whether from → to is a legal transition.
func CanTransition(from, to State) bool {
	for _, allowed := range transitions[from] {
		if allowed == to {
			return true
		}
	}
	return false
}

// Transition applies a state change to a record, validating the edge and
// updating the timestamp. The caller is responsible for persisting the record
// beforehand or afterwards; Transition itself does not write.
func Transition(rec *Record, to State, reason runtime.ExitReason) error {
	if rec == nil {
		return fmt.Errorf("sandbox: nil record")
	}
	if rec.State == to {
		return fmt.Errorf("sandbox: already in state %s", to)
	}
	if !CanTransition(rec.State, to) {
		return fmt.Errorf("sandbox: illegal transition %s → %s", rec.State, to)
	}
	rec.State = to
	if reason != "" {
		rec.Reason = reason
	}
	return nil
}
