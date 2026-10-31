package main

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/guards"
	"github.com/autonomousengine/autonomous-engine/internal/guards/groundingtest"
	"github.com/autonomousengine/autonomous-engine/internal/models"
	"github.com/autonomousengine/autonomous-engine/internal/workgraph"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Codex coherence Part 3 residual: the Re-index sweep and poster act only on
// refusals that still describe their item — blocked on the refusal's own
// block transition, or parked in awaiting_user from it. Any other refusal is
// dropped: no escalation, no re-post.

// movedOn walks a refused item out of its refusal's block: retried
// elsewhere (research), and when reblock is set blocked again for another
// reason (a newer block transition than the refusal's).
func movedOn(t *testing.T, pool *pgxpool.Pool, f groundingtest.Fixture, reblock bool) {
	t.Helper()
	ctx := context.Background()
	if err := workgraph.New(pool).TransitionState(ctx, f.Item, models.WorkItemStateResearch, "someone", "retried elsewhere"); err != nil {
		t.Fatal(err)
	}
	if !reblock {
		return
	}
	if err := workgraph.WithTx(ctx, pool, func(tx pgx.Tx) error {
		return guards.BlockWorkItemTx(ctx, tx, f.Item, f.Item, "research-coordinator", "No LLM provider configured.")
	}); err != nil {
		t.Fatal(err)
	}
}

// parked puts a refused item on its escalation prompt (awaiting_user from
// blocked) without a new block transition.
func parked(t *testing.T, pool *pgxpool.Pool, f groundingtest.Fixture) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `UPDATE work_items SET state = 'awaiting_user', previous_state = 'blocked' WHERE id = $1`, f.Item); err != nil {
		t.Fatal(err)
	}
}

func pendingEscalations(t *testing.T, pool *pgxpool.Pool, f groundingtest.Fixture) int {
	t.Helper()
	return count(t, pool, `SELECT count(*) FROM user_interactions WHERE work_item_id = $1 AND state = 'pending' AND prompt_type = 'escalation'`, f.Item)
}

func requestReindex(t *testing.T, pool *pgxpool.Pool, f groundingtest.Fixture, age time.Duration) {
	t.Helper()
	ctx := context.Background()
	if err := guards.MarkReindexRequested(ctx, pool, f.Item, "operator"); err != nil {
		t.Fatal(err)
	}
	if age > 0 {
		if _, err := pool.Exec(ctx, `UPDATE codex_grounding_refusals SET reindex_requested_at = now() - $2::interval WHERE work_item_id = $1`, f.Item, age.String()); err != nil {
			t.Fatal(err)
		}
	}
}

func TestIntegration_ReindexSweep_DropsRefusalsThatNoLongerDescribeTheItem(t *testing.T) {
	pool := integrationPool(t)
	ctx := context.Background()
	retried, reblocked, park := refused(t, pool), refused(t, pool), refused(t, pool)
	for _, f := range []groundingtest.Fixture{retried, reblocked, park} {
		requestReindex(t, pool, f, 3*time.Hour)
	}
	movedOn(t, pool, retried, false)
	movedOn(t, pool, reblocked, true)
	parked(t, pool, park)

	ids, err := sweepStaleReindex(ctx, pool, 2*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	swept := map[string]bool{}
	for _, id := range ids {
		swept[id.String()] = true
	}
	for name, f := range map[string]groundingtest.Fixture{"retried": retried, "re-blocked": reblocked} {
		if swept[f.Item.String()] {
			t.Errorf("%s item: a refusal that no longer describes the item was swept", name)
		}
		if _, ok, _ := guards.LoadRefusal(ctx, pool, f.Item); ok {
			t.Errorf("%s item: the stale refusal must be dropped", name)
		}
		if n := pendingEscalations(t, pool, f); n != 0 {
			t.Errorf("%s item: %d escalations raised for a stale refusal, want 0", name, n)
		}
	}
	if !swept[park.Item.String()] {
		t.Errorf("the parked item's refusal is current and must be swept (swept %v)", ids)
	}
}

func TestIntegration_ReindexPoster_DropsRefusalsThatNoLongerDescribeTheItem(t *testing.T) {
	pool := integrationPool(t)
	ctx := context.Background()
	retried, reblocked, park := refused(t, pool), refused(t, pool), refused(t, pool)
	for _, f := range []groundingtest.Fixture{retried, reblocked, park} {
		requestReindex(t, pool, f, 0)
	}
	movedOn(t, pool, retried, false)
	movedOn(t, pool, reblocked, true)
	parked(t, pool, park)

	// The indexer fails every post: a stale refusal would be escalated.
	idx := &fakeIndexer{status: http.StatusInternalServerError}
	url := idx.server(t)
	for i := 0; i < 3; i++ { // other packages' rows may share the database
		postPendingReindex(ctx, pool, url)
	}
	idx.mu.Lock()
	calls := append([]string(nil), idx.calls...)
	idx.mu.Unlock()
	posted := func(f groundingtest.Fixture) bool {
		for _, c := range calls {
			if strings.Contains(c, f.Product.String()) {
				return true
			}
		}
		return false
	}
	for name, f := range map[string]groundingtest.Fixture{"retried": retried, "re-blocked": reblocked} {
		if posted(f) {
			t.Errorf("%s item: a stale refusal's Re-index was posted", name)
		}
		if _, ok, _ := guards.LoadRefusal(ctx, pool, f.Item); ok {
			t.Errorf("%s item: the stale refusal must be dropped", name)
		}
		if n := pendingEscalations(t, pool, f); n != 0 {
			t.Errorf("%s item: %d escalations raised for a stale refusal, want 0", name, n)
		}
	}
	if !posted(park) {
		t.Errorf("the parked item's Re-index is current and must be posted (calls %v)", calls)
	}
}
