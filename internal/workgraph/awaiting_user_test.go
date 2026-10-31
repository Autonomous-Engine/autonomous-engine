package workgraph_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/workgraph"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func awaitPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN not set")
	}
	p, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	return p
}

func fixtureProductAndWI(t *testing.T, p *pgxpool.Pool, state string) (uuid.UUID, uuid.UUID) {
	t.Helper()
	productID := uuid.New()
	ctx := context.Background()
	if _, err := p.Exec(ctx, `INSERT INTO products (id, name, created_at, updated_at) VALUES ($1, $2, NOW(), NOW())`, productID, "fx-awaituser-"+productID.String()[:8]); err != nil {
		t.Fatalf("product: %v", err)
	}
	wiID := uuid.New()
	if _, err := p.Exec(ctx, `
		INSERT INTO work_items (id, product_id, title, state, type, priority, created_at, updated_at)
		VALUES ($1, $2, 'fx', $3, 'feature', 'medium', NOW(), NOW())
	`, wiID, productID, state); err != nil {
		t.Fatalf("wi: %v", err)
	}
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	t.Cleanup(func() { cleanupFixtureProductViaDSN(dsn, productID) })
	return productID, wiID
}

// cleanupFixtureProductViaDSN opens a fresh pool and tears down the product
// and every referencing row. Uses a fresh connection because t.Cleanup runs
// AFTER the test's deferred p.Close() — so the primary pool would be gone.
func cleanupFixtureProductViaDSN(dsn string, productID uuid.UUID) {
	if dsn == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return
	}
	defer pool.Close()
	for _, table := range []string{"codex_anti_patterns", "codex_patterns", "codex_modules", "codex_sections"} {
		_, _ = pool.Exec(ctx, `DELETE FROM `+table+` WHERE source_interaction_id IN (SELECT id FROM user_interactions WHERE product_id = $1) OR product_id = $1`, productID)
	}
	_, _ = pool.Exec(ctx, `UPDATE work_items SET awaiting_prompt_id = NULL WHERE product_id = $1`, productID)
	_, _ = pool.Exec(ctx, `DELETE FROM user_interactions WHERE product_id = $1`, productID)
	// Explicit work_items delete — FK is ON DELETE SET NULL, not CASCADE.
	_, _ = pool.Exec(ctx, `DELETE FROM work_items WHERE product_id = $1`, productID)
	_, _ = pool.Exec(ctx, `DELETE FROM products WHERE id = $1`, productID)
}

// fixtureInteraction inserts a user_interactions row so the FK from
// work_items.awaiting_prompt_id → user_interactions.id resolves. Returns the prompt id.
func fixtureInteraction(t *testing.T, p *pgxpool.Pool, wiID, productID uuid.UUID) uuid.UUID {
	t.Helper()
	promptID := uuid.New()
	if _, err := p.Exec(context.Background(), `
		INSERT INTO user_interactions (id, work_item_id, product_id, asking_agent, trigger, prompt_type,
			headline, question, state, requested_at)
		VALUES ($1, $2, $3, 'impl', 'agent_request', 'clarification', 'h', 'q', 'pending', NOW())
	`, promptID, wiID, productID); err != nil {
		t.Fatalf("interaction: %v", err)
	}
	return promptID
}

func TestTransitionToAwaitingUser_FromImplementation(t *testing.T) {
	p := awaitPool(t)
	defer p.Close()
	ctx := context.Background()

	productID, wiID := fixtureProductAndWI(t, p, "implementation")
	promptID := fixtureInteraction(t, p, wiID, productID)

	if err := workgraph.TransitionToAwaitingUser(ctx, p, wiID, promptID); err != nil {
		t.Fatalf("TransitionToAwaitingUser: %v", err)
	}

	var state, prev string
	var pid *uuid.UUID
	if err := p.QueryRow(ctx, `SELECT state, COALESCE(previous_state, ''), awaiting_prompt_id FROM work_items WHERE id = $1`, wiID).Scan(&state, &prev, &pid); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if state != "awaiting_user" {
		t.Errorf("state = %q", state)
	}
	if prev != "implementation" {
		t.Errorf("previous_state = %q", prev)
	}
	if pid == nil || *pid != promptID {
		t.Errorf("awaiting_prompt_id = %v, want %v", pid, promptID)
	}
}

func TestTransitionFromAwaitingUser_RestoresPreviousState(t *testing.T) {
	p := awaitPool(t)
	defer p.Close()
	ctx := context.Background()

	productID, wiID := fixtureProductAndWI(t, p, "research")
	promptID := fixtureInteraction(t, p, wiID, productID)

	// Go into awaiting_user.
	if err := workgraph.TransitionToAwaitingUser(ctx, p, wiID, promptID); err != nil {
		t.Fatalf("to: %v", err)
	}
	// Come back out.
	if err := workgraph.TransitionFromAwaitingUser(ctx, p, wiID); err != nil {
		t.Fatalf("from: %v", err)
	}

	var state string
	var prev *string
	var pid *uuid.UUID
	if err := p.QueryRow(ctx, `SELECT state, previous_state, awaiting_prompt_id FROM work_items WHERE id = $1`, wiID).Scan(&state, &prev, &pid); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if state != "research" {
		t.Errorf("state should be restored to 'research', got %q", state)
	}
	if prev != nil {
		t.Errorf("previous_state should be nil, got %q", *prev)
	}
	if pid != nil {
		t.Errorf("awaiting_prompt_id should be nil, got %v", *pid)
	}
}

func TestTransitionFromAwaitingUser_NoopWhenNotAwaiting(t *testing.T) {
	p := awaitPool(t)
	defer p.Close()
	ctx := context.Background()

	_, wiID := fixtureProductAndWI(t, p, "implementation")

	// Never transitioned to awaiting_user — From should be a no-op.
	if err := workgraph.TransitionFromAwaitingUser(ctx, p, wiID); err != nil {
		t.Fatalf("TransitionFromAwaitingUser (no-op path): %v", err)
	}

	var state string
	_ = p.QueryRow(ctx, `SELECT state FROM work_items WHERE id = $1`, wiID).Scan(&state)
	if state != "implementation" {
		t.Errorf("state changed unexpectedly: %q", state)
	}
}
