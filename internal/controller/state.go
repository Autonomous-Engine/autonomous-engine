// Package controller is the implementation run's state model (roadmap 3.1,
// tickets M3-01/M3-02). A run's state says what the tools have shown about
// one implementation attempt — a test file was written, a test run was seen
// failing, a test run was seen passing — and nothing else. States advance
// only through Derive, a pure function of the previous state, one tool
// event and the run's accumulated context; the model's prose is never an
// input. Store persists runs and their events (migration 052); Run ties the
// two together for a live attempt.
package controller

// State is one implementation run's phase.
type State string

const (
	// StateInspect is the initial state: nothing written yet.
	StateInspect State = "inspect"
	// StateTestAuthored: a recognised test file was written this run.
	StateTestAuthored State = "test_authored"
	// StateRedObserved: a recognised test command exited non-zero after a
	// test file was written this run.
	StateRedObserved State = "red_observed"
	// StateImplement: a non-test file was written.
	StateImplement State = "implement"
	// StateGreenObserved: a recognised test command exited 0 while
	// implementing, with a test file written this run.
	StateGreenObserved State = "green_observed"
	// StateDiffReviewed: git_diff, or read_file of a file this run wrote,
	// after green. A weak signal by design.
	StateDiffReviewed State = "diff_reviewed"
	// StateFinalVerified is set only from the final gate's evidence
	// outcome "verified" (internal/evidence), never from a tool call.
	StateFinalVerified State = "final_verified"
	// StateBlocked and StateFailed mirror the agent's outcome paths.
	StateBlocked State = "blocked"
	StateFailed  State = "failed"
)

// States lists every state in lifecycle order (terminal states last). The
// migration 052 CHECK constraint names exactly these values.
var States = []State{
	StateInspect, StateTestAuthored, StateRedObserved, StateImplement,
	StateGreenObserved, StateDiffReviewed, StateFinalVerified,
	StateBlocked, StateFailed,
}

// Valid reports whether s is a known state.
func (s State) Valid() bool {
	for _, k := range States {
		if s == k {
			return true
		}
	}
	return false
}

// Terminal reports whether s ends the run: nothing advances it further.
func (s State) Terminal() bool {
	return s == StateFinalVerified || s == StateBlocked || s == StateFailed
}

// rank orders the non-terminal states for the monotonicity rule.
func (s State) rank() int {
	switch s {
	case StateInspect:
		return 0
	case StateTestAuthored:
		return 1
	case StateRedObserved:
		return 2
	case StateImplement:
		return 3
	case StateGreenObserved:
		return 4
	case StateDiffReviewed:
		return 5
	}
	return 6
}
