package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/bus"
	"github.com/autonomousengine/autonomous-engine/internal/controller"
	"github.com/autonomousengine/autonomous-engine/internal/evidence"
	"github.com/autonomousengine/autonomous-engine/internal/llm/adapter"
	"github.com/autonomousengine/autonomous-engine/internal/llm/strategy"
	"github.com/autonomousengine/autonomous-engine/internal/llm/tools"
	"github.com/autonomousengine/autonomous-engine/internal/models"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// M3-02: the controller states of a real tool loop — a scripted model
// driving the production executor over a real git worktree — come only
// from the tool evidence, are persisted in order, and label the console
// events. The repository's test runner is a shell script (the detected
// test command), so the fixture needs only sh and git.

// scriptedAdapter replays tool calls, one model turn per entry, then
// answers with a short summary.
type scriptedAdapter struct {
	mu    sync.Mutex
	turns [][]adapter.ToolCall
	n     int
}

func (a *scriptedAdapter) Complete(_ context.Context, _ adapter.CompletionRequest) (*adapter.CompletionResponse, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.n < len(a.turns) {
		calls := a.turns[a.n]
		a.n++
		return &adapter.CompletionResponse{ToolCalls: calls, FinishReason: "tool_calls", InputTokens: 100, OutputTokens: 100}, nil
	}
	return &adapter.CompletionResponse{
		Content:      "Implemented add in calc.sh and covered it with tests/test_calc.sh; the test suite passes.",
		FinishReason: "stop", InputTokens: 100, OutputTokens: 100,
	}, nil
}

func (a *scriptedAdapter) Stream(context.Context, adapter.CompletionRequest) (<-chan adapter.StreamChunk, error) {
	return nil, errors.New("not used")
}
func (a *scriptedAdapter) Capabilities() adapter.ModelCapabilities {
	return adapter.ModelCapabilities{MaxContextTokens: 100000, SupportsFunctions: true}
}
func (a *scriptedAdapter) HealthCheck(context.Context) error { return nil }
func (a *scriptedAdapter) Name() string                      { return "scripted/test" }

var callSeq int

func call(tool string, args map[string]any) []adapter.ToolCall {
	callSeq++
	b, _ := json.Marshal(args)
	return []adapter.ToolCall{{ID: fmt.Sprintf("call_%03d_%s", callSeq, tool), Name: tool, Arguments: string(b)}}
}

// memRecorder is an in-memory controller.Recorder.
type memRecorder struct {
	mu    sync.Mutex
	rows  []controller.EventRow
	state controller.State
}

func (m *memRecorder) AppendEvent(_ context.Context, _ uuid.UUID, row controller.EventRow) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rows = append(m.rows, row)
	return nil
}
func (m *memRecorder) RecordTransition(_ context.Context, _ uuid.UUID, rows []controller.EventRow, to controller.State, _ controller.Detail) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rows = append(m.rows, rows...)
	m.state = to
	return nil
}
func (m *memRecorder) RecordTx(_ context.Context, _ pgx.Tx, _ uuid.UUID, rows []controller.EventRow, to *controller.State, _ *controller.Detail) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rows = append(m.rows, rows...)
	if to != nil {
		m.state = *to
	}
	return nil
}

func (m *memRecorder) transitions() []string {
	var out []string
	for _, r := range m.rows {
		if r.Kind == controller.KindTransition {
			out = append(out, string(r.FromState)+"→"+string(r.ToState))
		}
	}
	return out
}

// consoleBus captures console.stream payloads.
type consoleBus struct {
	bus.MessageBus
	mu      sync.Mutex
	console []map[string]any
}

func (b *consoleBus) Publish(_ context.Context, topic, _ string, payload []byte) error {
	if topic != bus.TopicConsoleStream {
		return nil
	}
	var m map[string]any
	_ = json.Unmarshal(payload, &m)
	b.mu.Lock()
	b.console = append(b.console, m)
	b.mu.Unlock()
	return nil
}

const testRunner = "sh run_tests.sh"

// newShellDir is a repository directory (not a git repo) whose test
// runner runs every tests/*.sh.
func newShellDir(t *testing.T) string {
	t.Helper()
	requireSh(t)
	dir := t.TempDir()
	writeFile(t, dir, "run_tests.sh", "for t in tests/*.sh; do [ -f \"$t\" ] || continue; sh \"$t\" || exit 1; done\n")
	writeFile(t, dir, "README.md", "calc\n")
	return dir
}

// newShellRepo is newShellDir as a git repo (for git_diff).
func newShellRepo(t *testing.T) string {
	t.Helper()
	dir := newShellDir(t)
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	gitIn(t, dir, "init", "-q")
	gitIn(t, dir, "config", "user.email", "t@example.com")
	gitIn(t, dir, "config", "user.name", "t")
	gitIn(t, dir, "add", ".")
	gitIn(t, dir, "commit", "-q", "-m", "init")
	return dir
}

func gitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

const (
	calcTest = `. ./calc.sh && [ "$(add 2 3)" = 5 ]` + "\n"
	calcImpl = "add() { echo $(($1 + $2)); }\n"
)

// tddScript is a model that does test-driven development.
func tddScript() [][]adapter.ToolCall {
	return [][]adapter.ToolCall{
		call("list_files", map[string]any{"path": "."}),
		call("read_file", map[string]any{"path": "tests/test_calc.sh"}),
		call("write_file", map[string]any{"path": "tests/test_calc.sh", "content": calcTest}),
		call("run_command", map[string]any{"command": testRunner}),
		call("read_file", map[string]any{"path": "calc.sh"}),
		call("write_file", map[string]any{"path": "calc.sh", "content": calcImpl}),
		call("run_command", map[string]any{"command": testRunner}),
		call("git_diff", map[string]any{}),
	}
}

// scriptedExecutor is the production executor over repo with the
// production callback feeding run.
func scriptedExecutor(t *testing.T, repo string, workID, taskID uuid.UUID, run *controller.Run) (*tools.Executor, *consoleBus) {
	t.Helper()
	kb := &consoleBus{}
	ex := tools.NewExecutor(repo)
	var turnToolCalls []strategy.ToolCall
	ex.SetCallback(newToolCallback(context.Background(), kb, workID, taskID, run, nil, &turnToolCalls))
	return ex, kb
}

// runScriptedWith drives RunToolLoop with a scripted model.
func runScriptedWith(t *testing.T, ex *tools.Executor, script [][]adapter.ToolCall, workID uuid.UUID) {
	t.Helper()
	req := adapter.CompletionRequest{
		Messages: []adapter.ChatMessage{{Role: adapter.RoleSystem, Content: "sys"}, {Role: adapter.RoleUser, Content: "add add()"}},
		Tools:    tools.SafeImplementationTools(), ToolChoice: "auto",
	}
	if _, err := tools.RunToolLoop(context.Background(), &scriptedAdapter{turns: script}, req, ex, 40, nil, &workID); err != nil {
		t.Fatalf("RunToolLoop: %v", err)
	}
}

// runScripted runs script against an in-memory recorded run.
func runScripted(t *testing.T, repo string, script [][]adapter.ToolCall, rec controller.Recorder) (*controller.Run, *consoleBus) {
	t.Helper()
	workID, taskID := uuid.New(), uuid.New()
	run := controller.NewRun(rec, controller.Started{ID: uuid.New(), Attempt: 1, NextSeq: 2}, workID, taskID.String())
	run.SetTestCommand(testRunner)
	ex, kb := scriptedExecutor(t, repo, workID, taskID, run)
	runScriptedWith(t, ex, script, workID)
	if len(kb.console) == 0 {
		t.Fatal("callback published no console events")
	}
	return run, kb
}

func TestControllerTDDRunReachesGreenFromToolEvidence(t *testing.T) {
	repo := newShellRepo(t)
	rec := &memRecorder{}
	run, kb := runScripted(t, repo, tddScript(), rec)

	want := []string{
		"inspect→test_authored", "test_authored→red_observed", "red_observed→implement",
		"implement→green_observed", "green_observed→diff_reviewed",
	}
	if got := rec.transitions(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("persisted transitions:\n got  %v\n want %v\nrows: %+v", got, want, rec.rows)
	}
	if run.State() != controller.StateDiffReviewed {
		t.Fatalf("run state = %s", run.State())
	}
	// Red and green point at the run_command calls, with their exit codes.
	d := run.Detail()
	if d.Red == nil || d.Red.ExitCode == nil || *d.Red.ExitCode == 0 || d.Green == nil || d.Green.ExitCode == nil || *d.Green.ExitCode != 0 {
		t.Fatalf("red/green evidence = %+v / %+v", d.Red, d.Green)
	}
	// Every persisted seq is unique and increasing.
	for i := 1; i < len(rec.rows); i++ {
		if rec.rows[i].Seq <= rec.rows[i-1].Seq {
			t.Fatalf("seq not increasing at %d: %+v", i, rec.rows)
		}
	}

	// Console events carry the phase after the call and run_command's exit code.
	var phases []string
	for _, ev := range kb.console {
		p, _ := ev["phase"].(string)
		phases = append(phases, ev["tool"].(string)+"="+p)
		if ev["tool"] == "run_command" {
			if _, ok := ev["exit_code"]; !ok {
				t.Fatalf("run_command console event without exit_code: %v", ev)
			}
		} else if _, ok := ev["exit_code"]; ok {
			t.Fatalf("%v console event has an exit_code", ev["tool"])
		}
	}
	wantPhases := "list_files=inspect,read_file=inspect,write_file=test_authored,run_command=red_observed," +
		"read_file=red_observed,write_file=implement,run_command=green_observed,git_diff=diff_reviewed"
	if strings.Join(phases, ",") != wantPhases {
		t.Fatalf("console phases:\n got  %s\n want %s", strings.Join(phases, ","), wantPhases)
	}

	// The summary artifact names the state and the red/green calls.
	f := runSummaryFields(run)
	if f["controller_state"] != "diff_reviewed" || f["red_call_id"] != d.Red.CallID || f["green_call_id"] != d.Green.CallID || f["green_call_id"] == "" {
		t.Fatalf("summary fields = %v", f)
	}
}

// Roadmap 3.1 proof: a model that skips tests cannot reach green_observed,
// however its final answer describes the work.
func TestControllerSkippedTestsNeverReachGreen(t *testing.T) {
	repo := newShellRepo(t)
	rec := &memRecorder{}
	script := [][]adapter.ToolCall{
		call("read_file", map[string]any{"path": "calc.sh"}),
		call("write_file", map[string]any{"path": "calc.sh", "content": calcImpl}),
		call("run_command", map[string]any{"command": testRunner}), // passes: no tests exist
		call("git_diff", map[string]any{}),
	}
	run, kb := runScripted(t, repo, script, rec)

	if got := rec.transitions(); strings.Join(got, ",") != "inspect→implement" {
		t.Fatalf("transitions = %v, want only inspect→implement", got)
	}
	if run.State() != controller.StateImplement {
		t.Fatalf("state = %s, want implement", run.State())
	}
	var noted bool
	for _, r := range rec.rows {
		if r.Kind == controller.KindTool && r.Tool == "run_command" && r.Note == "no test authored this run" {
			noted = true
		}
	}
	if !noted {
		t.Fatalf("the passing test run is not recorded with why=\"no test authored this run\": %+v", rec.rows)
	}
	for _, ev := range kb.console {
		if ev["phase"] == string(controller.StateGreenObserved) {
			t.Fatalf("console labelled an event green: %v", ev)
		}
	}
	if f := runSummaryFields(run); f["green_call_id"] != "" || f["red_call_id"] != "" {
		t.Fatalf("summary fields claim evidence: %v", f)
	}
}

// The sink's hook maps outcomes onto the final event: only a successful,
// verified outcome reaches final_verified.
func TestFinalOutcomeOf(t *testing.T) {
	verified := &evidence.ImplementationEvidence{Outcome: evidence.OutcomeVerified}
	partial := &evidence.ImplementationEvidence{Outcome: evidence.OutcomePartiallyVerified}
	cases := []struct {
		name string
		o    completionOutcome
		want controller.FinalOutcome
	}{
		{"verified success", completionOutcome{msg: &implementationCompleteMessage{Success: true, Evidence: verified}, evidence: verified}, controller.FinalVerified},
		{"verified evidence only on the message", completionOutcome{msg: &implementationCompleteMessage{Success: true, Evidence: verified}}, controller.FinalVerified},
		{"partially verified success", completionOutcome{msg: &implementationCompleteMessage{Success: true, Evidence: partial}}, controller.FinalUnverified},
		{"success without evidence", completionOutcome{msg: &implementationCompleteMessage{Success: true}}, controller.FinalUnverified},
		{"verified evidence but failed message", completionOutcome{msg: &implementationCompleteMessage{Success: false, Evidence: verified}}, controller.FinalFailed},
		{"block", completionOutcome{blockReason: "push rejected", msg: &implementationCompleteMessage{}}, controller.FinalBlocked},
		{"assignment failure only", completionOutcome{assignment: models.AssignmentStatusFailed}, controller.FinalFailed},
	}
	for _, tc := range cases {
		if got, _ := finalOutcomeOf(tc.o); got != tc.want {
			t.Errorf("%s: finalOutcomeOf = %s, want %s", tc.name, got, tc.want)
		}
	}
}
