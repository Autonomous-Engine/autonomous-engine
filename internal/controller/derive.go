package controller

import (
	"fmt"
	"sort"
	"strings"
)

// EventKind distinguishes tool evidence from the final gate's verdict.
type EventKind string

const (
	// EventTool is one executed tool call (the executor callback).
	EventTool EventKind = "tool"
	// EventFinal is the agent's recorded outcome: the final gate's evidence
	// outcome on success, or the failure/block path that ended the run.
	EventFinal EventKind = "final"
)

// FinalOutcome is what an EventFinal carries.
type FinalOutcome string

const (
	// FinalVerified: the evidence outcome was "verified" and the task
	// succeeded. The only way to reach StateFinalVerified.
	FinalVerified FinalOutcome = "verified"
	// FinalBlocked: the outcome blocked the work item.
	FinalBlocked FinalOutcome = "blocked"
	// FinalFailed: the outcome reported failure without blocking.
	FinalFailed FinalOutcome = "failed"
	// FinalUnverified: the task completed but its evidence outcome was not
	// "verified" (partially_verified, unverified). The state is left as the
	// tools last showed it.
	FinalUnverified FinalOutcome = "unverified"
)

// Event is one input to Derive.
type Event struct {
	Kind EventKind
	// Tool evidence (Kind == EventTool).
	CallID string
	Tool   string
	// Path is the file argument of write_file / edit_file / read_file.
	Path string
	// Command and ExitCode come from run_command's structured result.
	// ExitCode is nil when the command did not run to an exit status
	// (rejected, failed to start, killed by its timeout).
	Command  string
	ExitCode *int
	// IsError is the tool result's error flag: a rejected or failed tool
	// call shows nothing and never advances the state.
	IsError bool

	// Outcome is set for Kind == EventFinal.
	Outcome FinalOutcome
}

// RunContext is what the run has shown before the event being derived.
// Derive only reads it; Observe returns the context after an event.
type RunContext struct {
	// TestCommand is the repository's detected test command (may be "").
	TestCommand string
	// TestFiles and SourceFiles are the paths written this run.
	TestFiles   []string
	SourceFiles []string
	// RedSeen records that red_observed was reached this run.
	RedSeen bool
}

// TestAuthored reports whether a test file was written this run.
func (rc RunContext) TestAuthored() bool { return len(rc.TestFiles) > 0 }

// Wrote reports whether this run wrote p.
func (rc RunContext) Wrote(p string) bool {
	p = normalizePath(p)
	return contains(rc.TestFiles, p) || contains(rc.SourceFiles, p)
}

// Observe returns the context after ev was derived into next. It records
// successful writes and whether red was reached; it never decides state.
func (rc RunContext) Observe(ev Event, next State) RunContext {
	out := rc
	out.TestFiles = append([]string(nil), rc.TestFiles...)
	out.SourceFiles = append([]string(nil), rc.SourceFiles...)
	if ev.Kind == EventTool && !ev.IsError && isWriteTool(ev.Tool) {
		p := normalizePath(ev.Path)
		if p != "" {
			if IsTestFile("", p) {
				out.TestFiles = addSorted(out.TestFiles, p)
			} else {
				out.SourceFiles = addSorted(out.SourceFiles, p)
			}
		}
	}
	if next == StateRedObserved {
		out.RedSeen = true
	}
	return out
}

// Derive is the controller's only transition function. Given the previous
// state, one event and the run context before the event, it returns the
// next state, whether that is a change, and why (the evidence for a
// change; for no change, why the event was not evidence, or "" when the
// event is irrelevant to the lifecycle).
//
// Rules (ROADMAP M3-01/02 ruling):
//
//	inspect        + test-file write                         → test_authored
//	inspect        + non-test write                          → implement   (no test authored)
//	test_authored  + test command exit≠0                     → red_observed
//	test_authored  + non-test write                          → implement   (before any red)
//	red_observed   + non-test write                          → implement
//	implement      + test command exit 0, test authored      → green_observed
//	implement      + test command exit 0, no test authored   → (stays)     "no test authored this run"
//	green_observed + git_diff | read_file of a written file  → diff_reviewed
//	green_observed | diff_reviewed + test command exit≠0     → implement   (regression)
//	green_observed | diff_reviewed + any write               → implement   (green no longer covers the tree)
//	any non-terminal + final(verified)                       → final_verified
//	any non-terminal + final(blocked | failed)               → blocked | failed
//	terminal       + anything                                → (stays)
//
// Everything else leaves the state unchanged: states only move forward,
// except the regression back to implement and the moves to blocked/failed.
func Derive(prev State, ev Event, rc RunContext) (next State, advanced bool, why string) {
	if prev.Terminal() {
		return prev, false, ""
	}
	if ev.Kind == EventFinal {
		return deriveFinal(prev, ev)
	}
	if ev.IsError {
		if isWriteTool(ev.Tool) || ev.Tool == "run_command" {
			return prev, false, fmt.Sprintf("%s returned an error; not evidence", ev.Tool)
		}
		return prev, false, ""
	}

	switch {
	case isWriteTool(ev.Tool):
		return deriveWrite(prev, ev)
	case ev.Tool == "run_command":
		return deriveRun(prev, ev, rc)
	case ev.Tool == "git_diff":
		if prev == StateGreenObserved {
			return StateDiffReviewed, true, fmt.Sprintf("git_diff after green, call %s", shortID(ev.CallID))
		}
	case ev.Tool == "read_file":
		if prev == StateGreenObserved && rc.Wrote(ev.Path) {
			return StateDiffReviewed, true, fmt.Sprintf("read_file %s (changed this run) after green, call %s", normalizePath(ev.Path), shortID(ev.CallID))
		}
	}
	return prev, false, ""
}

func deriveFinal(prev State, ev Event) (State, bool, string) {
	switch ev.Outcome {
	case FinalVerified:
		return StateFinalVerified, true, "final gate evidence outcome verified"
	case FinalBlocked:
		return StateBlocked, true, "outcome blocked the work item"
	case FinalFailed:
		return StateFailed, true, "outcome reported failure"
	}
	return prev, false, fmt.Sprintf("final outcome %q is not verified; state stays as the tools showed it", string(ev.Outcome))
}

func deriveWrite(prev State, ev Event) (State, bool, string) {
	p := normalizePath(ev.Path)
	if p == "" {
		return prev, false, fmt.Sprintf("%s without a path; not evidence", ev.Tool)
	}
	isTest := IsTestFile("", p)
	evidence := fmt.Sprintf("%s %s, call %s", ev.Tool, p, shortID(ev.CallID))
	switch prev {
	case StateInspect:
		if isTest {
			return StateTestAuthored, true, "test file written: " + evidence
		}
		return StateImplement, true, "source written with no test authored this run: " + evidence
	case StateTestAuthored:
		if isTest {
			return prev, false, ""
		}
		return StateImplement, true, "source written before any failing test run was observed: " + evidence
	case StateRedObserved:
		if isTest {
			return prev, false, ""
		}
		return StateImplement, true, "source written after red: " + evidence
	case StateGreenObserved, StateDiffReviewed:
		return StateImplement, true, "file written after green; green no longer covers the tree: " + evidence
	}
	return prev, false, ""
}

func deriveRun(prev State, ev Event, rc RunContext) (State, bool, string) {
	if !IsTestCommand(ev.Command, rc.TestCommand) {
		return prev, false, ""
	}
	if ev.ExitCode == nil {
		return prev, false, fmt.Sprintf("test command %q has no exit status (rejected, not started or timed out); not evidence", ev.Command)
	}
	code := *ev.ExitCode
	evidence := fmt.Sprintf("%s exit %d, call %s", ev.Command, code, shortID(ev.CallID))
	if code != 0 {
		switch prev {
		case StateTestAuthored:
			return StateRedObserved, true, evidence
		case StateGreenObserved, StateDiffReviewed:
			return StateImplement, true, "tests failed after green: " + evidence
		}
		if !rc.TestAuthored() {
			return prev, false, "test run failed but no test authored this run"
		}
		return prev, false, ""
	}
	switch prev {
	case StateImplement:
		if !rc.TestAuthored() {
			return prev, false, "no test authored this run"
		}
		if !rc.RedSeen {
			return StateGreenObserved, true, evidence + " (no failing run was observed before implementation)"
		}
		return StateGreenObserved, true, evidence
	case StateInspect:
		return prev, false, "no test authored this run"
	case StateTestAuthored:
		return prev, false, "test run passed before any failure was observed"
	case StateRedObserved:
		return prev, false, "test run passed with no implementation written after red"
	}
	return prev, false, ""
}

func isWriteTool(tool string) bool {
	switch tool {
	case "write_file", "edit_file", "insert_after", "append_to_file":
		return true
	}
	return false
}

// shortID shortens a tool call id for log lines.
func shortID(id string) string {
	if len(id) > 12 {
		return id[:12] + "…"
	}
	if id == "" {
		return "?"
	}
	return id
}

func contains(xs []string, x string) bool {
	i := sort.SearchStrings(xs, x)
	return i < len(xs) && xs[i] == x
}

func addSorted(xs []string, x string) []string {
	i := sort.SearchStrings(xs, x)
	if i < len(xs) && xs[i] == x {
		return xs
	}
	xs = append(xs, "")
	copy(xs[i+1:], xs[i:])
	xs[i] = x
	return xs
}

// Transition is one state change with its evidence.
type Transition struct {
	From, To State
	Why      string
	Event    Event
	Seq      int64
}

// LogLine is the operator-facing lifecycle line, e.g.
// "controller: red_observed (go test ./... exit 1, call 3f2a…)".
func (t Transition) LogLine() string {
	return fmt.Sprintf("controller: %s (%s)", t.To, strings.TrimSpace(t.Why))
}
