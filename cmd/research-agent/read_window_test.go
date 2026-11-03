package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/llm/adapter"
	"github.com/google/uuid"
)

// Research reads in 120-line windows (240 with an explicit range): a
// window the model can cite in one finding, at a third of the tokens.
func TestResearchExecutorReadsInSmallWindows(t *testing.T) {
	dir := t.TempDir()
	var b strings.Builder
	for i := 1; i <= 300; i++ {
		fmt.Fprintf(&b, "line %03d\n", i)
	}
	if err := os.WriteFile(filepath.Join(dir, "big.go"), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	workID := uuid.New()
	ex := newResearchExecutor(dir, "r", nil, nil, nil, nil, &workID, "t", "code_trace")
	res := ex.Execute(context.Background(), adapter.ToolCall{ID: "r", Name: "codex_read_file", Arguments: `{"path":"big.go","start_line":100}`})
	if res.IsError || !strings.Contains(res.Content, "line 219") || strings.Contains(res.Content, "line 220") {
		t.Fatalf("research read from 100 = %+v, want lines 100-219", res)
	}
}

// A research task is capped at ten iterations (the scaffold's 30 let
// pattern_match explore for 25 and return nothing); with findings recorded
// the loop closes with an answer from them.
func TestResearchIterationCapIsTen(t *testing.T) {
	if got := researchMaxIterations(30); got != 10 {
		t.Fatalf("cap(30) = %d, want 10", got)
	}
	if got := researchMaxIterations(8); got != 8 {
		t.Fatalf("cap(8) = %d, want the smaller scaffold value", got)
	}
}
