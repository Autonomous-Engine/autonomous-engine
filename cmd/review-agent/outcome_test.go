package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/bus"
	"github.com/autonomousengine/autonomous-engine/internal/models"
	"github.com/autonomousengine/autonomous-engine/internal/outbox"
	"github.com/autonomousengine/autonomous-engine/internal/workgraph"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// reviewWriter runs the real workgraph/guards/outbox writers against a fake
// transaction; anything not overridden here (or a pooled write through the
// nil-pool WorkGraph) panics.

type stmt struct {
	sql  string
	args []any
}

type fakeTx struct {
	pgx.Tx
	state      models.WorkItemState
	failOn     string
	stmts      []stmt
	committed  bool
	rolledBack bool
}

func (f *fakeTx) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	f.stmts = append(f.stmts, stmt{sql, args})
	if f.failOn != "" && strings.Contains(sql, f.failOn) {
		return pgconn.CommandTag{}, errors.New("injected failure on " + f.failOn)
	}
	return pgconn.CommandTag{}, nil
}

func (f *fakeTx) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	f.stmts = append(f.stmts, stmt{sql, args})
	return stateRow{f.state}
}

func (f *fakeTx) Commit(context.Context) error   { f.committed = true; return nil }
func (f *fakeTx) Rollback(context.Context) error { f.rolledBack = true; return nil }

type stateRow struct{ state models.WorkItemState }

func (r stateRow) Scan(dest ...any) error {
	*(dest[0].(*models.WorkItemState)) = r.state
	return nil
}

// fakeDB hands out one fakeTx per Begin; state/failOn apply to the first.
type fakeDB struct {
	state  models.WorkItemState
	failOn string
	txs    []*fakeTx
}

func (d *fakeDB) Begin(context.Context) (pgx.Tx, error) {
	tx := &fakeTx{state: models.WorkItemStateReview}
	if len(d.txs) == 0 {
		if d.state != "" {
			tx.state = d.state
		}
		tx.failOn = d.failOn
	}
	d.txs = append(d.txs, tx)
	return tx, nil
}

func tables(tx *fakeTx) []string {
	var out []string
	for _, s := range tx.stmts {
		switch {
		case strings.Contains(s.sql, "INSERT INTO work_item_artifacts"):
			out = append(out, "artifact")
		case strings.Contains(s.sql, "FOR UPDATE"):
			out = append(out, "lock")
		case strings.Contains(s.sql, "UPDATE work_items SET state"):
			out = append(out, "state:"+string(s.args[0].(models.WorkItemState)))
		case strings.Contains(s.sql, "INSERT INTO work_item_transitions"):
			out = append(out, "transition")
		case strings.Contains(s.sql, "INSERT INTO outbox_events"):
			out = append(out, "outbox:"+s.args[3].(string))
		case strings.Contains(s.sql, "UPDATE agent_assignments"):
			out = append(out, "assignment:"+string(s.args[0].(models.AssignmentStatus)))
		default:
			out = append(out, "other:"+strings.TrimSpace(s.sql))
		}
	}
	return out
}

func outboxPayload(t *testing.T, tx *fakeTx, topic string) (args []any, body map[string]any) {
	t.Helper()
	for _, s := range tx.stmts {
		if strings.Contains(s.sql, "INSERT INTO outbox_events") && s.args[3] == topic {
			if err := json.Unmarshal(s.args[5].([]byte), &body); err != nil {
				t.Fatal(err)
			}
			return s.args, body
		}
	}
	t.Fatalf("no outbox row for %s in %v", topic, tables(tx))
	return nil, nil
}

func newArtifact(workID uuid.UUID) *models.WorkItemArtifact {
	return &models.WorkItemArtifact{WorkItemID: workID, Type: models.ArtifactTypeReviewComments, Content: []byte(`{"decision":"approve"}`), AgentID: agentID}
}

func TestReviewWriter_ChangesRequestedCommitsArtifactAndEventTogether(t *testing.T) {
	db := &fakeDB{}
	rw := reviewWriter{db: db, wg: workgraph.New(nil)}
	workID := uuid.New()
	if !rw.recordReview(context.Background(), workID, uuid.New(), newArtifact(workID), reviewDecision{feedback: "fix it"}) {
		t.Fatal("recordReview failed")
	}
	if len(db.txs) != 1 || !db.txs[0].committed {
		t.Fatalf("want one committed tx, got %d", len(db.txs))
	}
	want := []string{"artifact", "outbox:" + bus.TopicReviewComplete, "assignment:" + string(models.AssignmentStatusCompleted)}
	if got := tables(db.txs[0]); !reflect.DeepEqual(got, want) {
		t.Fatalf("statements = %v, want %v", got, want)
	}
	args, body := outboxPayload(t, db.txs[0], bus.TopicReviewComplete)
	if args[1] != outbox.AggregateTypeWorkItem || args[2] != workID || args[4] != workID.String() {
		t.Fatalf("aggregate/key = %v/%v/%v, want work_item/%s/%s", args[1], args[2], args[4], workID, workID)
	}
	delete(body, "event_id")
	wantBody := map[string]any{"work_id": workID.String(), "decision": "changes_requested", "feedback": "fix it", "human_review_required": false}
	if !reflect.DeepEqual(body, wantBody) {
		t.Fatalf("payload = %v, want %v (unchanged from the direct publish)", body, wantBody)
	}
}

func TestReviewWriter_ApprovedWithoutHumanReviewEnqueuesApproval(t *testing.T) {
	db := &fakeDB{}
	rw := reviewWriter{db: db, wg: workgraph.New(nil)}
	workID := uuid.New()
	rw.recordReview(context.Background(), workID, uuid.New(), newArtifact(workID), reviewDecision{approve: true, feedback: "lgtm"})
	_, body := outboxPayload(t, db.txs[0], bus.TopicReviewComplete)
	if body["decision"] != "approved" || body["human_review_required"] != false {
		t.Fatalf("payload = %v, want an approval", body)
	}
	if _, ok := body["feedback"]; ok {
		t.Fatalf("approval payload carries feedback %v; the direct publish did not", body["feedback"])
	}
}

func TestReviewWriter_ApprovedWithHumanReviewTransitionsInTheSameTx(t *testing.T) {
	db := &fakeDB{}
	rw := reviewWriter{db: db, wg: workgraph.New(nil)}
	workID := uuid.New()
	rw.recordReview(context.Background(), workID, uuid.New(), newArtifact(workID), reviewDecision{approve: true, humanReviewRequired: true})
	want := []string{"artifact", "lock", "state:" + string(models.WorkItemStateHumanReview), "transition", "assignment:" + string(models.AssignmentStatusCompleted)}
	if got := tables(db.txs[0]); len(db.txs) != 1 || !db.txs[0].committed || !reflect.DeepEqual(got, want) {
		t.Fatalf("statements = %v, want %v in one committed tx", got, want)
	}
}

func TestReviewWriter_FailedCommitRollsBackAndBlocks(t *testing.T) {
	db := &fakeDB{failOn: "INSERT INTO outbox_events"}
	rw := reviewWriter{db: db, wg: workgraph.New(nil)}
	workID := uuid.New()
	if rw.recordReview(context.Background(), workID, uuid.New(), newArtifact(workID), reviewDecision{feedback: "x"}) {
		t.Fatal("recordReview reported success although the commit failed")
	}
	if len(db.txs) != 2 || db.txs[0].committed || !db.txs[0].rolledBack {
		t.Fatalf("want the review tx rolled back then a block tx, got %d txs", len(db.txs))
	}
	want := []string{"lock", "state:" + string(models.WorkItemStateBlocked), "transition", "outbox:" + bus.TopicWorkBlocked, "assignment:" + string(models.AssignmentStatusFailed)}
	if got := tables(db.txs[1]); !db.txs[1].committed || !reflect.DeepEqual(got, want) {
		t.Fatalf("block tx = %v (committed=%v), want %v", got, db.txs[1].committed, want)
	}
	_, body := outboxPayload(t, db.txs[1], bus.TopicWorkBlocked)
	if r, _ := body["reason"].(string); !strings.Contains(r, "could not be recorded") {
		t.Fatalf("block reason = %q", r)
	}
}

func TestReviewWriter_BlockCommitsTransitionEventAndAssignment(t *testing.T) {
	db := &fakeDB{}
	rw := reviewWriter{db: db, wg: workgraph.New(nil)}
	workID := uuid.New()
	if err := rw.block(context.Background(), workID, uuid.New(), "no repo"); err != nil {
		t.Fatal(err)
	}
	want := []string{"lock", "state:" + string(models.WorkItemStateBlocked), "transition", "outbox:" + bus.TopicWorkBlocked, "assignment:" + string(models.AssignmentStatusFailed)}
	if got := tables(db.txs[0]); len(db.txs) != 1 || !db.txs[0].committed || !reflect.DeepEqual(got, want) {
		t.Fatalf("statements = %v, want %v in one committed tx", got, want)
	}
}

func TestReviewWriter_UnblockableItemStillFailsAssignment(t *testing.T) {
	// released -> blocked is not a legal transition: the block tx rolls
	// back, and the assignment is failed in a transaction of its own.
	db := &fakeDB{state: models.WorkItemStateReleased}
	rw := reviewWriter{db: db, wg: workgraph.New(nil)}
	if err := rw.block(context.Background(), uuid.New(), uuid.New(), "x"); err == nil {
		t.Fatal("block of a released item reported success")
	}
	if len(db.txs) != 2 || db.txs[0].committed || !db.txs[1].committed {
		t.Fatalf("want a rolled-back block tx then a committed assignment tx, got %d", len(db.txs))
	}
	if got := tables(db.txs[1]); !reflect.DeepEqual(got, []string{"assignment:" + string(models.AssignmentStatusFailed)}) {
		t.Fatalf("fallback statements = %v", got)
	}
}

// ── Source scans ────────────────────────────────────────────────────────────

var (
	lineComment  = regexp.MustCompile(`(?m)//.*$`)
	blockComment = regexp.MustCompile(`(?s)/\*.*?\*/`)
	publishCall  = regexp.MustCompile(`\.Publish\(\s*[^,]+,\s*([^,]+),`)
)

func sources(t *testing.T) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
			continue
		}
		b, err := os.ReadFile(filepath.Clean(n))
		if err != nil {
			t.Fatal(err)
		}
		out[n] = lineComment.ReplaceAllString(blockComment.ReplaceAllString(string(b), " "), " ")
	}
	return out
}

func TestReviewAgent_PublishesOnlyTelemetryDirectly(t *testing.T) {
	telemetry := map[string]bool{"bus.TopicToolExecuted": true, "bus.TopicConsoleStream": true}
	seen := 0
	for name, code := range sources(t) {
		for _, m := range publishCall.FindAllStringSubmatch(code, -1) {
			seen++
			if topic := strings.TrimSpace(m[1]); !telemetry[topic] {
				t.Errorf("%s publishes %s directly; only telemetry may bypass the outbox", name, topic)
			}
		}
		for _, needle := range []string{"publishJSON(", ".PublishMessage(", "guards.BlockWorkItem(", ".CompleteAssignment(", ".AddArtifact(", "TransitionState(msgCtx, workID, models.WorkItemStateHumanReview"} {
			if strings.Contains(code, needle) {
				t.Errorf("%s calls %s — review outcomes must commit through reviewWriter", name, needle)
			}
		}
	}
	if seen < 2 {
		t.Errorf("matched %d Publish calls, want the 2 telemetry publishes; the scan pattern is stale", seen)
	}
}

func TestReviewAgent_StartsAndWaitsForOneDispatcher(t *testing.T) {
	src := sources(t)
	starts, waits := 0, 0
	for _, code := range src {
		starts += strings.Count(code, "outbox.Start")
		waits += strings.Count(code, "dispatcher.Wait()")
	}
	if starts != 1 || !strings.Contains(src["main.go"], `outbox.StartForService(ctx, pool, kb, log.Default(), "review-agent")`) {
		t.Errorf("want exactly one outbox.StartForService(..., \"review-agent\"), found %d start call(s)", starts)
	}
	if waits != 1 {
		t.Errorf("found %d dispatcher.Wait() call(s), want 1", waits)
	}
}
