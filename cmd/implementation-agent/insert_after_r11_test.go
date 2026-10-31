package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/llm/adapter"
	"github.com/autonomousengine/autonomous-engine/internal/llm/tools"
)

// attempt11TestEdits are attempt 11's edit_file calls on
// tests/unit/server.test.ts (implementation inference rows 15, 16, 32 and
// 33), rewritten onto the demo-app fixture: the four the duplicate guards
// rejected, and the one that finally applied (row 34).
type attempt11TestEdits struct {
	Rejected []struct {
		Row       int            `json:"inference_row"`
		Arguments map[string]any `json:"arguments"`
	} `json:"rejected"`
	Applied struct {
		Row       int            `json:"inference_row"`
		Arguments map[string]any `json:"arguments"`
	} `json:"applied_in_attempt_11"`
}

func loadAttempt11TestEdits(t *testing.T) attempt11TestEdits {
	t.Helper()
	b, err := os.ReadFile("testdata/attempt11/test_file_edits.json")
	if err != nil {
		t.Fatal(err)
	}
	var e attempt11TestEdits
	if err := json.Unmarshal(b, &e); err != nil {
		t.Fatal(err)
	}
	if len(e.Rejected) != 4 {
		t.Fatalf("%d rejected edits", len(e.Rejected))
	}
	return e
}

var steerAnchorRE = regexp.MustCompile(`insert_after\(path="([^"]+)", anchor_line=(\d+)`)

// followAdapter replays attempt 11's read and four rejected edits, then
// does what the last rejection says: one insert_after at the line it
// names, with only the new lines (the content attempt 11's model finally
// managed to add at row 34).
type followAdapter struct {
	steps   [][]adapter.ToolCall
	n       int
	text    string
	results []string // tool results seen, in order
	inserts int
}

func (a *followAdapter) Complete(_ context.Context, req adapter.CompletionRequest) (*adapter.CompletionResponse, error) {
	if m := req.Messages[len(req.Messages)-1]; m.Role == adapter.RoleTool {
		a.results = append(a.results, m.Content)
	}
	if a.n < len(a.steps) {
		calls := a.steps[a.n]
		a.n++
		return &adapter.CompletionResponse{ToolCalls: calls, FinishReason: "tool_calls"}, nil
	}
	if a.inserts == 0 && len(a.results) > 0 {
		m := steerAnchorRE.FindStringSubmatch(a.results[len(a.results)-1])
		if m == nil {
			return &adapter.CompletionResponse{Content: "No guidance: giving up.", FinishReason: "stop"}, nil
		}
		a.inserts++
		args, _ := json.Marshal(map[string]any{"path": m[1], "anchor_line": json.Number(m[2]), "text": a.text})
		return &adapter.CompletionResponse{ToolCalls: []adapter.ToolCall{{ID: "ins", Name: "insert_after", Arguments: string(args)}}, FinishReason: "tool_calls"}, nil
	}
	return &adapter.CompletionResponse{Content: "Added the client-config test suite.", FinishReason: "stop"}, nil
}
func (a *followAdapter) Stream(context.Context, adapter.CompletionRequest) (<-chan adapter.StreamChunk, error) {
	return nil, errors.New("not used")
}
func (a *followAdapter) Capabilities() adapter.ModelCapabilities {
	return adapter.ModelCapabilities{MaxContextTokens: 32768, SupportsFunctions: true}
}
func (a *followAdapter) HealthCheck(context.Context) error { return nil }
func (a *followAdapter) Name() string                      { return "follow/attempt11" }

// R11 S2 replay: attempt 11's four rejected edits of
// tests/unit/server.test.ts each get the steering ("to add new content
// after line N use insert_after(…); to change lines a-b, quote only those
// lines as old_string"), and a model that follows it lands the test with
// one insert_after call — the file then has the new suite once and the old
// one once. On df414a9 the rejections named no other call.
func TestReplayAttempt11RejectedEditsSteerToInsertAfter(t *testing.T) {
	edits := loadAttempt11TestEdits(t)
	root := copyAttempt9Tree(t)
	const path = "tests/unit/server.test.ts"
	orig, err := os.ReadFile(filepath.Join(root, path))
	if err != nil {
		t.Fatal(err)
	}

	steps := [][]adapter.ToolCall{call("read_file", map[string]any{"path": path})}
	for i, e := range edits.Rejected {
		b, _ := json.Marshal(e.Arguments)
		steps = append(steps, []adapter.ToolCall{{ID: fmt.Sprintf("e%d", i), Name: "edit_file", Arguments: string(b)}})
	}
	applied := edits.Applied.Arguments
	text := strings.TrimSuffix(applied["new_string"].(string), applied["old_string"].(string))
	model := &followAdapter{steps: steps, text: text}

	req := adapter.CompletionRequest{
		Messages: []adapter.ChatMessage{{Role: adapter.RoleSystem, Content: "sys"}, {Role: adapter.RoleUser, Content: "Add a vitest unit test for /api/client-config."}},
		Tools:    withoutLSPUnlessReady(editTools(), false),
	}
	opts := implementationLoopOptions(req.Tools, func() bool { return false }, attempt9Files)
	ex := replayExecutor(root)
	if _, err := tools.RunToolLoopWithOptions(context.Background(), model, req, ex, 20, nil, nil, opts); err != nil {
		t.Fatalf("loop: %v", err)
	}

	if len(model.results) < 6 {
		t.Fatalf("tool results = %d: %q", len(model.results), model.results)
	}
	wantAnchor := []string{"122", "122", "20", "20"}
	for i, r := range model.results[1:5] {
		t.Logf("rejection %d (row %d): %s", i+1, edits.Rejected[i].Row, firstLines(r, 2))
		if !strings.HasPrefix(r, "REJECTED") {
			t.Fatalf("edit %d was not rejected: %s", i+1, firstLines(r, 2))
		}
		m := steerAnchorRE.FindStringSubmatch(r)
		if m == nil || m[1] != path || m[2] != wantAnchor[i] || !strings.Contains(r, "quote only those lines as old_string") {
			t.Fatalf("rejection %d does not steer to insert_after at line %s:\n%s", i+1, wantAnchor[i], r)
		}
	}
	if model.inserts != 1 {
		t.Fatalf("insert_after calls = %d, want 1", model.inserts)
	}
	last := model.results[5]
	if !strings.HasPrefix(last, "Inserted ") {
		t.Fatalf("insert_after result: %s", last)
	}
	got, _ := os.ReadFile(filepath.Join(root, path))
	s := string(got)
	if strings.Count(s, "describe('API Client Config'") != 1 || strings.Count(s, "describe('Places Server'") != 1 ||
		strings.Count(s, "import { describe, it, expect, vi, beforeAll } from 'vitest';") != 1 {
		t.Fatalf("file after insert:\n%s", s)
	}
	lines := strings.Split(string(orig), "\n")
	want := strings.Join(lines[:20], "\n") + "\n" + text + strings.Join(lines[20:], "\n")
	if s != want {
		t.Fatalf("insert_after did not add exactly the new lines after line 20")
	}
}

func firstLines(s string, n int) string {
	ls := strings.SplitN(s, "\n", n+1)
	if len(ls) > n {
		ls = ls[:n]
	}
	return strings.Join(ls, " | ")
}
