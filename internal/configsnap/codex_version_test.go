package configsnap

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// fakeRow answers one QueryRow: raw JSON for []byte targets, a number for
// int64 targets, or pgx.ErrNoRows.
type fakeRow struct {
	raw   []byte
	num   int64
	noRow bool
}

func (r fakeRow) Scan(dest ...any) error {
	if r.noRow {
		return pgx.ErrNoRows
	}
	switch d := dest[0].(type) {
	case *[]byte:
		*d = r.raw
	case *int64:
		*d = r.num
	}
	return nil
}

// fakeQuerier is a database with no definition or providers, two prompt
// templates and a work item at codexVersion.
type fakeQuerier struct{ codexVersion int64 }

func (f fakeQuerier) QueryRow(_ context.Context, sql string, _ ...any) pgx.Row {
	switch sql {
	case promptTemplatesSQL:
		return fakeRow{raw: []byte(`[{"intent_type":"a","agent_type":"","provider":"","model_id":"","operating_mode":"","prompt_type":"system","content":"A"}]`)}
	case `SELECT codex_version FROM work_items WHERE id = $1`:
		return fakeRow{num: f.codexVersion}
	}
	return fakeRow{noRow: true}
}

// Migration 060 gives work items real, per-product codex versions (the
// pinned product_version). The codex an item ran against is not part of
// the configuration: two captures identical except for the work item's
// codex version are the same snapshot, so production outcomes of one
// configuration are not split by codex version.
func TestCapture_CodexVersionIsNotPartOfTheSnapshotID(t *testing.T) {
	in := Input{WorkItemID: uuid.New(), AgentType: "implementation-agent", Model: Model{Provider: "ollama", ModelID: "m", Source: SourceGlobalDefault}}
	a, err := Capture(context.Background(), fakeQuerier{codexVersion: 3}, in)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Capture(context.Background(), fakeQuerier{codexVersion: 9}, in)
	if err != nil {
		t.Fatal(err)
	}
	ida, ca, _ := Hash(a)
	idb, cb, _ := Hash(b)
	if ida != idb {
		t.Fatalf("codex version changed the snapshot id:\n%s\n%s", ca, cb)
	}
	// The same as a capture with no work item at all (the pre-060 value
	// for every parent item), so existing ids stay stable.
	none, err := Capture(context.Background(), fakeQuerier{}, Input{AgentType: in.AgentType, Model: in.Model})
	if err != nil {
		t.Fatal(err)
	}
	if idn, _, _ := Hash(none); idn != ida {
		t.Fatal("a capture for a work item differs from one without")
	}
}
