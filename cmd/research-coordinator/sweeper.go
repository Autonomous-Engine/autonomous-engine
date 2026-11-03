package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/idempotency"
	"github.com/autonomousengine/autonomous-engine/internal/models"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// sweepGrace is how long past the round time budget the sweeper waits before
// closing a round itself, so a result that arrives at the deadline still
// closes the round through the normal path.
const sweepGrace = 5 * time.Minute

// expiredRoundsSQL closes every open round past its time budget and returns
// what the block reason needs. Only open rounds qualify: a synthesizing
// round has a claimant that will close it.
const expiredRoundsSQL = `
UPDATE research_rounds r
SET status = 'blocked', closed_at = now()
WHERE r.status = 'open' AND r.created_at < now() - $1::interval
RETURNING r.id, r.work_item_id, COALESCE(r.aggregate_id, r.work_item_id),
	(SELECT count(*) FROM research_round_tasks t WHERE t.round_id = r.id AND t.status = 'pending'),
	(SELECT count(*) FROM research_round_tasks t WHERE t.round_id = r.id),
	COALESCE((SELECT string_agg(t.task_id, ', ' ORDER BY t.task_id) FROM research_round_tasks t WHERE t.round_id = r.id AND t.status = 'pending'), ''),
	COALESCE((SELECT w.state::text FROM work_items w WHERE w.id = r.work_item_id), '')`

type expiredRound struct {
	id, workID, aggregateID uuid.UUID
	pending, total          int
	pendingIDs, itemState   string
}

// sweepExpiredRounds closes every open round older than timeBudget as
// blocked and blocks its work item with the reason. The round time budget
// was otherwise only evaluated when a result arrived, so a round whose
// tasks never reported left the item in "research" forever (F37). It
// returns the number of rounds closed.
func sweepExpiredRounds(ctx context.Context, pool *pgxpool.Pool, timeBudget time.Duration) (int, error) {
	rows, err := pool.Query(ctx, expiredRoundsSQL, timeBudget.String())
	if err != nil {
		return 0, fmt.Errorf("close expired rounds: %w", err)
	}
	var expired []expiredRound
	for rows.Next() {
		var r expiredRound
		if err := rows.Scan(&r.id, &r.workID, &r.aggregateID, &r.pending, &r.total, &r.pendingIDs, &r.itemState); err != nil {
			rows.Close()
			return 0, fmt.Errorf("scan expired round: %w", err)
		}
		expired = append(expired, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	for _, r := range expired {
		reason := fmt.Sprintf("research round timed out after %s: %d of %d tasks never reported (%s). Retry re-runs the missing tasks.",
			timeBudget, r.pending, r.total, r.pendingIDs)
		log.Printf("research-coordinator: round %s for work_id=%s expired — %s", r.id, r.workID, reason)
		if r.itemState != string(models.WorkItemStateResearch) {
			// Already blocked, cancelled or moved on: the round is closed, nothing to report.
			continue
		}
		if err := blockInOwnTx(ctx, idempotency.Wrap(ctx, pool), r.aggregateID, r.workID, reason); err != nil {
			log.Printf("research-coordinator: block work_id=%s after round expiry: %v", r.workID, err)
		}
	}
	return len(expired), nil
}

// runRoundSweeper runs sweepExpiredRounds every interval until ctx ends.
func runRoundSweeper(ctx context.Context, pool *pgxpool.Pool, interval, timeBudget time.Duration) {
	run := func() {
		if _, err := sweepExpiredRounds(ctx, pool, timeBudget); err != nil && ctx.Err() == nil {
			log.Printf("research-coordinator: round sweeper: %v", err)
		}
	}
	run()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			run()
		}
	}
}
