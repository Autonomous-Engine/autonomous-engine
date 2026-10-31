package tools

import (
	"context"
	"encoding/json"
	"os/exec"
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/llm/adapter"
)

// M3-01: run_command results carry the exit code, duration and command as
// structured fields, and the executor callback receives them. The content
// string the model sees is unchanged.

type callbackRecord struct {
	callID, tool, args, result string
	isError                    bool
	meta                       ResultMeta
}

func runTool(t *testing.T, e *Executor, id, tool string, args map[string]any) adapter.ToolResult {
	t.Helper()
	b, _ := json.Marshal(args)
	return e.Execute(context.Background(), adapter.ToolCall{ID: id, Name: tool, Arguments: string(b)})
}

func TestRunCommandStructuredResultAndCallback(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
	e := NewExecutor(t.TempDir())
	var got []callbackRecord
	e.SetCallback(func(callID, toolName, arguments, result string, isError bool, meta ResultMeta) {
		got = append(got, callbackRecord{callID, toolName, arguments, result, isError, meta})
	})

	ok := runTool(t, e, "c-ok", "run_command", map[string]any{"command": "echo hi"})
	if ok.ExitCode == nil || *ok.ExitCode != 0 || ok.Command != "echo hi" || ok.IsError || ok.DurationMs < 0 {
		t.Fatalf("passing command result = %+v", ok)
	}
	if strings.Contains(ok.Content, "exit code") {
		t.Fatalf("content of a passing command changed: %q", ok.Content)
	}

	fail := runTool(t, e, "c-fail", "run_command", map[string]any{"command": "echo boom; exit 3"})
	if fail.ExitCode == nil || *fail.ExitCode != 3 {
		t.Fatalf("failing command exit code = %v", fail.ExitCode)
	}
	if fail.IsError {
		t.Fatal("a non-zero exit must not set IsError (behaviour unchanged)")
	}
	if !strings.Contains(fail.Content, "boom") || !strings.Contains(fail.Content, "[exit code: exit status 3]") {
		t.Fatalf("failing command content changed: %q", fail.Content)
	}

	rejected := runTool(t, e, "c-rej", "run_command", map[string]any{"command": "sudo ls"})
	if !rejected.IsError || rejected.ExitCode != nil || rejected.Command != "sudo ls" {
		t.Fatalf("rejected command result = %+v", rejected)
	}

	timedOut := runTool(t, e, "c-slow", "run_command", map[string]any{"command": "sleep 5", "timeout_seconds": 1})
	if timedOut.ExitCode != nil {
		t.Fatalf("a command killed by its timeout has no exit status, got %d", *timedOut.ExitCode)
	}

	list := runTool(t, e, "c-list", "list_files", map[string]any{"path": "."})
	if list.ExitCode != nil || list.Command != "" || list.DurationMs != 0 {
		t.Fatalf("non-command tool carries structured fields: %+v", list)
	}

	if len(got) != 5 {
		t.Fatalf("callback ran %d times, want 5", len(got))
	}
	if m := got[1].meta; got[1].callID != "c-fail" || m.ExitCode == nil || *m.ExitCode != 3 || m.Command != "echo boom; exit 3" {
		t.Fatalf("callback meta for failing command = %+v", got[1])
	}
	if got[1].result != fail.Content {
		t.Fatal("callback result differs from the tool result content")
	}
	if got[2].meta.ExitCode != nil || !got[2].isError {
		t.Fatalf("callback for rejected command = %+v", got[2])
	}
	if got[4].meta != (ResultMeta{}) {
		t.Fatalf("callback meta for list_files = %+v, want zero", got[4].meta)
	}
}

// Invalid arguments reach the callback with zero meta.
func TestCallbackInvalidArguments(t *testing.T) {
	e := NewExecutor(t.TempDir())
	var meta *ResultMeta
	e.SetCallback(func(_, _, _, _ string, isError bool, m ResultMeta) {
		if !isError {
			t.Fatal("invalid arguments must be an error")
		}
		meta = &m
	})
	e.Execute(context.Background(), adapter.ToolCall{ID: "x", Name: "run_command", Arguments: "{"})
	if meta == nil || *meta != (ResultMeta{}) {
		t.Fatalf("meta = %+v", meta)
	}
}
