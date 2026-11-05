package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/db"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func budgetIntegrationPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN not set — skipping DB-integration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	p, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	if err := db.Migrate(ctx, p, filepath.Join("..", "..", "migrations")); err != nil {
		p.Close()
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(p.Close)
	return p
}

// The research share was measured over the item's lifetime, so once a
// research round had spent it, "Retry" from the dashboard re-dispatched
// research and it blocked again after 0 iterations, forever (rerun NEW-8).
// The share is per research round: spend before the latest dispatch to
// research does not count. WORK_ITEM_TOKEN_BUDGET still caps the lifetime.
func TestIntegration_ResearchSpentCountsTheCurrentRoundOnly(t *testing.T) {
	pool := budgetIntegrationPool(t)
	ctx := context.Background()
	workID := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO work_items (id, type, state, title) VALUES ($1, 'feature', 'research', 'share per round')`, workID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM inference_logs WHERE work_item_id = $1`, workID)
		_, _ = pool.Exec(ctx, `DELETE FROM work_item_transitions WHERE work_item_id = $1`, workID)
		_, _ = pool.Exec(ctx, `DELETE FROM work_items WHERE id = $1`, workID)
	})
	logRow := func(tokens int64, at time.Time) {
		if _, err := pool.Exec(ctx, `INSERT INTO inference_logs (id, model_provider, model_id, intent_type, input_tokens, output_tokens, latency_ms, success, error_message, work_item_id, created_at, prompt_messages, response_content, role)
			VALUES ($1, 'ollama', 'qwen', 'tool_loop', $2, 0, 1, true, '', $3, $4, '[]', '', 'research')`, uuid.New(), tokens, workID, at); err != nil {
			t.Fatal(err)
		}
	}
	dispatch := func(at time.Time) {
		if _, err := pool.Exec(ctx, `INSERT INTO work_item_transitions (work_item_id, from_state, to_state, agent_id, reason, "timestamp") VALUES ($1, 'prd', 'research', 'research-coordinator', 'dispatching', $2)`, workID, at); err != nil {
			t.Fatal(err)
		}
	}
	base := time.Now().UTC().Add(-time.Hour)
	dispatch(base)
	logRow(150_000, base.Add(time.Minute))
	logRow(70_000, base.Add(2*time.Minute)) // the first round spent 220 000

	spent := researchSpent(pool, workID)
	if n, ok := spent(ctx); !ok || n != 220_000 {
		t.Fatalf("first round spend = %d ok=%v, want 220000", n, ok)
	}

	dispatch(base.Add(10 * time.Minute)) // Retry: a new research round
	logRow(5_000, base.Add(11*time.Minute))
	if n, ok := spent(ctx); !ok || n != 5_000 {
		t.Fatalf("second round spend = %d ok=%v, want 5000 (the first round's spend must not count)", n, ok)
	}
}

// The research share is measured on work tokens where a row has them.
func TestIntegration_ResearchSpentUsesWorkTokens(t *testing.T) {
	pool := budgetIntegrationPool(t)
	ctx := context.Background()
	workID := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO work_items (id, type, state, title) VALUES ($1, 'feature', 'research', 'work tokens')`, workID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM inference_logs WHERE work_item_id = $1`, workID)
		_, _ = pool.Exec(ctx, `DELETE FROM work_items WHERE id = $1`, workID)
	})
	if _, err := pool.Exec(ctx, `INSERT INTO inference_logs (id, model_provider, model_id, intent_type, input_tokens, output_tokens, latency_ms, success, error_message, work_item_id, created_at, prompt_messages, response_content, role, work_tokens)
		VALUES ($1, 'ollama', 'qwen', 'tool_loop', 10000, 500, 1, true, '', $2, now(), '[]', '', 'research', 900),
		       ($3, 'ollama', 'qwen', 'criteria', 3000, 400, 1, true, '', $2, now(), '[]', '', 'research', NULL)`, uuid.New(), workID, uuid.New()); err != nil {
		t.Fatal(err)
	}
	if n, ok := researchSpent(pool, workID)(ctx); !ok || n != 900+3400 {
		t.Fatalf("research spend = %d ok=%v, want 900 (work) + 3400 (raw, no work_tokens)", n, ok)
	}
}
