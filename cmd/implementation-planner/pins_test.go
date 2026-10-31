package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// M2-02 source checks: the plan copies each subtask's pin inside the plan
// transaction, after the subtask row exists, and the message carries it.

func TestPlanner_CopiesPinInPlanTransaction(t *testing.T) {
	src := strings.ReplaceAll(stripComments(plannerSources(t)["main.go"]), "\r\n", "\n")
	commit := src[strings.Index(src, "func commitDecomposition("):]
	commit = commit[:strings.Index(commit, "\n}\n")]
	create := strings.Index(commit, "wg.CreateWorkItemTx(ctx, tx, ps.item)")
	cp := strings.Index(commit, "copyPinTx(ctx, tx, parentWorkID, ps)")
	if create < 0 || cp < 0 {
		t.Fatalf("commitDecomposition: CreateWorkItemTx at %d, copyPinTx at %d — both must be present", create, cp)
	}
	if cp < create {
		t.Error("the pin is copied before the subtask row exists (foreign key)")
	}
	if cp > strings.Index(commit, "tx.Commit(ctx)") {
		t.Error("the pin copy is outside the plan transaction")
	}
	// Phase 1 (building each subtask's message) moved to validation.go with
	// the M2-05 repair round.
	phase1 := src + strings.ReplaceAll(stripComments(plannerSources(t)["validation.go"]), "\r\n", "\n")
	for _, want := range []string{"parentPins, pinErr := loadParentPins(msgCtx, pool, workID)", "return fmt.Errorf(\"read base pins for %s: %w\", workID, pinErr)", "taskMsg.BaseSHA = pin.BaseSHA", "taskMsg.BaseRef = pin.BaseRef", "snapshot.NoteUnpinned(agentID, workID,"} {
		if !strings.Contains(phase1, want) {
			t.Errorf("planner does not contain %q", want)
		}
	}
}

type failingQuerier struct{}

func (failingQuerier) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, errors.New("db down")
}
func (failingQuerier) QueryRow(context.Context, string, ...any) pgx.Row { return nil }

// A pin read error is an error, never "no pins": planning a pinned item as
// unpinned would silently drop its snapshot.
func TestLoadParentPins_ReadErrorIsReturned(t *testing.T) {
	pins, err := loadParentPins(context.Background(), failingQuerier{}, uuid.New())
	if err == nil || pins != nil {
		t.Fatalf("pins=%v err=%v, want an error", pins, err)
	}
}

func TestImplementationTaskMessage_BaseFieldsAreAdditive(t *testing.T) {
	b, err := json.Marshal(implementationTaskMessage{TaskID: "t"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "base_sha") || strings.Contains(string(b), "base_ref") {
		t.Fatalf("unpinned message serialises base fields: %s", b)
	}
	b, _ = json.Marshal(implementationTaskMessage{TaskID: "t", BaseSHA: "abc", BaseRef: "main"})
	if !strings.Contains(string(b), `"base_sha":"abc"`) || !strings.Contains(string(b), `"base_ref":"main"`) {
		t.Fatalf("pinned message: %s", b)
	}
}
