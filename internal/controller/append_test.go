package controller

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
)

type mergingRecorder struct {
	memRecorder
	merged []string
}

func (m *mergingRecorder) MergeDetail(_ context.Context, _ uuid.UUID, patch json.RawMessage) error {
	m.merged = append(m.merged, string(patch))
	return nil
}

// M3-06: Append sequences agent-produced events without deriving a state
// from them; MergeDetail reaches recorders that support it.
func TestRunAppendAndMergeDetail(t *testing.T) {
	rec := &mergingRecorder{}
	r := newTestRun(rec)
	r.SetTestCommand("go test ./...")
	exit := 1
	seq := r.Append(context.Background(), EventRow{Kind: KindTool, Tool: "pre_commit_checks", Command: "go test ./...", ExitCode: &exit, Detail: json.RawMessage(`{"records":[]}`)})
	if seq != 2 || r.State() != StateInspect {
		t.Fatalf("seq=%d state=%s: Append must take the next seq and never move the state", seq, r.State())
	}
	if next := r.Append(context.Background(), EventRow{Kind: KindNote, Note: "plan_revision: x"}); next != 3 {
		t.Fatalf("next seq = %d", next)
	}
	if len(rec.rows) != 2 || string(rec.rows[0].Detail) != `{"records":[]}` {
		t.Fatalf("rows = %+v", rec.rows)
	}
	r.MergeDetail(context.Background(), "budget", map[string]int{"tokens_used": 5})
	if len(rec.merged) != 1 || rec.merged[0] != `{"budget":{"tokens_used":5}}` {
		t.Fatalf("merged = %v", rec.merged)
	}
	var nilRun *Run
	if nilRun.Append(context.Background(), EventRow{Kind: KindNote}) != 0 {
		t.Fatal("nil run Append must be a no-op")
	}
	nilRun.MergeDetail(context.Background(), "k", 1)
	newTestRun(&memRecorder{}).MergeDetail(context.Background(), "k", 1) // no merger: dropped
}
