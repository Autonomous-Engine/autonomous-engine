package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/bus"
	"github.com/autonomousengine/autonomous-engine/internal/guards"
	"github.com/autonomousengine/autonomous-engine/internal/idempotency"
	"github.com/autonomousengine/autonomous-engine/internal/interactions"
	"github.com/autonomousengine/autonomous-engine/internal/outbox"
	"github.com/autonomousengine/autonomous-engine/internal/snapshot"
	"github.com/autonomousengine/autonomous-engine/internal/workgraph"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// M1-04b: every event pr-coordinator emits goes through the transactional
// outbox.
// The work item id pr-coordinator handles is the parent work item, so it is
// the aggregate for every event here.

// gatedChange is what the manual PR gate records for one repository: the
// pending_pr_changes row, the pr.gated event and the pr-gate prompt.
type gatedChange struct {
	WorkID     uuid.UUID
	RepoID     string
	Branch     string
	BaseBranch string
	Files      []string
	PRTitle    string
	PRDesc     string
	HeadSHA    string

	// Gated is the pr.gated payload, unchanged from the direct publish.
	Gated prReadyMessage
	// Prompt is the pr-gate approval prompt. Nil when the work item has no
	// product: user_interactions.product_id is NOT NULL with a foreign key,
	// so the row cannot be written and would roll the pending change back.
	Prompt *interactions.Prompt
	// BaseEvents are base_sha_events rows (M2-03: a clean base drift) that
	// commit with the pending change.
	BaseEvents []snapshot.Event
}

// commitGatedPR writes the pending change, enqueues pr.gated and records the
// pr-gate prompt (row + user_prompt.requested) in one transaction: all of it
// commits or none of it does.
func commitGatedPR(ctx context.Context, db workgraph.TxBeginner, prompts *interactions.Publisher, c gatedChange) error {
	return workgraph.WithTx(ctx, db, func(tx pgx.Tx) error {
		if err := insertPendingPRChange(ctx, tx, c.WorkID, c.RepoID, c.Branch, c.BaseBranch, c.Files, c.PRTitle, c.PRDesc, c.HeadSHA); err != nil {
			return fmt.Errorf("insert pending_pr_changes: %w", err)
		}
		if err := outbox.EnqueueJSON(ctx, tx, c.WorkID, bus.TopicPRGated, c.WorkID.String(), c.Gated); err != nil {
			return err
		}
		if c.Prompt != nil {
			if err := prompts.PublishTx(ctx, tx, c.WorkID, c.Prompt); err != nil {
				return fmt.Errorf("pr gate prompt: %w", err)
			}
		}
		for _, ev := range c.BaseEvents {
			if err := snapshots.RecordEventTx(ctx, tx, ev); err != nil {
				return err
			}
		}
		return nil
	})
}

// branchRecorder is the slice of *workgraph.WorkGraph commitPRReady needs.
type branchRecorder interface {
	SetBranchTx(ctx context.Context, tx pgx.Tx, itemID uuid.UUID, branch string) error
}

// commitPRReady records the PR's branch on the work item and enqueues pr.ready
// in one transaction. It runs only AFTER github-service has opened the PR:
// the GitHub call is an external side effect that cannot be part of the
// transaction. If this commit fails the PR exists without its pr.ready, which
// the caller must log with the PR URL (M1-05 idempotency makes a retry safe).
// baseEvents (M2-03: a clean base drift) commit with them.
func commitPRReady(ctx context.Context, db workgraph.TxBeginner, branches branchRecorder, workID uuid.UUID, branch string, msg prReadyMessage, baseEvents ...snapshot.Event) error {
	return workgraph.WithTx(ctx, db, func(tx pgx.Tx) error {
		if err := branches.SetBranchTx(ctx, tx, workID, branch); err != nil {
			return fmt.Errorf("record branch: %w", err)
		}
		for _, ev := range baseEvents {
			if err := snapshots.RecordEventTx(ctx, tx, ev); err != nil {
				return err
			}
		}
		return outbox.EnqueueJSON(ctx, tx, workID, bus.TopicPRReady, workID.String(), msg)
	})
}

// replayCompleteMessage is the replay.complete payload (M4-06).
type replayCompleteMessage struct {
	WorkID    string `json:"work_id"`
	Reason    string `json:"reason"`
	Timestamp string `json:"timestamp"`
}

// commitReplayComplete enqueues replay.complete for a replay work item with
// nothing to review; on the guarded beginner it carries the delivery's
// ledger row. status-orchestrator cancels the item.
func commitReplayComplete(ctx context.Context, db workgraph.TxBeginner, workID uuid.UUID, reason string) error {
	return workgraph.WithTx(ctx, db, func(tx pgx.Tx) error {
		return outbox.EnqueueJSON(ctx, tx, workID, bus.TopicReplayComplete, workID.String(), replayCompleteMessage{
			WorkID: workID.String(), Reason: reason, Timestamp: time.Now().UTC().Format(time.RFC3339),
		})
	})
}

// blockAfterCommitFailure reports a failed commit the only way pr-coordinator
// can: before the outbox, a lost publish left the work item silently waiting
// for a pr.ready that never came. It blocks the work item (with work.blocked,
// so status-orchestrator raises the escalation prompt) naming what was lost.
// On the guarded beginner the block carries the delivery's ledger row unless
// an earlier commit of the delivery already recorded it, so a committed block
// acknowledges the delivery. If the database is still failing, the block
// fails too and the error is returned: the caller must not acknowledge the
// delivery, or the item is left with neither its event nor a block.
func blockAfterCommitFailure(ctx context.Context, db workgraph.TxBeginner, workID uuid.UUID, reason string) error {
	err := workgraph.WithTx(ctx, db, func(tx pgx.Tx) error {
		return guards.BlockWorkItemTx(ctx, tx, workID, workID, agentID, reason)
	})
	if err == nil {
		return nil
	}
	if d := idempotency.FromContext(ctx); d != nil && d.Recorded() {
		// An earlier commit of this delivery recorded the guard row, so a
		// redelivery is acknowledged as a duplicate without running: nothing
		// will retry this block.
		log.Printf("pr-coordinator: ERROR UNRECOVERABLE could not block work_id=%s after a failed commit (%s): %v — the delivery is already recorded, so a redelivery will not retry; the item needs manual attention", workID, reason, err)
	} else {
		log.Printf("pr-coordinator: ERROR could not block work_id=%s after a failed commit (%s): %v — delivery not acknowledged", workID, reason, err)
	}
	return fmt.Errorf("block work_id=%s after a failed commit: %w", workID, err)
}

// maxCommitAttempts is how many times a commit is run in process before the
// item is blocked (F2).
const maxCommitAttempts = 3

// commitRetryBackoff is the wait before the 2nd and 3rd attempt; tests
// shorten it.
var commitRetryBackoff = []time.Duration{200 * time.Millisecond, 800 * time.Millisecond}

// permanentCommitError reports an error a retry cannot fix: an invalid
// state-machine transition, a missing row, or a PostgreSQL data, integrity
// constraint or schema error (classes 22, 23, 42). Anything else (a lost
// connection, a serialization failure, a deadlock, a shutdown) is transient.
func permanentCommitError(err error) bool {
	if errors.Is(err, workgraph.ErrInvalidTransition) || errors.Is(err, pgx.ErrNoRows) {
		return true
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch {
		case strings.HasPrefix(pgErr.Code, "22"), strings.HasPrefix(pgErr.Code, "23"), strings.HasPrefix(pgErr.Code, "42"):
			return true
		}
	}
	return false
}

// retryCommit runs commit up to maxCommitAttempts times in process (F2). Each
// failed attempt rolled back its own transaction, including the delivery's
// ledger guard row, so running it again is safe; the bus does not redeliver a
// failed message in process, so returning the error instead would leave the
// item stuck. It stops early on success, on a duplicate delivery, on a
// permanent error and when ctx is done, and returns the last error.
func retryCommit(ctx context.Context, workID uuid.UUID, what string, commit func() error) error {
	var err error
	for attempt := 1; attempt <= maxCommitAttempts; attempt++ {
		if err = commit(); err == nil || idempotency.IsDuplicate(err) || permanentCommitError(err) {
			return err
		}
		if attempt == maxCommitAttempts {
			break
		}
		wait := commitRetryBackoff[len(commitRetryBackoff)-1]
		if attempt-1 < len(commitRetryBackoff) {
			wait = commitRetryBackoff[attempt-1]
		}
		log.Printf("pr-coordinator: work_id=%s %s failed (attempt %d/%d): %v — retrying in %s", workID, what, attempt, maxCommitAttempts, err, wait)
		select {
		case <-ctx.Done():
			return err
		case <-time.After(wait):
		}
	}
	return err
}

// recordPRReady commits the branch and pr.ready for a PR github-service has
// already opened (or, for a replay, the PR-less review hand-off), retrying a
// transient failure in process (retryCommit). It returns (false, nil) on
// success. Otherwise stop is true and the caller opens no further PRs for
// the parent: a duplicate delivery records nothing; a commit that still
// fails blocks the parent naming the PR URL (or the replay wording); err is
// non-nil only when that block failed too, and must be returned so the
// delivery is not acknowledged.
func recordPRReady(ctx context.Context, db workgraph.TxBeginner, branches branchRecorder, workID uuid.UUID, branch, repoName string, msg prReadyMessage, baseEvents ...snapshot.Event) (stop bool, err error) {
	cerr := retryCommit(ctx, workID, "pr.ready commit", func() error {
		return commitPRReady(ctx, db, branches, workID, branch, msg, baseEvents...)
	})
	if cerr == nil {
		return false, nil
	}
	if idempotency.IsDuplicate(cerr) {
		// M1-05: a duplicate implementation.complete raced the first past
		// the up-front Seen check; the first delivery recorded its PRs.
		log.Printf("pr-coordinator: work_id=%s duplicate delivery reached the pr.ready commit for %s — nothing recorded, stopping", workID, msg.PRURL)
		return true, nil
	}
	reason := prReadyFailureReason(msg, cerr)
	log.Printf("pr-coordinator: ERROR work_id=%s repo=%s branch=%s: %s (branch update and event rolled back) — blocking", workID, repoName, branch, reason)
	return true, blockAfterCommitFailure(ctx, db, workID, reason)
}

// prReadyFailureReason is the block reason when a pr.ready commit fails. A
// replay review hand-off opened no PR, so it must not say one was opened.
func prReadyFailureReason(msg prReadyMessage, err error) string {
	if msg.Replay {
		return fmt.Sprintf("replay: could not record review hand-off: %v", err)
	}
	return fmt.Sprintf("PR %s was opened but pr-coordinator could not record pr.ready: %v", msg.PRURL, err)
}

// replayCompleteFailureReason is the block reason when replay.complete
// cannot be recorded.
func replayCompleteFailureReason(err error) string {
	return fmt.Sprintf("replay: could not record replay.complete: %v", err)
}

// recordReplayComplete commits replay.complete, retrying a transient failure
// in process. A commit that still fails blocks the replay item with a reason
// that says so (a replay opens no PR). A duplicate delivery records nothing
// and is not a failure. The error is non-nil only when the block failed too:
// the delivery must then not be acknowledged.
func recordReplayComplete(ctx context.Context, db workgraph.TxBeginner, workID uuid.UUID, detail string) error {
	err := retryCommit(ctx, workID, "replay.complete commit", func() error {
		return commitReplayComplete(ctx, db, workID, detail)
	})
	if err == nil || idempotency.IsDuplicate(err) {
		return nil
	}
	reason := replayCompleteFailureReason(err)
	log.Printf("pr-coordinator: ERROR work_id=%s: %s — blocking", workID, reason)
	return blockAfterCommitFailure(ctx, db, workID, reason)
}

// recordGatedPR commits one repository's gated change, retrying a transient
// failure in process. When the commit still fails it blocks the parent and
// returns stop = true: the caller must not gate the remaining repositories,
// because further pending rows and pr-gate prompts against a blocked parent
// would ask a person to approve PRs for work the pipeline has stopped. err is
// non-nil only when the block failed too, and must be returned so the
// delivery is not acknowledged. Re-running after the block is resolved gates
// every repository again.
func recordGatedPR(ctx context.Context, db workgraph.TxBeginner, prompts *interactions.Publisher, c gatedChange, repoName string) (stop bool, err error) {
	cerr := retryCommit(ctx, c.WorkID, "gate commit", func() error {
		return commitGatedPR(ctx, db, prompts, c)
	})
	if cerr == nil {
		log.Printf("pr-coordinator: PR gated for work_id=%s repo=%s branch=%s head=%s — recorded as pending", c.WorkID, repoName, c.Branch, c.HeadSHA)
		return false, nil
	}
	if idempotency.IsDuplicate(cerr) {
		log.Printf("pr-coordinator: work_id=%s duplicate delivery reached the gate commit — nothing recorded, stopping", c.WorkID)
		return true, nil // M1-05: the first delivery gated these repositories
	}
	log.Printf("pr-coordinator: ERROR gated PR for work_id=%s repo=%s branch=%s not recorded (pending row, pr.gated and prompt rolled back): %v — blocking", c.WorkID, repoName, c.Branch, cerr)
	return true, blockAfterCommitFailure(ctx, db, c.WorkID, fmt.Sprintf("pr-coordinator could not record the gated PR for repo %s branch %s: %v", repoName, c.Branch, cerr))
}
