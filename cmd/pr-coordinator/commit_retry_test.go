package main

import (
	"bytes"
	"context"
	"errors"
	"log"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/bus"
	"github.com/autonomousengine/autonomous-engine/internal/idempotency"
	"github.com/autonomousengine/autonomous-engine/internal/interactions"
	"github.com/autonomousengine/autonomous-engine/internal/workgraph"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// F2: a failed commit is retried in process (the bus does not redeliver a
// failed message in process), then the item is blocked; an error reaches the
// bus only when the block itself failed.

func init() {
	// No real waiting between attempts in this package's tests.
	commitRetryBackoff = []time.Duration{0, 0}
}

// scriptTx is one attempt's transaction: Exec fails with err when the SQL
// contains failOn, and the ledger guard insert reports one row so the real
// idempotency guard (Wrap) treats the delivery as first-time.
type scriptTx struct {
	*fakeTx
	err error
}

func (s scriptTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	tag, err := s.fakeTx.Exec(ctx, sql, args...)
	if err != nil && s.err != nil {
		return tag, s.err
	}
	if err == nil && strings.Contains(sql, "INSERT INTO processed_events") {
		return pgconn.NewCommandTag("INSERT 0 1"), nil
	}
	return tag, err
}

// attempt is one scripted transaction.
type attempt struct {
	tx  *fakeTx
	err error // returned instead of fakeTx's own error when failOn matches
}

func ok() attempt { return attempt{tx: &fakeTx{}} }

// transient fails the outbox insert with a connection-style error.
func transient() attempt {
	return attempt{tx: &fakeTx{failOn: "INSERT INTO outbox_events"}, err: errors.New("conn reset")}
}

// permanent fails the outbox insert with a unique violation.
func permanent() attempt {
	return attempt{tx: &fakeTx{failOn: "INSERT INTO outbox_events"}, err: &pgconn.PgError{Code: "23505", Message: "duplicate key"}}
}

type scriptBeginner struct {
	steps  []attempt
	begins int
}

func (b *scriptBeginner) Begin(context.Context) (pgx.Tx, error) {
	if b.begins >= len(b.steps) {
		return nil, errors.New("unexpected Begin")
	}
	s := b.steps[b.begins]
	b.begins++
	return scriptTx{fakeTx: s.tx, err: s.err}, nil
}

func (b *scriptBeginner) tx(i int) *fakeTx { return b.steps[i].tx }

// delivery runs fn the way the consumer does: under Ledger.Handler, with the
// delivery's guarded beginner over db. It returns what the handler returned
// to the bus (non-nil: not acknowledged) and the delivery.
func delivery(t *testing.T, db workgraph.TxBeginner, fn func(ctx context.Context, guarded workgraph.TxBeginner) error) (error, *idempotency.Delivery) {
	t.Helper()
	msg := &bus.Message{Topic: bus.TopicVerificationComplete, Headers: map[string]string{bus.HeaderEventID: uuid.NewString()}}
	var d *idempotency.Delivery
	// A ledger without a database: Seen finds nothing, so fn always runs.
	h := idempotency.NewLedger(nil).Handler(idempotencyConsumer, func(ctx context.Context, _ *bus.Message) error {
		d = idempotency.FromContext(ctx)
		return fn(ctx, idempotency.Wrap(ctx, db))
	})
	return h(context.Background(), msg), d
}

func topicRows(t *testing.T, topic string, txs ...*fakeTx) []outboxRow {
	t.Helper()
	var out []outboxRow
	for _, tx := range txs {
		for _, r := range outboxRows(t, tx) {
			if r.topic == topic {
				out = append(out, r)
			}
		}
	}
	return out
}

func allTxs(b *scriptBeginner) []*fakeTx {
	out := make([]*fakeTx, len(b.steps))
	for i := range b.steps {
		out[i] = b.steps[i].tx
	}
	return out
}

func replayComplete(workID uuid.UUID) func(ctx context.Context, g workgraph.TxBeginner) error {
	return func(ctx context.Context, g workgraph.TxBeginner) error {
		return recordReplayComplete(ctx, g, workID, "no verified branch to review")
	}
}

func TestRecordReplayComplete_TransientTwiceThenSucceeds(t *testing.T) {
	db := &scriptBeginner{steps: []attempt{transient(), transient(), ok()}}
	workID := uuid.New()
	herr, d := delivery(t, db, replayComplete(workID))
	if herr != nil {
		t.Fatalf("handler error %v", herr)
	}
	if db.begins != 3 {
		t.Fatalf("begins = %d, want 3 attempts", db.begins)
	}
	for i := 0; i < 2; i++ {
		if tx := db.tx(i); tx.commits != 0 || tx.rollbacks != 1 {
			t.Errorf("attempt %d: commits=%d rollbacks=%d, want rolled back", i+1, tx.commits, tx.rollbacks)
		}
	}
	if rows := topicRows(t, bus.TopicReplayComplete, db.tx(2)); len(rows) != 1 || db.tx(2).commits != 1 {
		t.Fatalf("third attempt: replay.complete rows=%+v commits=%d", rows, db.tx(2).commits)
	}
	if rows := topicRows(t, bus.TopicWorkBlocked, allTxs(db)...); len(rows) != 0 {
		t.Errorf("work.blocked enqueued although the third attempt succeeded: %+v", rows)
	}
	if !d.Recorded() || !strings.Contains(strings.Join(db.tx(2).sqls, "\n"), "INSERT INTO processed_events") {
		t.Error("the successful attempt does not carry the guard row")
	}
}

func TestRecordReplayComplete_TransientThreeTimesBlocks(t *testing.T) {
	db := &scriptBeginner{steps: []attempt{transient(), transient(), transient(), ok()}}
	workID := uuid.New()
	herr, d := delivery(t, db, replayComplete(workID))
	if herr != nil {
		t.Fatalf("handler error %v, want the blocked delivery acknowledged", herr)
	}
	block := db.tx(3)
	rows := topicRows(t, bus.TopicWorkBlocked, block)
	if len(rows) != 1 || rows[0].aggregate != workID {
		t.Fatalf("work.blocked rows = %+v", rows)
	}
	if reason := rows[0].payload["reason"].(string); !strings.HasPrefix(reason, "replay: could not record replay.complete: ") {
		t.Errorf("block reason %q", reason)
	}
	if block.commits != 1 || !d.Recorded() || !strings.Contains(strings.Join(block.sqls, "\n"), "INSERT INTO processed_events") {
		t.Errorf("block commits=%d recorded=%t, want the block to carry the guard row", block.commits, d.Recorded())
	}
	if len(topicRows(t, bus.TopicReplayComplete, allTxs(db)...)) != 3 {
		t.Error("expected three replay.complete attempts")
	}
}

func TestRecordReplayComplete_PermanentErrorBlocksAtOnce(t *testing.T) {
	db := &scriptBeginner{steps: []attempt{permanent(), ok()}}
	herr, _ := delivery(t, db, replayComplete(uuid.New()))
	if herr != nil {
		t.Fatalf("handler error %v", herr)
	}
	if db.begins != 2 || len(topicRows(t, bus.TopicWorkBlocked, db.tx(1))) != 1 {
		t.Fatalf("begins=%d, want one attempt and the block", db.begins)
	}
}

func TestRecordReplayComplete_BlockFailureIsNotAcknowledged(t *testing.T) {
	blockFails := attempt{tx: &fakeTx{failOn: "INSERT INTO outbox_events"}, err: errors.New("still down")}
	db := &scriptBeginner{steps: []attempt{transient(), transient(), transient(), blockFails}}
	herr, d := delivery(t, db, replayComplete(uuid.New()))
	if herr == nil {
		t.Fatal("handler returned nil after the block failed: the item is stranded")
	}
	if !strings.Contains(herr.Error(), "after a failed commit") {
		t.Errorf("handler error %q", herr)
	}
	if blockFails.tx.commits != 0 || d.Recorded() {
		t.Errorf("block commits=%d recorded=%t", blockFails.tx.commits, d.Recorded())
	}
}

func TestPermanentCommitError(t *testing.T) {
	for _, err := range []error{
		&pgconn.PgError{Code: "23505"}, &pgconn.PgError{Code: "23503"}, &pgconn.PgError{Code: "22P02"},
		&pgconn.PgError{Code: "42703"}, workgraph.ErrInvalidTransition, pgx.ErrNoRows,
		errors.Join(errors.New("record branch"), workgraph.ErrInvalidTransition),
	} {
		if !permanentCommitError(err) {
			t.Errorf("%v classified transient", err)
		}
	}
	for _, err := range []error{
		errors.New("conn closed"), &pgconn.PgError{Code: "40001"}, &pgconn.PgError{Code: "40P01"},
		&pgconn.PgError{Code: "57P01"}, context.DeadlineExceeded,
	} {
		if permanentCommitError(err) {
			t.Errorf("%v classified permanent", err)
		}
	}
}

// A cancelled context stops the retries (shutdown); the item is then blocked
// on whatever the database still accepts, or the error goes to the bus.
func TestRetryCommit_StopsWhenContextIsDone(t *testing.T) {
	prev := commitRetryBackoff
	commitRetryBackoff = []time.Duration{time.Hour}
	t.Cleanup(func() { commitRetryBackoff = prev })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	calls := 0
	err := retryCommit(ctx, uuid.New(), "x", func() error { calls++; return errors.New("conn reset") })
	if err == nil || calls != 1 {
		t.Fatalf("err=%v calls=%d, want one attempt", err, calls)
	}
}

// The ordinary PR path and the replay hand-off share the policy.
func TestRecordPRReady_RetriesInProcessThenBlocks(t *testing.T) {
	for _, msg := range []prReadyMessage{
		{PRURL: "https://github.invalid/pr/9", PRNumber: 9},
		{Branch: "agent/x/y", HeadSHA: "abc", Replay: true},
	} {
		// Transient twice, then the pr.ready commits.
		db := &scriptBeginner{steps: []attempt{transient(), transient(), ok()}}
		var stop bool
		herr, _ := delivery(t, db, func(ctx context.Context, g workgraph.TxBeginner) error {
			var err error
			stop, err = recordPRReady(ctx, g, &fakeBranches{}, uuid.New(), "b", "repo", msg)
			return err
		})
		if stop || herr != nil || len(topicRows(t, bus.TopicPRReady, db.tx(2))) != 1 || len(topicRows(t, bus.TopicWorkBlocked, allTxs(db)...)) != 0 {
			t.Fatalf("replay=%t recovered: stop=%t herr=%v", msg.Replay, stop, herr)
		}

		// Transient three times: blocked, naming the PR (or the replay).
		db = &scriptBeginner{steps: []attempt{transient(), transient(), transient(), ok()}}
		herr, _ = delivery(t, db, func(ctx context.Context, g workgraph.TxBeginner) error {
			var err error
			stop, err = recordPRReady(ctx, g, &fakeBranches{}, uuid.New(), "b", "repo", msg)
			return err
		})
		rows := topicRows(t, bus.TopicWorkBlocked, db.tx(3))
		if !stop || herr != nil || len(rows) != 1 {
			t.Fatalf("replay=%t exhausted: stop=%t herr=%v rows=%+v", msg.Replay, stop, herr, rows)
		}
		reason := rows[0].payload["reason"].(string)
		if msg.Replay && !strings.HasPrefix(reason, "replay: could not record review hand-off: ") {
			t.Errorf("replay block reason %q", reason)
		}
		if !msg.Replay && !strings.Contains(reason, msg.PRURL) {
			t.Errorf("block reason %q does not name the PR", reason)
		}

		// The block fails too: the error reaches the bus.
		db = &scriptBeginner{steps: []attempt{transient(), transient(), transient(), transient()}}
		herr, _ = delivery(t, db, func(ctx context.Context, g workgraph.TxBeginner) error {
			_, err := recordPRReady(ctx, g, &fakeBranches{}, uuid.New(), "b", "repo", msg)
			return err
		})
		if herr == nil {
			t.Errorf("replay=%t: a failed block was acknowledged", msg.Replay)
		}
	}
}

func TestRecordGatedPR_RetriesInProcessThenBlocks(t *testing.T) {
	c := sampleGatedChange(true)
	db := &scriptBeginner{steps: []attempt{transient(), ok()}}
	herr, _ := delivery(t, db, func(ctx context.Context, g workgraph.TxBeginner) error {
		stop, err := recordGatedPR(ctx, g, interactions.NewPublisher(nil), c, "repo")
		if stop {
			t.Error("stop = true after the retry succeeded")
		}
		return err
	})
	if herr != nil || len(topicRows(t, bus.TopicPRGated, db.tx(1))) != 1 {
		t.Fatalf("recovered gate: herr=%v", herr)
	}

	db = &scriptBeginner{steps: []attempt{transient(), transient(), transient(), ok()}}
	herr, _ = delivery(t, db, func(ctx context.Context, g workgraph.TxBeginner) error {
		stop, err := recordGatedPR(ctx, g, interactions.NewPublisher(nil), c, "repo")
		if !stop {
			t.Error("stop = false after the gate was blocked")
		}
		return err
	})
	if herr != nil || len(topicRows(t, bus.TopicWorkBlocked, db.tx(3))) != 1 {
		t.Fatalf("exhausted gate: herr=%v", herr)
	}
}

// Once an earlier commit of the delivery recorded the guard row, a failed
// block cannot be retried by redelivery (it would be acknowledged as a
// duplicate): the error is still returned and logged as unrecoverable.
func TestRecordPRReady_BlockFailureAfterGuardRecordedIsUnrecoverable(t *testing.T) {
	var buf bytes.Buffer
	prevOut, prevFlags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prevOut); log.SetFlags(prevFlags) })

	db := &scriptBeginner{steps: []attempt{ok(), transient(), transient(), transient(), transient()}}
	workID := uuid.New()
	herr, d := delivery(t, db, func(ctx context.Context, g workgraph.TxBeginner) error {
		if stop, err := recordPRReady(ctx, g, &fakeBranches{}, workID, "b1", "repo1", prReadyMessage{PRURL: "https://github.invalid/pr/1"}); stop || err != nil {
			t.Fatalf("first repo: stop=%t err=%v", stop, err)
		}
		_, err := recordPRReady(ctx, g, &fakeBranches{}, workID, "b2", "repo2", prReadyMessage{PRURL: "https://github.invalid/pr/2"})
		return err
	})
	if herr == nil || !d.Recorded() {
		t.Fatalf("herr=%v recorded=%t", herr, d.Recorded())
	}
	if !strings.Contains(buf.String(), "UNRECOVERABLE") {
		t.Errorf("log does not say the failed block is unrecoverable:\n%s", buf.String())
	}
}

// main.go: an error from a record helper (a failed block) stops the repo
// loop, skips replay.complete, clears the in-memory "already published" mark
// and returns the error to the bus.
func TestCoordinator_FailedBlockIsReturnedToTheBus(t *testing.T) {
	code := stripComments(coordinatorSources(t)["main.go"])
	if n := strings.Count(code, "unackedErr = rerr\n\t\t\t\t\tbreak subtasks") + strings.Count(code, "unackedErr = rerr\n\t\t\t\tbreak subtasks"); n != 3 {
		t.Errorf("found %d unacknowledged exits from the repo loop, want 3 (replay hand-off, gate, pr.ready)", n)
	}
	if !regexp.MustCompile(`if unackedErr == nil && replay && replayReviews == 0 \{\s*unackedErr = recordReplayComplete\(`).MatchString(code) {
		t.Error("replay.complete is not skipped after a failed block, or its error is dropped")
	}
	if !regexp.MustCompile(`if unackedErr != nil \{\s*session\.mu\.Lock\(\)\s*session\.prPublished = false\s*session\.mu\.Unlock\(\)\s*return fmt\.Errorf\([^\n]*unackedErr\)`).MatchString(code) {
		t.Error("a failed block does not clear prPublished and return the error")
	}
	if !regexp.MustCompile(`return recordReplayComplete\(msgCtx, idempotency\.Wrap\(msgCtx, wg\.DB\(\)\), workID, "no verified subtask"\)`).MatchString(code) {
		t.Error("the eligibleCount==0 replay path drops recordReplayComplete's error")
	}
}
