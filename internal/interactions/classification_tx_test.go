package interactions

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
)

func TestApplyClassificationTx_RunsTheSharedUpdateInTheCallersTx(t *testing.T) {
	tx := &recordingTx{}
	id := uuid.New()
	entry := uuid.New()
	r := ClassificationResult{
		ClassifiedAs: ClassifiedCodexPattern, Confidence: 0.9,
		CodexEntryID: &entry, CodexEntryTable: "codex_patterns", QdrantPointID: "p-1",
	}
	if err := NewStore(nil).ApplyClassificationTx(context.Background(), tx, id, r); err != nil {
		t.Fatalf("ApplyClassificationTx: %v", err)
	}
	if len(tx.sqls) != 1 || tx.sqls[0] != applyClassificationSQL {
		t.Fatalf("statements = %q, want exactly applyClassificationSQL", tx.sqls)
	}
	a := tx.args[0]
	if a[0] != id || a[1] != r.ClassifiedAs || a[2] != r.Confidence || a[3] != r.CodexEntryID || a[5] != "p-1" {
		t.Errorf("args = %v", a)
	}
	if a[4] != "codex_patterns" {
		t.Errorf("codex_entry_table arg = %v", a[4])
	}

	if err := NewStore(nil).ApplyClassificationTx(context.Background(), nil, id, r); err == nil {
		t.Error("nil tx accepted")
	}

	failing := &recordingTx{failAt: 1, execErr: errors.New("boom")}
	if err := NewStore(nil).ApplyClassificationTx(context.Background(), failing, id, r); err == nil {
		t.Error("exec error swallowed")
	}
}
