package main

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/bus"
	"github.com/autonomousengine/autonomous-engine/internal/evidence"
	"github.com/autonomousengine/autonomous-engine/internal/interactions"
	"github.com/autonomousengine/autonomous-engine/internal/logging"
	"github.com/autonomousengine/autonomous-engine/internal/models"
	"github.com/autonomousengine/autonomous-engine/internal/outbox"
	"github.com/autonomousengine/autonomous-engine/internal/workgraph"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// These tests run the production outboxCompletionSink — the real
// workgraph, guards, evidence, interactions and outbox writers — against a
// fake transaction, so they pin exactly which statements one outcome
// commits together. Anything not overridden on fakeTx (or a pooled write
// through the nil-pool WorkGraph / evidence store) panics, and the prompt
// publisher has a nil bus, so a direct publish cannot hide in here.

type stmt struct {
	sql  string
	args []any
}

type fakeTx struct {
	pgx.Tx
	state      models.WorkItemState
	failOn     string // Exec of a statement containing this fails
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

// fakeDB hands out one fakeTx per Begin. failOn applies to the first
// transaction only, so a fallback can be observed committing; thenFailOn
// applies to the transactions after it, one entry each.
type fakeDB struct {
	txs        []*fakeTx
	failOn     string
	thenFailOn []string
}

func (d *fakeDB) Begin(context.Context) (pgx.Tx, error) {
	tx := &fakeTx{state: models.WorkItemStateImplementation}
	if n := len(d.txs); n == 0 {
		tx.failOn = d.failOn
	} else if n-1 < len(d.thenFailOn) {
		tx.failOn = d.thenFailOn[n-1]
	}
	d.txs = append(d.txs, tx)
	return tx, nil
}

// txSink is the production sink with StoreOutput replaced (raw outputs are
// pooled artifact writes, deliberately outside the outcome transaction).
type txSink struct {
	*outboxCompletionSink
	outputs recordingSink
}

func (s *txSink) StoreOutput(name string, content []byte) (string, error) {
	return s.outputs.StoreOutput(name, content)
}

func newTxSink(db *fakeDB, in finalizeInputs) *txSink {
	return &txSink{outboxCompletionSink: &outboxCompletionSink{
		ctx:          context.Background(),
		db:           db,
		wg:           workgraph.New(nil),
		prompts:      interactions.NewPublisher(nil),
		workID:       uuid.MustParse(in.task.WorkID),
		assignmentID: uuid.New(),
		taskID:       in.task.TaskID,
		logger:       logging.New("implementation-agent-test"),
	}}
}

// tables names the table each statement writes (or locks), in order.
func tables(tx *fakeTx) []string {
	var out []string
	for _, s := range tx.stmts {
		switch {
		case strings.Contains(s.sql, "INSERT INTO implementation_verifications"):
			out = append(out, "evidence")
		case strings.Contains(s.sql, "INSERT INTO user_interactions"):
			out = append(out, "prompt")
		case strings.Contains(s.sql, "FOR UPDATE"):
			out = append(out, "lock")
		case strings.Contains(s.sql, "UPDATE work_items SET state"):
			out = append(out, "state")
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

// outboxRow returns the outbox insert for topic in tx.
func outboxRow(t *testing.T, tx *fakeTx, topic string) []any {
	t.Helper()
	for _, s := range tx.stmts {
		if strings.Contains(s.sql, "INSERT INTO outbox_events") && s.args[3] == topic {
			return s.args
		}
	}
	t.Fatalf("no outbox row for %s in %v", topic, tables(tx))
	return nil
}

func decodeComplete(t *testing.T, args []any) implementationCompleteMessage {
	t.Helper()
	var msg implementationCompleteMessage
	if err := json.Unmarshal(args[5].([]byte), &msg); err != nil {
		t.Fatalf("decode implementation.complete payload: %v", err)
	}
	return msg
}

func TestFinalizeTx_SuccessCommitsEvidenceAndEventTogether(t *testing.T) {
	requireSh(t)
	f := newFinalizeFixture(t)
	head := f.commit(t, "feature.txt", "value=2\n", "feat: initial")
	in := f.inputs()
	in.evidenceStore = evidence.NewStore(nil) // nil pool: only InsertTx can work
	db := &fakeDB{}
	sink := newTxSink(db, in)

	if !finalizeImplementation(context.Background(), in, sink) {
		t.Fatal("finalize failed on a passing, pushable head")
	}
	if len(db.txs) != 1 {
		t.Fatalf("began %d transactions, want 1", len(db.txs))
	}
	tx := db.txs[0]
	if !tx.committed || tx.rolledBack {
		t.Fatalf("committed=%v rolledBack=%v, want one clean commit", tx.committed, tx.rolledBack)
	}
	want := []string{"evidence", "outbox:" + bus.TopicImplementationComplete, "assignment:" + string(models.AssignmentStatusCompleted)}
	if got := tables(tx); !reflect.DeepEqual(got, want) {
		t.Fatalf("statements = %v, want %v", got, want)
	}
	row := outboxRow(t, tx, bus.TopicImplementationComplete)
	if row[1] != outbox.AggregateTypeWorkItem || row[2] != uuid.MustParse(in.task.WorkID) || row[4] != in.task.TaskID {
		t.Fatalf("aggregate/key = %v/%v/%v, want work_item/<parent work id>/<task id>", row[1], row[2], row[4])
	}
	msg := decodeComplete(t, row)
	if !msg.Success || !msg.Verified || msg.HeadSHA != head || f.remoteSHA(t) != head {
		t.Fatalf("enqueued %+v (remote %s), want verified success at the pushed head %s", msg, f.remoteSHA(t), head)
	}
	if msg.Evidence == nil || msg.Evidence.Outcome != evidence.OutcomeVerified || msg.Evidence.HeadSHA != head {
		t.Fatalf("enqueued evidence = %+v, want verified at %s", msg.Evidence, head)
	}
	// The evidence row is the record the message carries.
	evArgs := tx.stmts[0].args
	found := false
	for _, a := range evArgs {
		if a == head {
			found = true
		}
	}
	if !found {
		t.Fatalf("evidence insert args %v do not carry head %s", evArgs, head)
	}
}

func TestFinalizeTx_FailureCommitsEvidenceBlockAndEventTogether(t *testing.T) {
	requireSh(t)
	f := newFinalizeFixture(t)
	broken := f.commit(t, "feature.txt", "value=3\n", "feat: wrong")
	in := f.inputs()
	in.evidenceStore = evidence.NewStore(nil)
	db := &fakeDB{}
	sink := newTxSink(db, in)

	if finalizeImplementation(context.Background(), in, sink) {
		t.Fatal("finalize succeeded on failing tests")
	}
	if len(db.txs) != 1 || !db.txs[0].committed {
		t.Fatalf("transactions = %d (committed=%v), want one committed", len(db.txs), len(db.txs) > 0 && db.txs[0].committed)
	}
	tx := db.txs[0]
	want := []string{"evidence", "lock", "state", "transition", "outbox:" + bus.TopicWorkBlocked,
		"outbox:" + bus.TopicImplementationComplete, "assignment:" + string(models.AssignmentStatusFailed)}
	if got := tables(tx); !reflect.DeepEqual(got, want) {
		t.Fatalf("statements = %v, want %v", got, want)
	}
	msg := decodeComplete(t, outboxRow(t, tx, bus.TopicImplementationComplete))
	if msg.Success || msg.HeadSHA != broken || msg.Evidence == nil || msg.Evidence.Outcome != evidence.OutcomeFailed {
		t.Fatalf("enqueued %+v, want a failed outcome at %s", msg, broken)
	}
	blocked := outboxRow(t, tx, bus.TopicWorkBlocked)
	if blocked[2] != uuid.MustParse(in.task.WorkID) || blocked[4] != in.task.WorkID {
		t.Fatalf("work.blocked aggregate/key = %v/%v, want the parent work item", blocked[2], blocked[4])
	}
	if f.remoteSHA(t) != "" {
		t.Fatal("a failing head was pushed")
	}
}

func TestFinalizeTx_EvidenceInsertFailureRollsBackTheSuccessEvent(t *testing.T) {
	requireSh(t)
	f := newFinalizeFixture(t)
	head := f.commit(t, "feature.txt", "value=2\n", "feat: initial")
	in := f.inputs()
	in.evidenceStore = evidence.NewStore(nil)
	db := &fakeDB{failOn: "implementation_verifications"}
	sink := newTxSink(db, in)

	if finalizeImplementation(context.Background(), in, sink) {
		t.Fatal("finalize reported success although its outcome rolled back")
	}
	if len(db.txs) != 2 {
		t.Fatalf("began %d transactions, want the success then the fallback", len(db.txs))
	}
	first, second := db.txs[0], db.txs[1]
	if first.committed || !first.rolledBack {
		t.Fatalf("success tx committed=%v rolledBack=%v, want rolled back", first.committed, first.rolledBack)
	}
	if got := tables(first); !reflect.DeepEqual(got, []string{"evidence"}) {
		t.Fatalf("success tx ran %v after the failed evidence insert, want nothing", got)
	}
	want := []string{"lock", "state", "transition", "outbox:" + bus.TopicWorkBlocked,
		"outbox:" + bus.TopicImplementationComplete, "assignment:" + string(models.AssignmentStatusFailed)}
	if got := tables(second); !second.committed || !reflect.DeepEqual(got, want) {
		t.Fatalf("fallback committed=%v statements=%v, want %v", second.committed, got, want)
	}
	msg := decodeComplete(t, outboxRow(t, second, bus.TopicImplementationComplete))
	if msg.Success || msg.Verified || !strings.Contains(msg.Reason, head) {
		t.Fatalf("fallback event = %+v, want a failure naming %s", msg, head)
	}
	if msg.Evidence == nil || msg.Evidence.Outcome != evidence.OutcomeVerified || msg.Evidence.HeadSHA != head {
		t.Fatalf("fallback evidence = %+v, want the unchanged verified record", msg.Evidence)
	}
}

// A failure outcome whose evidence insert fails must still block the
// parent: tier 1 drops only the evidence row.
func TestFinalizeTx_FailureEvidenceInsertFailureKeepsBlockAndEvent(t *testing.T) {
	requireSh(t)
	f := newFinalizeFixture(t)
	broken := f.commit(t, "feature.txt", "value=3\n", "feat: wrong")
	in := f.inputs()
	in.evidenceStore = evidence.NewStore(nil)
	db := &fakeDB{failOn: "implementation_verifications"}
	sink := newTxSink(db, in)

	if finalizeImplementation(context.Background(), in, sink) {
		t.Fatal("finalize succeeded on failing tests")
	}
	if len(db.txs) != 2 || db.txs[0].committed || !db.txs[0].rolledBack {
		t.Fatalf("want the failure tx rolled back then tier 1, got %d txs", len(db.txs))
	}
	want := []string{"lock", "state", "transition", "outbox:" + bus.TopicWorkBlocked,
		"outbox:" + bus.TopicImplementationComplete, "assignment:" + string(models.AssignmentStatusFailed)}
	if got := tables(db.txs[1]); !db.txs[1].committed || !reflect.DeepEqual(got, want) {
		t.Fatalf("tier 1 committed=%v statements=%v, want %v", db.txs[1].committed, got, want)
	}
	if blocked := outboxRow(t, db.txs[1], bus.TopicWorkBlocked); blocked[2] != uuid.MustParse(in.task.WorkID) {
		t.Fatalf("work.blocked aggregate = %v, want the parent", blocked[2])
	}
	msg := decodeComplete(t, outboxRow(t, db.txs[1], bus.TopicImplementationComplete))
	if msg.Success || msg.HeadSHA != broken || msg.Evidence == nil || msg.Evidence.Outcome != evidence.OutcomeFailed {
		t.Fatalf("tier 1 event = %+v, want the unchanged failure at %s", msg, broken)
	}
}

// When tier 1 fails too (the block itself cannot be written), tier 2
// commits the event and the assignment without the block — on the success
// path as well, so implementation.complete is never lost to a bad block.
func TestFinalizeTx_SuccessFallbackTier2DropsAFailingBlock(t *testing.T) {
	requireSh(t)
	f := newFinalizeFixture(t)
	head := f.commit(t, "feature.txt", "value=2\n", "feat: initial")
	in := f.inputs()
	in.evidenceStore = evidence.NewStore(nil)
	db := &fakeDB{failOn: "implementation_verifications", thenFailOn: []string{"UPDATE work_items SET state"}}
	sink := newTxSink(db, in)

	if finalizeImplementation(context.Background(), in, sink) {
		t.Fatal("finalize reported success although its outcome rolled back")
	}
	if len(db.txs) != 3 || db.txs[0].committed || db.txs[1].committed || !db.txs[2].committed {
		t.Fatalf("want success and tier 1 rolled back, tier 2 committed; got %d txs", len(db.txs))
	}
	want := []string{"outbox:" + bus.TopicImplementationComplete, "assignment:" + string(models.AssignmentStatusFailed)}
	if got := tables(db.txs[2]); !reflect.DeepEqual(got, want) {
		t.Fatalf("tier 2 statements = %v, want %v", got, want)
	}
	msg := decodeComplete(t, outboxRow(t, db.txs[2], bus.TopicImplementationComplete))
	if msg.Success || msg.Verified || !strings.Contains(msg.Reason, head) || msg.Evidence == nil || msg.Evidence.HeadSHA != head {
		t.Fatalf("tier 2 event = %+v, want a failure naming %s carrying the evidence", msg, head)
	}
}

func TestReportTx_PromptCommitsWithBlockAndEvent(t *testing.T) {
	db := &fakeDB{}
	workID, productID := uuid.New(), uuid.New()
	sink := newTxSink(db, finalizeInputs{task: implementationTaskMessage{TaskID: uuid.NewString(), WorkID: workID.String()}})
	prompt := escalationPrompt(&interactions.Prompt{
		ID: uuid.New(), WorkItemID: workID, AskingAgent: agentID,
		Trigger: interactions.TriggerPreflightBrokenBuild, PromptType: interactions.PromptTypeEscalation,
		Headline: "broken", State: interactions.StatePending,
	}, &productID)
	if prompt == nil || prompt.ProductID != productID {
		t.Fatalf("escalationPrompt = %+v, want the prompt filed under %s", prompt, productID)
	}
	if !reportOutcome(sink, completionOutcome{
		prompt:      prompt,
		blockReason: "repo_env_broken: x",
		msg:         &implementationCompleteMessage{TaskID: sink.taskID, WorkID: workID.String(), Reason: "broken"},
		assignment:  models.AssignmentStatusFailed,
	}) {
		t.Fatal("reportOutcome failed")
	}
	want := []string{"prompt", "outbox:" + bus.TopicUserPromptRequested, "lock", "state", "transition",
		"outbox:" + bus.TopicWorkBlocked, "outbox:" + bus.TopicImplementationComplete, "assignment:" + string(models.AssignmentStatusFailed)}
	if got := tables(db.txs[0]); len(db.txs) != 1 || !db.txs[0].committed || !reflect.DeepEqual(got, want) {
		t.Fatalf("statements = %v, want %v in one committed tx", got, want)
	}
	if row := outboxRow(t, db.txs[0], bus.TopicUserPromptRequested); row[2] != workID {
		t.Fatalf("prompt aggregate = %v, want %s", row[2], workID)
	}
}

func TestEscalationPrompt_NoProductRaisesNoPrompt(t *testing.T) {
	p := &interactions.Prompt{WorkItemID: uuid.New(), Headline: "broken"}
	if got := escalationPrompt(p, nil); got != nil {
		t.Fatalf("escalationPrompt without a product = %+v, want nil (product_id is a NOT NULL foreign key)", got)
	}
	nilID := uuid.Nil
	if got := escalationPrompt(p, &nilID); got != nil {
		t.Fatal("escalationPrompt with the nil product id must not raise a prompt")
	}
}

func TestReportTx_BlockOnlyOutcome(t *testing.T) {
	db := &fakeDB{}
	sink := newTxSink(db, finalizeInputs{task: implementationTaskMessage{TaskID: uuid.NewString(), WorkID: uuid.NewString()}})
	if !reportOutcome(sink, completionOutcome{blockReason: "no repo", assignment: models.AssignmentStatusFailed}) {
		t.Fatal("reportOutcome failed")
	}
	want := []string{"lock", "state", "transition", "outbox:" + bus.TopicWorkBlocked, "assignment:" + string(models.AssignmentStatusFailed)}
	if got := tables(db.txs[0]); !reflect.DeepEqual(got, want) {
		t.Fatalf("statements = %v, want %v", got, want)
	}
}
