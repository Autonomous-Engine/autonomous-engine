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
	"github.com/autonomousengine/autonomous-engine/internal/evidence"
	"github.com/autonomousengine/autonomous-engine/internal/models"
	"github.com/autonomousengine/autonomous-engine/internal/outbox"
	"github.com/autonomousengine/autonomous-engine/internal/workgraph"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// qaWriter runs the real workgraph/guards/outbox writers against a fake
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

type fakeDB struct {
	failOn string
	txs    []*fakeTx
}

func (d *fakeDB) Begin(context.Context) (pgx.Tx, error) {
	tx := &fakeTx{state: models.WorkItemStateQA}
	if len(d.txs) == 0 {
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

func qaArtifactFor(workID uuid.UUID) *models.WorkItemArtifact {
	return &models.WorkItemArtifact{WorkItemID: workID, Type: models.ArtifactTypeQAReport, Content: []byte(`{"all_pass":true}`), AgentID: agentID}
}

func TestQAWriter_PassCommitsArtifactResultAndAssignmentTogether(t *testing.T) {
	db := &fakeDB{}
	qw := qaWriter{db: db, wg: workgraph.New(nil)}
	workID := uuid.New()
	res := &qaResultMessage{WorkID: workID.String(), Result: "pass", Report: `{"all_pass":true}`}
	if !qw.recordResult(context.Background(), workID, uuid.New(), qaArtifactFor(workID), res, nil) {
		t.Fatal("recordResult failed")
	}
	want := []string{"artifact", "outbox:" + bus.TopicQAResult, "assignment:" + string(models.AssignmentStatusCompleted)}
	if got := tables(db.txs[0]); len(db.txs) != 1 || !db.txs[0].committed || !reflect.DeepEqual(got, want) {
		t.Fatalf("statements = %v, want %v in one committed tx", got, want)
	}
	args, body := outboxPayload(t, db.txs[0], bus.TopicQAResult)
	if args[1] != outbox.AggregateTypeWorkItem || args[2] != workID || args[4] != workID.String() {
		t.Fatalf("aggregate/key = %v/%v/%v", args[1], args[2], args[4])
	}
	delete(body, "event_id")
	if want := map[string]any{"work_id": workID.String(), "result": "pass", "report": `{"all_pass":true}`}; !reflect.DeepEqual(body, want) {
		t.Fatalf("payload = %v, want %v (unchanged from the direct publish)", body, want)
	}
}

func TestQAWriter_FailCommitsArtifactBugsAndAssignmentTogether(t *testing.T) {
	db := &fakeDB{}
	qw := qaWriter{db: db, wg: workgraph.New(nil)}
	workID := uuid.New()
	bugs := &qaBugsMessage{WorkID: workID.String(), Bugs: "FAIL: x", ReproductionSteps: "1. Test: x"}
	qw.recordResult(context.Background(), workID, uuid.New(), qaArtifactFor(workID), nil, bugs)
	want := []string{"artifact", "outbox:" + bus.TopicQABugs, "assignment:" + string(models.AssignmentStatusCompleted)}
	if got := tables(db.txs[0]); !db.txs[0].committed || !reflect.DeepEqual(got, want) {
		t.Fatalf("statements = %v, want %v", got, want)
	}
	_, body := outboxPayload(t, db.txs[0], bus.TopicQABugs)
	delete(body, "event_id")
	if want := map[string]any{"work_id": workID.String(), "bugs": "FAIL: x", "reproduction_steps": "1. Test: x"}; !reflect.DeepEqual(body, want) {
		t.Fatalf("payload = %v, want %v", body, want)
	}
}

func TestQAWriter_FailedCommitRollsBackAndBlocks(t *testing.T) {
	db := &fakeDB{failOn: "INSERT INTO work_item_artifacts"}
	qw := qaWriter{db: db, wg: workgraph.New(nil)}
	workID := uuid.New()
	if qw.recordResult(context.Background(), workID, uuid.New(), qaArtifactFor(workID), &qaResultMessage{WorkID: workID.String(), Result: "pass"}, nil) {
		t.Fatal("recordResult reported success although the commit failed")
	}
	if len(db.txs) != 2 || db.txs[0].committed || !db.txs[0].rolledBack {
		t.Fatalf("want the QA tx rolled back then a block tx, got %d txs", len(db.txs))
	}
	if got := tables(db.txs[0]); !reflect.DeepEqual(got, []string{"artifact"}) {
		t.Fatalf("QA tx ran %v after the failed artifact insert", got)
	}
	want := []string{"lock", "state:" + string(models.WorkItemStateBlocked), "transition", "outbox:" + bus.TopicWorkBlocked, "assignment:" + string(models.AssignmentStatusFailed)}
	if got := tables(db.txs[1]); !db.txs[1].committed || !reflect.DeepEqual(got, want) {
		t.Fatalf("block tx = %v, want %v", got, want)
	}
}

func TestQAWriter_RejectsAnOutcomeWithoutAVerdict(t *testing.T) {
	db := &fakeDB{}
	qw := qaWriter{db: db, wg: workgraph.New(nil)}
	workID := uuid.New()
	if err := qw.commitResult(context.Background(), workID, uuid.New(), qaArtifactFor(workID), nil, nil); err == nil {
		t.Fatal("commitResult accepted an outcome with neither qa.result nor qa.bugs")
	}
	if db.txs[0].committed {
		t.Fatal("an outcome without a verdict committed")
	}
}

// TestQAWriter_CheckoutBlockReasonReachesWorkBlocked drives the existing
// checkout seam: a moved branch's block reason is carried verbatim into the
// work.blocked event committed with the transition.
func TestQAWriter_CheckoutBlockReasonReachesWorkBlocked(t *testing.T) {
	f := newRepoFixture(t)
	verified := f.head(t)
	f.push(t, "c.txt")
	res := checkout(f, f.branch, verified, evidence.CompatWindowStrict)
	if res.Cleanup != nil {
		res.Cleanup()
	}
	if res.BlockReason == "" {
		t.Fatal("moved branch did not block the checkout")
	}

	db := &fakeDB{}
	qw := qaWriter{db: db, wg: workgraph.New(nil)}
	workID := uuid.New()
	if err := qw.block(context.Background(), workID, uuid.New(), res.BlockReason); err != nil {
		t.Fatal(err)
	}
	want := []string{"lock", "state:" + string(models.WorkItemStateBlocked), "transition", "outbox:" + bus.TopicWorkBlocked, "assignment:" + string(models.AssignmentStatusFailed)}
	if got := tables(db.txs[0]); len(db.txs) != 1 || !db.txs[0].committed || !reflect.DeepEqual(got, want) {
		t.Fatalf("statements = %v, want %v in one committed tx", got, want)
	}
	_, body := outboxPayload(t, db.txs[0], bus.TopicWorkBlocked)
	if body["reason"] != res.BlockReason || body["work_id"] != workID.String() {
		t.Fatalf("work.blocked = %v, want reason %q", body, res.BlockReason)
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

func TestQAAgent_PublishesOnlyTelemetryDirectly(t *testing.T) {
	telemetry := map[string]bool{"bus.TopicToolExecuted": true, "bus.TopicConsoleStream": true}
	seen := 0
	for name, code := range sources(t) {
		for _, m := range publishCall.FindAllStringSubmatch(code, -1) {
			seen++
			if topic := strings.TrimSpace(m[1]); !telemetry[topic] {
				t.Errorf("%s publishes %s directly; only telemetry may bypass the outbox", name, topic)
			}
		}
		for _, needle := range []string{"publishJSON(", ".PublishMessage(", "guards.BlockWorkItem(", ".CompleteAssignment(", ".AddArtifact("} {
			if strings.Contains(code, needle) {
				t.Errorf("%s calls %s — QA outcomes must commit through qaWriter", name, needle)
			}
		}
	}
	if seen < 2 {
		t.Errorf("matched %d Publish calls, want the 2 telemetry publishes; the scan pattern is stale", seen)
	}
}

func TestQAAgent_StartsAndWaitsForOneDispatcher(t *testing.T) {
	src := sources(t)
	starts, waits := 0, 0
	for _, code := range src {
		starts += strings.Count(code, "outbox.Start")
		waits += strings.Count(code, "dispatcher.Wait()")
	}
	if starts != 1 || !strings.Contains(src["main.go"], `outbox.StartForService(ctx, pool, kb, log.Default(), "qa-agent")`) {
		t.Errorf("want exactly one outbox.StartForService(..., \"qa-agent\"), found %d start call(s)", starts)
	}
	if waits != 1 {
		t.Errorf("found %d dispatcher.Wait() call(s), want 1", waits)
	}
}
