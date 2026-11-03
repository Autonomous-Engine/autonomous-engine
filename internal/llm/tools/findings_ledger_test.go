package tools

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/llm/adapter"
)

// The ledger message carries the loop's findings: the model's memory of
// what it learned, with citations, after the reads themselves are gone.
func TestLedgerMessageCarriesFindings(t *testing.T) {
	fs := []CitedFinding{
		{Claim: "Option is the functional option type", Path: "options.go", StartLine: 12, EndLine: 14},
		{Claim: "charsets map is keyed lower-case", Path: "internal/coding/charsets.go", StartLine: 40, EndLine: 41},
		{Claim: "Option is the functional option type", Path: "options.go", StartLine: 12, EndLine: 14}, // duplicate
	}
	got := findingsSection(fs)
	want := findingsHeader + "\n" +
		"- Option is the functional option type (options.go:12-14)\n" +
		"- charsets map is keyed lower-case (internal/coding/charsets.go:40-41)"
	if got != want {
		t.Fatalf("section =\n%s\nwant\n%s", got, want)
	}
	if findingsSection(nil) != "" {
		t.Fatal("no findings must render nothing")
	}
	var many []CitedFinding
	for i := 0; i < 40; i++ {
		many = append(many, CitedFinding{Claim: fmt.Sprintf("finding %02d %s", i, strings.Repeat("y", 90)), Path: "p.go", StartLine: i + 1, EndLine: i + 1})
	}
	capped := findingsSection(many)
	if len(capped) > findingsSectionMax {
		t.Fatalf("section is %d chars, cap %d", len(capped), findingsSectionMax)
	}
	if !strings.Contains(capped, "earlier findings") || !strings.Contains(capped, "finding 39") || strings.Contains(capped, "finding 00") {
		t.Fatalf("the oldest findings must be the ones dropped:\n%s", capped[:200])
	}

	l := newEvidenceLedger()
	l.findings = func() []CitedFinding { return fs[:1] }
	l.add("k", "- codex_read_file options.go 1-40 → 40 lines")
	msg := l.message("")
	if !strings.Contains(msg, ledgerHeader) || !strings.Contains(msg, "\n\n"+findingsHeader+"\n- Option is the functional option type (options.go:12-14)") {
		t.Fatalf("ledger message lacks the findings section:\n%s", msg)
	}
}

func readTurn(turn int) []turnCall {
	c := adapter.ToolCall{ID: fmt.Sprintf("r%d", turn), Name: "codex_read_file", Arguments: `{"path":"a.go","start_line":1,"end_line":5}`}
	return []turnCall{{call: c, fp: "codex_read_file\x1fa.go 1-5\x1fsha", path: "a.go", desc: "codex_read_file a.go 1-5"}}
}

// A read repeated after a finding was recorded is the model coming back
// for more citations from content compaction took away: productive, not
// stuck. Repeats with no finding between them still escalate.
func TestFindingAfterAReadMakesTheRereadProductive(t *testing.T) {
	allowed := map[string]bool{"codex_read_file": true}
	res := func() []adapter.ToolResult {
		return []adapter.ToolResult{{Content: `{"path":"a.go","start_line":1,"end_line":5,"content":"a\nb\nc\nd\ne"}`}}
	}
	r := newReadRepeats()
	for turn := 1; turn <= readRepeatLimit; turn++ {
		prior, stuck := r.check(readTurn(turn), allowed)
		if stuck != nil {
			t.Fatalf("turn %d stuck early: %v", turn, stuck)
		}
		r.annotate(readTurn(turn), prior, res(), turn, false)
	}
	if _, stuck := r.check(readTurn(readRepeatLimit+1), allowed); stuck == nil {
		t.Fatal("control: the repeat past the limit with no finding must be stuck")
	}
	r.productive(readRepeatLimit+1, []CitedFinding{{Path: "a.go", StartLine: 2, EndLine: 3}})
	if _, stuck := r.check(readTurn(readRepeatLimit+2), allowed); stuck != nil {
		t.Fatalf("after a finding the read is productive, got stuck: %v", stuck)
	}
}

// rereadingAdapter reads a.go 1-5 three times; with findings between the
// reads (record) it is a model citing one range in several claims.
type rereadingAdapter struct {
	n      int
	record bool
}

func (a *rereadingAdapter) Complete(context.Context, adapter.CompletionRequest) (*adapter.CompletionResponse, error) {
	a.n++
	read := adapter.ToolCall{ID: fmt.Sprintf("c%d", a.n), Name: "read_file", Arguments: `{"path":"a.go","start_line":1,"end_line":5}`}
	finding := adapter.ToolCall{ID: fmt.Sprintf("c%d", a.n), Name: "record_finding", Arguments: fmt.Sprintf(`{"claim":"claim %d","path":"a.go","start_line":%d,"end_line":%d}`, a.n, a.n/2, a.n/2)}
	resp := &adapter.CompletionResponse{FinishReason: "tool_calls", InputTokens: 10, OutputTokens: 5}
	switch {
	case a.record && a.n <= 8:
		if a.n%2 == 1 {
			resp.ToolCalls = []adapter.ToolCall{read}
		} else {
			resp.ToolCalls = []adapter.ToolCall{finding}
		}
	case !a.record && a.n <= 4:
		resp.ToolCalls = []adapter.ToolCall{read}
	default:
		return &adapter.CompletionResponse{Content: "done", FinishReason: "stop", InputTokens: 10, OutputTokens: 5}, nil
	}
	return resp, nil
}
func (a *rereadingAdapter) Stream(context.Context, adapter.CompletionRequest) (<-chan adapter.StreamChunk, error) {
	return nil, errors.New("not used")
}
func (a *rereadingAdapter) Capabilities() adapter.ModelCapabilities {
	return adapter.ModelCapabilities{MaxContextTokens: 100000, SupportsFunctions: true}
}
func (a *rereadingAdapter) HealthCheck(context.Context) error { return nil }
func (a *rereadingAdapter) Name() string                      { return "reread/test" }

func TestLoopDoesNotEndStuckWhenFindingsSeparateRereads(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte("l1\nl2\nl3\nl4\nl5\nl6\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run := func(record bool) (*adapter.CompletionResponse, error, *Executor) {
		ex := NewExecutor(dir)
		req := adapter.CompletionRequest{Messages: []adapter.ChatMessage{{Role: adapter.RoleUser, Content: "go"}}, Tools: []adapter.Tool{ReadFile(), RecordFinding()}}
		resp, err := RunToolLoopWithOptions(context.Background(), &rereadingAdapter{record: record}, req, ex, 20, nil, nil,
			LoopOptions{Findings: true, EvidenceLedger: true, KeepRecent: 1, ReadOnly: true, StuckControl: true})
		return resp, err, ex
	}
	resp, err, ex := run(true)
	if err != nil || resp == nil || resp.Content != "done" {
		t.Fatalf("with findings: resp=%+v err=%v", resp, err)
	}
	if n := len(ex.Findings()); n != 4 {
		t.Fatalf("findings = %d, want 4", n)
	}
	var stuck *LoopStuck
	if _, err, _ = run(false); !errors.As(err, &stuck) || stuck.Kind != StuckKindRepeatedRead {
		t.Fatalf("control without findings: err = %v, want LoopStuck repeated_read", err)
	}
}
