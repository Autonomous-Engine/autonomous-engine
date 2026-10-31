package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/bus"
	"github.com/autonomousengine/autonomous-engine/internal/interactions"
	"github.com/autonomousengine/autonomous-engine/internal/models"
	"github.com/autonomousengine/autonomous-engine/internal/outbox"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// ─── fakes ───────────────────────────────────────────────────────────────────

// fakeTx records the statements run on it and how it ended. Methods the
// commit helpers never call fall through to the nil embedded interface and
// panic, so an unexpected call fails the test.
type fakeTx struct {
	pgx.Tx
	sqls      []string
	args      [][]any
	failOn    string // Exec fails when the SQL contains this
	commits   int
	rollbacks int
}

func (f *fakeTx) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	f.sqls = append(f.sqls, sql)
	f.args = append(f.args, args)
	if f.failOn != "" && strings.Contains(sql, f.failOn) {
		return pgconn.CommandTag{}, errors.New("exec failed: " + f.failOn)
	}
	return pgconn.CommandTag{}, nil
}

// QueryRow answers the block path's SELECT state ... FOR UPDATE with
// "implementation", so BlockWorkItemTx can run on the fake.
func (f *fakeTx) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	f.sqls = append(f.sqls, sql)
	f.args = append(f.args, args)
	return implementationRow{}
}

type implementationRow struct{}

func (implementationRow) Scan(dest ...any) error {
	*(dest[0].(*models.WorkItemState)) = models.WorkItemStateImplementation
	return nil
}

func (f *fakeTx) Commit(context.Context) error   { f.commits++; return nil }
func (f *fakeTx) Rollback(context.Context) error { f.rollbacks++; return nil }

type fakeBeginner struct {
	tx     *fakeTx
	begins int
}

func (b *fakeBeginner) Begin(context.Context) (pgx.Tx, error) {
	b.begins++
	return b.tx, nil
}

// outboxRows returns the outbox inserts recorded on tx as (aggregate, topic,
// key, payload).
type outboxRow struct {
	aggregate uuid.UUID
	topic     string
	key       string
	payload   map[string]any
}

func outboxRows(t *testing.T, tx *fakeTx) []outboxRow {
	t.Helper()
	var rows []outboxRow
	for i, sql := range tx.sqls {
		if !strings.Contains(sql, "INSERT INTO outbox_events") {
			continue
		}
		a := tx.args[i]
		if a[1] != outbox.AggregateTypeWorkItem {
			t.Errorf("outbox aggregate_type = %v, want %s", a[1], outbox.AggregateTypeWorkItem)
		}
		var body map[string]any
		if err := json.Unmarshal(a[5].([]byte), &body); err != nil {
			t.Fatalf("outbox payload: %v", err)
		}
		rows = append(rows, outboxRow{aggregate: a[2].(uuid.UUID), topic: a[3].(string), key: a[4].(string), payload: body})
	}
	return rows
}

type fakeBranches struct {
	calls []string
	err   error
}

func (f *fakeBranches) SetBranchTx(_ context.Context, tx pgx.Tx, id uuid.UUID, branch string) error {
	if tx == nil {
		return errors.New("nil tx")
	}
	f.calls = append(f.calls, id.String()+"="+branch)
	return f.err
}

func sampleGatedChange(withPrompt bool) gatedChange {
	workID := uuid.New()
	c := gatedChange{
		WorkID:     workID,
		RepoID:     uuid.New().String(),
		Branch:     "agent/" + workID.String() + "/abcd1234",
		BaseBranch: "main",
		Files:      []string{"a.go"},
		PRTitle:    "[repo] Title",
		PRDesc:     "body",
		HeadSHA:    "0123456789abcdef0123456789abcdef01234567",
		Gated: prReadyMessage{
			WorkID:       workID.String(),
			RepositoryID: "r",
			Branch:       "agent/" + workID.String() + "/abcd1234",
			FilesChanged: []string{"a.go"},
			HeadSHA:      "0123456789abcdef0123456789abcdef01234567",
		},
	}
	if withPrompt {
		c.Prompt = &interactions.Prompt{ID: uuid.New(), WorkItemID: workID, ProductID: uuid.New(), Headline: "PR ready"}
	}
	return c
}

// ─── manual gate: pending row + pr.gated + prompt in one tx ─────────────────

func TestCommitGatedPR_PendingRowEventAndPromptShareOneTx(t *testing.T) {
	tx := &fakeTx{}
	db := &fakeBeginner{tx: tx}
	c := sampleGatedChange(true)

	if err := commitGatedPR(context.Background(), db, interactions.NewPublisher(nil), c); err != nil {
		t.Fatalf("commitGatedPR: %v", err)
	}
	if db.begins != 1 || tx.commits != 1 || tx.rollbacks != 0 {
		t.Fatalf("begins=%d commits=%d rollbacks=%d, want one committed tx", db.begins, tx.commits, tx.rollbacks)
	}
	if len(tx.sqls) != 4 {
		t.Fatalf("statements = %d (%q), want pending insert, pr.gated, prompt row, prompt event", len(tx.sqls), tx.sqls)
	}
	if tx.sqls[0] != insertPendingPRChangeSQL {
		t.Errorf("first statement = %q, want the pending_pr_changes insert", tx.sqls[0])
	}
	if got := tx.args[0][8]; got != c.HeadSHA {
		t.Errorf("pending row head_sha = %v, want %s", got, c.HeadSHA)
	}
	if !strings.Contains(tx.sqls[2], "INSERT INTO user_interactions") {
		t.Errorf("third statement = %q, want the prompt row", tx.sqls[2])
	}
	rows := outboxRows(t, tx)
	if len(rows) != 2 {
		t.Fatalf("outbox rows = %d, want pr.gated + user_prompt.requested", len(rows))
	}
	if rows[0].topic != bus.TopicPRGated || rows[0].key != c.WorkID.String() || rows[0].aggregate != c.WorkID {
		t.Errorf("pr.gated row = %+v", rows[0])
	}
	if rows[0].payload["head_sha"] != c.HeadSHA || rows[0].payload["branch"] != c.Gated.Branch {
		t.Errorf("pr.gated payload = %v", rows[0].payload)
	}
	if rows[1].topic != bus.TopicUserPromptRequested || rows[1].aggregate != c.WorkID {
		t.Errorf("prompt event row = %+v", rows[1])
	}
}

func TestCommitGatedPR_NoPromptWithoutProduct(t *testing.T) {
	tx := &fakeTx{}
	c := sampleGatedChange(false)
	if err := commitGatedPR(context.Background(), &fakeBeginner{tx: tx}, interactions.NewPublisher(nil), c); err != nil {
		t.Fatalf("commitGatedPR: %v", err)
	}
	rows := outboxRows(t, tx)
	if len(tx.sqls) != 2 || len(rows) != 1 || rows[0].topic != bus.TopicPRGated {
		t.Errorf("statements = %q, want pending insert + pr.gated only", tx.sqls)
	}
}

func TestCommitGatedPR_AnyFailureRollsBackEverything(t *testing.T) {
	for _, failOn := range []string{"INSERT INTO pending_pr_changes", "INSERT INTO outbox_events", "INSERT INTO user_interactions"} {
		t.Run(failOn, func(t *testing.T) {
			tx := &fakeTx{failOn: failOn}
			err := commitGatedPR(context.Background(), &fakeBeginner{tx: tx}, interactions.NewPublisher(nil), sampleGatedChange(true))
			if err == nil {
				t.Fatal("commitGatedPR succeeded despite a failing statement")
			}
			if tx.commits != 0 || tx.rollbacks != 1 {
				t.Errorf("commits=%d rollbacks=%d, want a rollback only", tx.commits, tx.rollbacks)
			}
			if strings.Contains(failOn, "pending") && len(tx.sqls) != 1 {
				t.Errorf("statements after a failed pending insert: %q — a failed statement aborts the tx, nothing may follow it", tx.sqls)
			}
		})
	}
}

// recordGatedPR: success gates and lets the loop continue; a failed commit
// blocks the parent (work.blocked in its own committed tx) and tells the loop
// to stop, so no further repository is gated against a blocked parent.
func TestRecordGatedPR_ContinuesOnSuccess(t *testing.T) {
	tx := &fakeTx{}
	db := &fakeBeginner{tx: tx}
	if stop, err := recordGatedPR(context.Background(), db, interactions.NewPublisher(nil), sampleGatedChange(true), "repo"); stop || err != nil {
		t.Fatal("stop = true after a successful gate commit")
	}
	if db.begins != 1 || tx.commits != 1 {
		t.Errorf("begins=%d commits=%d", db.begins, tx.commits)
	}
	for _, r := range outboxRows(t, tx) {
		if r.topic == bus.TopicWorkBlocked {
			t.Error("work.blocked enqueued after a successful gate commit")
		}
	}
}

func TestRecordGatedPR_FailedCommitBlocksParentAndStops(t *testing.T) {
	tx := &fakeTx{failOn: "INSERT INTO pending_pr_changes"}
	db := &fakeBeginner{tx: tx}
	c := sampleGatedChange(true)
	if stop, err := recordGatedPR(context.Background(), db, interactions.NewPublisher(nil), c, "repo"); !stop || err != nil {
		t.Fatal("stop = false after a failed gate commit; the loop would gate the remaining repos against a blocked parent")
	}
	// F2: maxCommitAttempts failed attempts (each rolled back), then the
	// block commits.
	if db.begins != maxCommitAttempts+1 || tx.rollbacks != maxCommitAttempts || tx.commits != 1 {
		t.Fatalf("begins=%d rollbacks=%d commits=%d, want the gate rolled back and the block committed", db.begins, tx.rollbacks, tx.commits)
	}
	rows := outboxRows(t, tx)
	if len(rows) != 1 || rows[0].topic != bus.TopicWorkBlocked || rows[0].key != c.WorkID.String() || rows[0].aggregate != c.WorkID {
		t.Fatalf("outbox rows = %+v, want exactly one work.blocked for the parent", rows)
	}
	if !strings.Contains(rows[0].payload["reason"].(string), "could not record the gated PR for repo repo") {
		t.Errorf("block reason = %v", rows[0].payload["reason"])
	}
}

// The pre-046 fallback is gone: an undefined-column error is returned, not
// retried with another statement inside an aborted transaction.
func TestInsertPendingPRChange_NoLegacyRetry(t *testing.T) {
	tx := &fakeTx{failOn: "head_sha"}
	err := insertPendingPRChange(context.Background(), tx, uuid.New(), "r", "b", "main", nil, "t", "d", "sha")
	if err == nil {
		t.Fatal("expected the insert error")
	}
	if len(tx.sqls) != 1 {
		t.Errorf("statements = %q, want exactly one attempt", tx.sqls)
	}
}

// ─── auto mode: branch + pr.ready in one tx after the PR exists ─────────────

func TestCommitPRReady_BranchAndEventShareOneTx(t *testing.T) {
	tx := &fakeTx{}
	db := &fakeBeginner{tx: tx}
	branches := &fakeBranches{}
	workID := uuid.New()
	msg := prReadyMessage{WorkID: workID.String(), PRURL: "https://github.invalid/pr/7", PRNumber: 7, Branch: "agent/x", HeadSHA: "abc"}

	if err := commitPRReady(context.Background(), db, branches, workID, "agent/x", msg); err != nil {
		t.Fatalf("commitPRReady: %v", err)
	}
	if db.begins != 1 || tx.commits != 1 || tx.rollbacks != 0 {
		t.Fatalf("begins=%d commits=%d rollbacks=%d", db.begins, tx.commits, tx.rollbacks)
	}
	if len(branches.calls) != 1 || branches.calls[0] != workID.String()+"=agent/x" {
		t.Errorf("SetBranchTx calls = %v", branches.calls)
	}
	rows := outboxRows(t, tx)
	if len(rows) != 1 || rows[0].topic != bus.TopicPRReady || rows[0].key != workID.String() || rows[0].aggregate != workID {
		t.Fatalf("outbox rows = %+v, want one pr.ready keyed by the work item", rows)
	}
	if rows[0].payload["pr_url"] != msg.PRURL || rows[0].payload["pr_number"] != float64(7) || rows[0].payload["head_sha"] != "abc" {
		t.Errorf("pr.ready payload = %v", rows[0].payload)
	}
}

func TestCommitPRReady_BranchFailureEnqueuesNothing(t *testing.T) {
	tx := &fakeTx{}
	err := commitPRReady(context.Background(), &fakeBeginner{tx: tx}, &fakeBranches{err: errors.New("db down")}, uuid.New(), "b", prReadyMessage{})
	if err == nil {
		t.Fatal("expected an error")
	}
	if tx.commits != 0 || tx.rollbacks != 1 || len(outboxRows(t, tx)) != 0 {
		t.Errorf("commits=%d rollbacks=%d sqls=%q", tx.commits, tx.rollbacks, tx.sqls)
	}
}

func TestRecordPRReady_ContinuesOnSuccess(t *testing.T) {
	tx := &fakeTx{}
	db := &fakeBeginner{tx: tx}
	workID := uuid.New()
	if stop, err := recordPRReady(context.Background(), db, &fakeBranches{}, workID, "b", "repo", prReadyMessage{WorkID: workID.String(), PRURL: "https://github.invalid/pr/1"}); stop || err != nil {
		t.Fatal("stop = true after a successful pr.ready commit")
	}
	if db.begins != 1 || tx.commits != 1 {
		t.Errorf("begins=%d commits=%d", db.begins, tx.commits)
	}
	rows := outboxRows(t, tx)
	if len(rows) != 1 || rows[0].topic != bus.TopicPRReady {
		t.Errorf("outbox rows = %+v, want only pr.ready", rows)
	}
}

// A failed pr.ready commit after the PR exists blocks the parent and tells
// the loop to stop opening PRs for the other repositories.
func TestRecordPRReady_FailedCommitBlocksParentAndStops(t *testing.T) {
	tx := &fakeTx{}
	db := &fakeBeginner{tx: tx}
	workID := uuid.New()
	msg := prReadyMessage{WorkID: workID.String(), PRURL: "https://github.invalid/pr/9", PRNumber: 9}
	if stop, err := recordPRReady(context.Background(), db, &fakeBranches{err: errors.New("db down")}, workID, "b", "repo", msg); !stop || err != nil {
		t.Fatal("stop = false after a failed pr.ready commit; the loop would open more PRs for a blocked parent")
	}
	// F2: maxCommitAttempts failed attempts (each rolled back), then the
	// block commits.
	if db.begins != maxCommitAttempts+1 || tx.rollbacks != maxCommitAttempts || tx.commits != 1 {
		t.Fatalf("begins=%d rollbacks=%d commits=%d, want pr.ready rolled back and the block committed", db.begins, tx.rollbacks, tx.commits)
	}
	rows := outboxRows(t, tx)
	if len(rows) != 1 || rows[0].topic != bus.TopicWorkBlocked || rows[0].aggregate != workID {
		t.Fatalf("outbox rows = %+v, want exactly one work.blocked for the parent", rows)
	}
	if !strings.Contains(rows[0].payload["reason"].(string), msg.PRURL) {
		t.Errorf("block reason %v does not name the PR URL", rows[0].payload["reason"])
	}
}

func TestCommitPRReady_EnqueueFailureRollsBack(t *testing.T) {
	tx := &fakeTx{failOn: "INSERT INTO outbox_events"}
	err := commitPRReady(context.Background(), &fakeBeginner{tx: tx}, &fakeBranches{}, uuid.New(), "b", prReadyMessage{})
	if err == nil {
		t.Fatal("expected an error")
	}
	if tx.commits != 0 || tx.rollbacks != 1 {
		t.Errorf("commits=%d rollbacks=%d", tx.commits, tx.rollbacks)
	}
}
