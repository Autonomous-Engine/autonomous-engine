package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/llm/adapter"
)

func stuckTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.ts"), []byte("export const a = 1;\nexport const b = 2;\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

func badEdit(i int) scriptedCall {
	args, _ := json.Marshal(map[string]string{"path": "a.ts", "old_string": "export const nope = " + strings.Repeat("9", i) + ";", "new_string": "x"})
	return scriptedCall{"edit_file", string(args)}
}

var readA = scriptedCall{"read_file", `{"path":"a.ts"}`}

func stuckLoop(t *testing.T, root string, calls []scriptedCall, opts LoopOptions) (*scriptAdapter, error) {
	t.Helper()
	model := &scriptAdapter{turns: turnsFor(calls, func(int) string { return "" })}
	req := adapter.CompletionRequest{
		Messages: []adapter.ChatMessage{{Role: adapter.RoleSystem, Content: "s"}, {Role: adapter.RoleUser, Content: "task"}},
		Tools:    []adapter.Tool{ReadFile(), EditFile()},
	}
	_, err := RunToolLoopWithOptions(context.Background(), model, req, NewExecutor(root), 40, nil, nil, opts)
	return model, err
}

// A read between identical failing edits no longer resets the count: the
// third identical failure ends the loop with LoopStuck, and the second
// carries the first failure verbatim.
func TestStuckControlCountsOverTheWholeLoop(t *testing.T) {
	root := stuckTree(t)
	calls := []scriptedCall{readA, badEdit(1), readA, badEdit(1), readA, badEdit(1), readA, readA}
	model, err := stuckLoop(t, root, calls, LoopOptions{StuckControl: true})
	var s *LoopStuck
	if !errors.As(err, &s) || !errors.Is(err, ErrLoopStuck) {
		t.Fatalf("err = %v, want LoopStuck", err)
	}
	if s.Tool != "edit_file" || s.Path != "a.ts" || s.Attempts != 3 || !strings.Contains(s.Reason, "not found") {
		t.Fatalf("LoopStuck = %+v", *s)
	}
	if len(model.reqs) != 6 {
		t.Fatalf("calls = %d, want 6 (ended after the third failing edit)", len(model.reqs))
	}
	// Request 5 carries the second failure's result with the first appended.
	var second string
	for _, m := range model.reqs[4].Messages {
		if m.Role == adapter.RoleTool && strings.Contains(m.Content, "[repeated call]") {
			second = m.Content
		}
	}
	if second == "" || strings.Count(second, "old_string not found") != 2 {
		t.Fatalf("second failure does not carry the first:\n%s", second)
	}
}

// Five failed edits of one path end the loop whatever their arguments.
func TestStuckControlCapsFailedEditsPerPath(t *testing.T) {
	root := stuckTree(t)
	calls := []scriptedCall{readA, badEdit(1), badEdit(2), badEdit(3), badEdit(4), badEdit(5), badEdit(6)}
	model, err := stuckLoop(t, root, calls, LoopOptions{StuckControl: true})
	var s *LoopStuck
	if !errors.As(err, &s) || s.Attempts != 5 || s.Path != "a.ts" {
		t.Fatalf("err = %v", err)
	}
	if len(model.reqs) != 6 {
		t.Fatalf("calls = %d, want 6", len(model.reqs))
	}
}

// INSUFFICIENT is a structured failure, not a final answer.
func TestStuckControlInsufficientEndsTheLoop(t *testing.T) {
	root := stuckTree(t)
	model := &scriptAdapter{turns: append(turnsFor([]scriptedCall{readA, badEdit(1)}, func(int) string { return "" }),
		adapter.CompletionResponse{Content: "INSUFFICIENT: the edit keeps failing.", FinishReason: "stop"})}
	req := adapter.CompletionRequest{Messages: []adapter.ChatMessage{{Role: adapter.RoleSystem, Content: "s"}, {Role: adapter.RoleUser, Content: "task"}}, Tools: []adapter.Tool{ReadFile(), EditFile()}}
	_, err := RunToolLoopWithOptions(context.Background(), model, req, NewExecutor(root), 40, nil, nil, LoopOptions{StuckControl: true})
	var s *LoopStuck
	if !errors.As(err, &s) || s.Path != "a.ts" || !strings.Contains(s.Reason, "INSUFFICIENT") {
		t.Fatalf("err = %v", err)
	}
}

// A call an earlier pass was stuck on ends this loop at its first failure.
func TestStuckControlPriorStuckEndsAtFirstRepeat(t *testing.T) {
	root := stuckTree(t)
	ex := NewExecutor(root)
	fp, _ := ex.callFingerprint(adapter.ToolCall{Name: "edit_file", Arguments: badEdit(1).args})
	prior := []LoopStuck{{Tool: "edit_file", Path: "a.ts", Reason: "not found", Attempts: 3, Fingerprint: fp}}
	model, err := stuckLoop(t, root, []scriptedCall{readA, badEdit(2), badEdit(1), readA}, LoopOptions{StuckControl: true, PriorStuck: prior})
	var s *LoopStuck
	if !errors.As(err, &s) || !strings.Contains(s.Reason, "earlier pass") || s.Attempts != 4 {
		t.Fatalf("err = %v", err)
	}
	if len(model.reqs) != 3 {
		t.Fatalf("calls = %d, want 3", len(model.reqs))
	}
}

// A tool the loop dropped stays dropped across RefreshTools, and a call to
// it (emitted as text, so it still arrives) is refused with the reason.
func droppedToolRun(t *testing.T, stuckControl bool) adapter.CompletionRequest {
	t.Helper()
	root := stuckTree(t)
	same := scriptedCall{"read_file", `{"path":"a.ts"}`}
	calls := []scriptedCall{same, same, same, same, same}
	model := &scriptAdapter{turns: turnsFor(calls, func(int) string { return "" })}
	base := []adapter.Tool{ReadFile(), EditFile()}
	req := adapter.CompletionRequest{Messages: []adapter.ChatMessage{{Role: adapter.RoleSystem, Content: "s"}, {Role: adapter.RoleUser, Content: "task"}}, Tools: base}
	opts := LoopOptions{RefreshTools: func() []adapter.Tool { return base }, StuckControl: stuckControl}
	if _, err := RunToolLoopWithOptions(context.Background(), model, req, NewExecutor(root), 40, nil, nil, opts); err != nil {
		t.Fatal(err)
	}
	return model.reqs[len(model.reqs)-1]
}

func lastToolResult(r adapter.CompletionRequest) string {
	for i := len(r.Messages) - 1; i >= 0; i-- {
		if r.Messages[i].Role == adapter.RoleTool {
			return r.Messages[i].Content
		}
	}
	return ""
}

// Under StuckControl a tool the loop dropped stays dropped across
// RefreshTools, and a call to it (emitted as text, so it still arrives) is
// refused with the reason.
func TestDroppedToolStaysDroppedAcrossRefresh(t *testing.T) {
	last := droppedToolRun(t, true)
	// Turns 1-4 repeat read_file: the 4th is the third repeat, read_file is
	// dropped; turn 5's read_file is refused, and no later request offers it.
	for _, tl := range last.Tools {
		if tl.Name == "read_file" {
			t.Fatal("read_file offered again after it was dropped")
		}
	}
	if !strings.Contains(lastToolResult(last), `"read_file" is no longer available for this task`) {
		t.Fatalf("a call to the dropped tool was not refused with the reason: %s", lastToolResult(last))
	}
}

// Without StuckControl (research) the behaviour is 360e324's: the tool is
// hidden from the offered list but a call to it still runs.
func TestDroppedToolRunsWithoutStuckControl(t *testing.T) {
	last := droppedToolRun(t, false)
	for _, tl := range last.Tools {
		if tl.Name == "read_file" {
			t.Fatal("read_file offered again after it was stripped")
		}
	}
	if got := lastToolResult(last); !strings.Contains(got, "export const a = 1;") {
		t.Fatalf("stripped tool's call did not run: %s", got)
	}
}

// R10 review: under StuckControl the sliding-window exit (varied calls
// that never make progress) is a structured LoopStuck, not a prompt or a
// plain error; without it the old error remains.
func TestStuckControlSlidingWindowExitIsLoopStuck(t *testing.T) {
	var calls []scriptedCall
	for i := 1; i <= 40; i++ {
		calls = append(calls, scriptedCall{"read_file", fmt.Sprintf(`{"path":"a.ts","start_line":%d,"end_line":%d}`, i%2+1, i)})
	}
	_, err := stuckLoop(t, stuckTree(t), calls, LoopOptions{StuckControl: true})
	var s *LoopStuck
	if !errors.As(err, &s) || !strings.Contains(s.Reason, "distinct tool patterns") {
		t.Fatalf("err = %v, want LoopStuck", err)
	}
	_, err = stuckLoop(t, stuckTree(t), calls, LoopOptions{})
	if err == nil || errors.As(err, &s) {
		t.Fatalf("without StuckControl err = %v, want the plain error", err)
	}
}
