package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/bus"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// seqBeginner hands out its transactions in order.
type seqBeginner struct {
	txs    []*fakeTx
	begins int
}

func (b *seqBeginner) Begin(context.Context) (pgx.Tx, error) {
	tx := b.txs[b.begins]
	b.begins++
	return tx, nil
}

// M4-06: a replay review hand-off opens no PR, so a failed commit must not
// block the item claiming one was opened.
func TestRecordPRReady_ReplayFailureSaysReplay(t *testing.T) {
	tx := &fakeTx{}
	db := &fakeBeginner{tx: tx}
	workID := uuid.New()
	msg := prReadyMessage{WorkID: workID.String(), Branch: "agent/x/y", HeadSHA: "abc", Replay: true}
	if stop, err := recordPRReady(context.Background(), db, &fakeBranches{err: errors.New("db down")}, workID, "agent/x/y", "repo", msg); !stop || err != nil {
		t.Fatal("stop = false after a failed replay hand-off commit")
	}
	rows := outboxRows(t, tx)
	if len(rows) != 1 || rows[0].topic != bus.TopicWorkBlocked {
		t.Fatalf("outbox rows = %+v, want one work.blocked", rows)
	}
	reason := rows[0].payload["reason"].(string)
	if !strings.HasPrefix(reason, "replay: could not record review hand-off: ") || strings.Contains(reason, "was opened") {
		t.Errorf("block reason %q", reason)
	}
	// A real PR keeps the old wording.
	if got := prReadyFailureReason(prReadyMessage{PRURL: "https://github.invalid/pr/1"}, errors.New("x")); !strings.Contains(got, "PR https://github.invalid/pr/1 was opened") {
		t.Errorf("non-replay reason %q", got)
	}
}

func TestRecordReplayComplete_FailureBlocksWithReplayReason(t *testing.T) {
	failing := &fakeTx{failOn: "INSERT INTO outbox_events"}
	blocking := &fakeTx{}
	db := &seqBeginner{txs: []*fakeTx{failing, failing, failing, blocking}}
	workID := uuid.New()
	// F2: retried in process maxCommitAttempts times, then blocked.
	if err := recordReplayComplete(context.Background(), db, workID, "no verified branch to review"); err != nil {
		t.Fatalf("recordReplayComplete: %v, want the item blocked", err)
	}
	if failing.rollbacks != maxCommitAttempts || failing.commits != 0 {
		t.Fatalf("replay.complete tx: commits=%d rollbacks=%d", failing.commits, failing.rollbacks)
	}
	rows := outboxRows(t, blocking)
	if len(rows) != 1 || rows[0].topic != bus.TopicWorkBlocked {
		t.Fatalf("outbox rows = %+v, want one work.blocked", rows)
	}
	if reason := rows[0].payload["reason"].(string); !strings.HasPrefix(reason, "replay: could not record replay.complete: ") {
		t.Errorf("block reason %q", reason)
	}

	// Success enqueues replay.complete and blocks nothing.
	ok := &fakeTx{}
	if err := recordReplayComplete(context.Background(), &fakeBeginner{tx: ok}, workID, "x"); err != nil {
		t.Fatalf("recordReplayComplete: %v", err)
	}
	if rows := outboxRows(t, ok); len(rows) != 1 || rows[0].topic != bus.TopicReplayComplete {
		t.Errorf("outbox rows = %+v, want one replay.complete", rows)
	}
}
