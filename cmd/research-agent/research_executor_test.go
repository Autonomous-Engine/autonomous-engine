package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/llm/adapter"
	"github.com/google/uuid"
)

// Review #1: the research executor read the shared clone through the codex
// read tools while its tree was the pinned snapshot, so no read was ever
// marked as coverage and record_finding refused every citation. The
// executor reads its own tree (the pin) as the task's repository, and a
// finding on what it read is accepted.
func TestResearchExecutorReadsThePinnedTreeAndRecordsFindings(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "options.go"), []byte("package enmime\n\n// Option configures the parser.\ntype Option func(*Parser)\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	workID := uuid.New()
	ex := newResearchExecutor(dir, "enmime", nil, nil, nil, nil, &workID, "task-1", "code_trace")
	ctx := context.Background()
	read := ex.Execute(ctx, adapter.ToolCall{ID: "r", Name: "codex_read_file", Arguments: `{"path":"options.go","start_line":1,"end_line":4}`})
	if read.IsError || !strings.Contains(read.Content, "type Option func(*Parser)") {
		t.Fatalf("codex_read_file on the pin = %+v", read)
	}
	res := ex.Execute(ctx, adapter.ToolCall{ID: "f", Name: "record_finding", Arguments: `{"claim":"Option is the functional option type","path":"options.go","start_line":4,"end_line":4}`})
	if res.IsError {
		t.Fatalf("record_finding after codex_read_file refused: %s", res.Content)
	}
	fs := ex.Findings()
	if len(fs) != 1 || fs[0].Excerpt != "type Option func(*Parser)" {
		t.Fatalf("findings = %+v", fs)
	}
	// repo=<the task's repository> reads the same pin.
	read = ex.Execute(ctx, adapter.ToolCall{ID: "r2", Name: "codex_read_file", Arguments: `{"path":"options.go","start_line":1,"end_line":1,"repo":"enmime"}`})
	if read.IsError {
		t.Fatalf("codex_read_file with repo=enmime: %s", read.Content)
	}
}
