package tools

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/db"
	"github.com/autonomousengine/autonomous-engine/internal/llm"
	"github.com/autonomousengine/autonomous-engine/internal/llm/adapter"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func toolsIntegrationPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		dsn = os.Getenv("TEST_DATABASE_URL")
	}
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN / TEST_DATABASE_URL not set — skipping DB-integration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	p, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(ctx, p, filepath.Join("..", "..", "..", "migrations")); err != nil {
		p.Close()
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(p.Close)
	return p
}

// Every tool-loop call must leave an inference_logs row with its tokens:
// the budget, cost and outcome views are built on them. A string written
// into the UUID agent_id column made the insert fail silently, so no
// tool-loop spend was recorded at all (review C1).
func TestIntegration_ToolLoopRecordsInferenceRowsWithTags(t *testing.T) {
	pool := toolsIntegrationPool(t)
	ctx := context.Background()
	productID, workID := uuid.New(), uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO products (id, name) VALUES ($1, $2)`, productID, "tools-"+productID.String()[:8]); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO work_items (id, type, state, title, product_id) VALUES ($1, 'feature', 'research', 'tool loop rows', $2)`, workID, productID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM inference_logs WHERE work_item_id = $1`, workID)
		_, _ = pool.Exec(ctx, `DELETE FROM work_items WHERE id = $1`, workID)
		_, _ = pool.Exec(ctx, `DELETE FROM products WHERE id = $1`, productID)
	})

	t.Setenv("WORK_ITEM_TOKEN_BUDGET", "500000")
	tagged := llm.WithInferenceTags(ctx, llm.InferenceTags{Role: "research", AgentType: "research-agent"})
	ex := NewExecutor(t.TempDir())
	req := adapter.CompletionRequest{Messages: []adapter.ChatMessage{{Role: adapter.RoleUser, Content: "go"}}, Tools: SafeImplementationTools()}
	_, _ = RunToolLoop(tagged, &listingAdapter{}, req, ex, 2, pool, &workID)

	var rows, tokens int64
	if err := pool.QueryRow(ctx, `SELECT count(*), COALESCE(SUM(input_tokens + output_tokens), 0) FROM inference_logs
		WHERE work_item_id = $1 AND intent_type = 'tool_loop' AND agent_type = 'research-agent'`, workID).Scan(&rows, &tokens); err != nil {
		t.Fatal(err)
	}
	if rows < 1 || tokens < 200 {
		t.Fatalf("rows=%d tokens=%d: tool-loop calls must be recorded with their tokens", rows, tokens)
	}
}

// Each tool-loop row records work_tokens — the call's output plus the
// input it had not sent before — which the budget sums instead of the
// whole resent prompt.
func TestIntegration_ToolLoopRowsCarryWorkTokens(t *testing.T) {
	pool := toolsIntegrationPool(t)
	ctx := context.Background()
	productID, workID := uuid.New(), uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO products (id, name) VALUES ($1, $2)`, productID, "work-"+productID.String()[:8]); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO work_items (id, type, state, title, product_id) VALUES ($1, 'feature', 'research', 'work tokens', $2)`, workID, productID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM inference_logs WHERE work_item_id = $1`, workID)
		_, _ = pool.Exec(ctx, `DELETE FROM work_items WHERE id = $1`, workID)
		_, _ = pool.Exec(ctx, `DELETE FROM products WHERE id = $1`, productID)
	})
	t.Setenv("WORK_ITEM_TOKEN_BUDGET", "500000")
	ex := NewExecutor(t.TempDir())
	req := adapter.CompletionRequest{Messages: []adapter.ChatMessage{{Role: adapter.RoleUser, Content: "go"}}, Tools: SafeImplementationTools()}
	_, _ = RunToolLoop(ctx, &listingAdapter{}, req, ex, 3, pool, &workID)
	rows, err := pool.Query(ctx, `SELECT input_tokens, output_tokens, work_tokens FROM inference_logs WHERE work_item_id = $1 AND intent_type = 'tool_loop' ORDER BY created_at`, workID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var in, out int
		var work *int
		if err := rows.Scan(&in, &out, &work); err != nil {
			t.Fatal(err)
		}
		n++
		if work == nil || *work <= 0 || *work > in+out {
			t.Fatalf("row %d: work_tokens = %d, want 0 < work ≤ input+output (%d+%d)", n, *work, in, out)
		}
	}
	if n < 2 {
		t.Fatalf("%d tool-loop rows, want at least 2", n)
	}
	var budgetSum, rawSum int64
	_ = pool.QueryRow(ctx, `SELECT COALESCE(SUM(COALESCE(work_tokens, input_tokens + output_tokens)), 0), COALESCE(SUM(input_tokens + output_tokens), 0) FROM inference_logs WHERE work_item_id = $1`, workID).Scan(&budgetSum, &rawSum)
	if got, _ := sumWorkItemTokens(ctx, pool, workID); got != budgetSum || got > rawSum {
		t.Fatalf("sumWorkItemTokens = %d, want the work sum %d (raw %d)", got, budgetSum, rawSum)
	}
}
