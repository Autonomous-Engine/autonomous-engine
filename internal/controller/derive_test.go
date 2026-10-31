package controller

import (
	"strings"
	"testing"
)

func exit(n int) *int { return &n }

func write(p string) Event {
	return Event{Kind: EventTool, CallID: "c-" + p, Tool: "write_file", Path: p}
}
func edit(p string) Event {
	return Event{Kind: EventTool, CallID: "c-" + p, Tool: "edit_file", Path: p}
}
func run(cmd string, code int) Event {
	return Event{Kind: EventTool, CallID: "c-run", Tool: "run_command", Command: cmd, ExitCode: exit(code)}
}
func final(o FinalOutcome) Event { return Event{Kind: EventFinal, Outcome: o} }

// replay derives a whole tool sequence the way Run does and returns every
// state visited plus the last why.
func replay(t *testing.T, testCmd string, evs ...Event) ([]State, string) {
	t.Helper()
	state := StateInspect
	rc := RunContext{TestCommand: testCmd}
	path := []State{state}
	var why string
	for _, ev := range evs {
		next, advanced, w := Derive(state, ev, rc)
		why = w
		if advanced {
			if next == state {
				t.Fatalf("advanced=true without a state change at %+v", ev)
			}
			path = append(path, next)
		} else if next != state {
			t.Fatalf("state changed %s→%s with advanced=false at %+v", state, next, ev)
		}
		rc = rc.Observe(ev, next)
		state = next
	}
	return path, why
}

func statesString(ss []State) string {
	parts := make([]string, len(ss))
	for i, s := range ss {
		parts[i] = string(s)
	}
	return strings.Join(parts, " → ")
}

func TestDeriveSequences(t *testing.T) {
	cases := []struct {
		name    string
		testCmd string
		events  []Event
		want    string
		wantWhy string // substring of the last event's why, when set
	}{
		{
			name:    "full TDD cycle (Go)",
			testCmd: "go test ./... -count=1",
			events: []Event{
				write("pkg/add_test.go"), run("go test ./...", 1), edit("pkg/add.go"), run("go test ./...", 0),
				{Kind: EventTool, CallID: "d", Tool: "git_diff"}, final(FinalVerified),
			},
			want: "inspect → test_authored → red_observed → implement → green_observed → diff_reviewed → final_verified",
		},
		{
			// Roadmap 3.1 proof: a model that skips tests cannot reach green_observed.
			name:    "no test written, passing test run stays in implement",
			testCmd: "go test ./... -count=1",
			events:  []Event{edit("pkg/add.go"), run("go test ./...", 0)},
			want:    "inspect → implement",
			wantWhy: "no test authored this run",
		},
		{
			name:    "no test written, passing detected test command stays in implement",
			testCmd: "npm test",
			events:  []Event{write("src/app.ts"), run("npm test", 0), run("npx vitest run", 0)},
			want:    "inspect → implement",
			wantWhy: "no test authored this run",
		},
		{
			name:    "regression: green → implement on a later red, then green again",
			testCmd: "npm test",
			events: []Event{
				write("src/sum.test.ts"), run("npm test", 1), edit("src/sum.ts"), run("npm test", 0),
				run("npm test", 1), edit("src/sum.ts"), run("npm test", 0),
			},
			want: "inspect → test_authored → red_observed → implement → green_observed → implement → green_observed",
		},
		{
			name:    "write after green invalidates green",
			testCmd: "pytest",
			events: []Event{
				write("tests/test_calc.py"), run("pytest", 1), edit("calc.py"), run("python -m pytest -q", 0), edit("calc.py"),
			},
			want: "inspect → test_authored → red_observed → implement → green_observed → implement",
		},
		{
			name:   "red requires a test written earlier",
			events: []Event{run("go test ./...", 1), run("go test ./...", 1)},
			want:   "inspect",
		},
		{
			name:    "red with failing run before the test file does not count",
			events:  []Event{run("go test ./...", 1), write("a_test.go"), run("go test ./...", 0)},
			want:    "inspect → test_authored",
			wantWhy: "test run passed before any failure",
		},
		{
			name:   "source before red goes to implement, green still needs the test",
			events: []Event{write("a_test.go"), edit("a.go"), run("go test ./...", 0)},
			want:   "inspect → test_authored → implement → green_observed",
		},
		{
			name:    "unrecognised command is not evidence",
			events:  []Event{write("a_test.go"), run("echo FAIL; exit 1", 1), run("go build ./...", 1)},
			want:    "inspect → test_authored",
			wantWhy: "",
		},
		{
			name:   "piped test command is not evidence (exit status is the pipe's)",
			events: []Event{write("a_test.go"), run("go test ./... 2>&1 | tail -20", 1)},
			want:   "inspect → test_authored",
		},
		{
			name:    "test command without exit code is not evidence",
			events:  []Event{write("a_test.go"), {Kind: EventTool, CallID: "x", Tool: "run_command", Command: "go test ./..."}},
			want:    "inspect → test_authored",
			wantWhy: "no exit status",
		},
		{
			name:    "errored write is not evidence",
			events:  []Event{{Kind: EventTool, CallID: "x", Tool: "write_file", Path: "a_test.go", IsError: true}},
			want:    "inspect",
			wantWhy: "returned an error",
		},
		{
			name:   "read_file of a changed file after green is diff review; of an untouched file is not",
			events: []Event{write("a_test.go"), run("go test ./...", 1), edit("a.go"), run("go test ./...", 0), {Kind: EventTool, Tool: "read_file", Path: "b.go"}, {Kind: EventTool, Tool: "read_file", Path: "./a.go"}},
			want:   "inspect → test_authored → red_observed → implement → green_observed → diff_reviewed",
		},
		{
			name:   "git_diff before green is not diff review",
			events: []Event{write("a_test.go"), {Kind: EventTool, Tool: "git_diff"}},
			want:   "inspect → test_authored",
		},
		{
			name:   "failure ends the run; nothing advances after",
			events: []Event{write("a_test.go"), final(FinalFailed), run("go test ./...", 1), final(FinalVerified)},
			want:   "inspect → test_authored → failed",
		},
		{
			name:   "blocked from inspect",
			events: []Event{final(FinalBlocked)},
			want:   "inspect → blocked",
		},
		{
			name:    "unverified outcome leaves the state",
			events:  []Event{edit("a.go"), final(FinalUnverified)},
			want:    "inspect → implement",
			wantWhy: "not verified",
		},
		{
			name:   "codex and search tools never move the state",
			events: []Event{{Kind: EventTool, Tool: "codex_find_module"}, {Kind: EventTool, Tool: "search_code"}, {Kind: EventTool, Tool: "list_files"}},
			want:   "inspect",
		},
		{
			name:   "test files written in later states do not regress",
			events: []Event{write("a_test.go"), run("go test ./...", 1), write("b_test.go"), edit("a.go"), write("c_test.go")},
			want:   "inspect → test_authored → red_observed → implement",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, why := replay(t, tc.testCmd, tc.events...)
			if s := statesString(got); s != tc.want {
				t.Fatalf("states:\n got  %s\n want %s", s, tc.want)
			}
			if tc.wantWhy != "" && !strings.Contains(why, tc.wantWhy) {
				t.Fatalf("last why = %q, want it to contain %q", why, tc.wantWhy)
			}
		})
	}
}

// The roadmap's exact proof: why is "no test authored this run".
func TestDeriveProofNoTestAuthored(t *testing.T) {
	rc := RunContext{TestCommand: "go test ./... -count=1", SourceFiles: []string{"a.go"}}
	next, advanced, why := Derive(StateImplement, run("go test ./... -count=1", 0), rc)
	if next != StateImplement || advanced || why != "no test authored this run" {
		t.Fatalf("Derive = (%s, %t, %q), want (implement, false, \"no test authored this run\")", next, advanced, why)
	}
}

// Monotonic: every advance moves forward in lifecycle order except the
// regressions to implement and the moves to blocked / failed.
func TestDeriveMonotonic(t *testing.T) {
	events := []Event{
		write("a_test.go"), write("a.go"), run("go test ./...", 0), run("go test ./...", 1),
		{Kind: EventTool, Tool: "git_diff"}, {Kind: EventTool, Tool: "read_file", Path: "a.go"},
		final(FinalVerified), final(FinalBlocked), final(FinalFailed), final(FinalUnverified),
	}
	contexts := []RunContext{
		{}, {TestFiles: []string{"a_test.go"}}, {TestFiles: []string{"a_test.go"}, SourceFiles: []string{"a.go"}, RedSeen: true},
	}
	for _, prev := range States {
		for _, ev := range events {
			for _, rc := range contexts {
				next, advanced, _ := Derive(prev, ev, rc)
				if !advanced {
					continue
				}
				if !next.Valid() {
					t.Fatalf("Derive(%s) produced invalid state %q", prev, next)
				}
				switch {
				case next.Terminal():
				case next == StateImplement && (prev == StateGreenObserved || prev == StateDiffReviewed):
				case next.rank() > prev.rank():
				default:
					t.Fatalf("non-monotonic %s → %s on %+v", prev, next, ev)
				}
				if prev.Terminal() {
					t.Fatalf("terminal %s advanced to %s", prev, next)
				}
			}
		}
	}
}

// final_verified is reachable only from a final event with outcome verified.
func TestFinalVerifiedOnlyFromFinalGate(t *testing.T) {
	rc := RunContext{TestFiles: []string{"a_test.go"}, SourceFiles: []string{"a.go"}, RedSeen: true}
	tools := []Event{write("x.go"), run("go test ./...", 0), {Kind: EventTool, Tool: "git_diff"}, {Kind: EventTool, Tool: "read_file", Path: "a.go"}}
	for _, prev := range States {
		for _, ev := range tools {
			if next, _, _ := Derive(prev, ev, rc); next == StateFinalVerified && prev != StateFinalVerified {
				t.Fatalf("tool %s moved %s to final_verified", ev.Tool, prev)
			}
		}
	}
}

func TestTransitionLogLine(t *testing.T) {
	ev := Event{Kind: EventTool, CallID: "3f2a1b2c3d4e5f60", Tool: "run_command", Command: "go test ./...", ExitCode: exit(1)}
	next, _, why := Derive(StateTestAuthored, ev, RunContext{TestFiles: []string{"a_test.go"}})
	got := Transition{From: StateTestAuthored, To: next, Why: why}.LogLine()
	want := "controller: red_observed (go test ./... exit 1, call 3f2a1b2c3d4e…)"
	if got != want {
		t.Fatalf("LogLine = %q, want %q", got, want)
	}
}
