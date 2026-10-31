// Package groundingtest holds the fixtures the stages' codex grounding
// tests share (codex coherence Part 3): a product whose codex has patterns
// but no module, and the assertions that a refusal escalated through
// work.blocked (kind codex_ungrounded) with a recorded re-run.
package groundingtest

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/bus"
	"github.com/autonomousengine/autonomous-engine/internal/guards"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Fixture is a product with one enabled repository on DefaultBranch, a
// codex version, codex patterns but no module, and one work item.
type Fixture struct {
	Product, Repo, Item uuid.UUID
	DefaultBranch       string
}

// PatternsOnly seeds the fixture with the item in state and registers its
// cleanup.
func PatternsOnly(t *testing.T, pool *pgxpool.Pool, state, defaultBranch string) Fixture {
	t.Helper()
	ctx := context.Background()
	f := Fixture{Product: uuid.New(), Repo: uuid.New(), Item: uuid.New(), DefaultBranch: defaultBranch}
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO products (id, name) VALUES ($1, $2)`, []any{f.Product, "p3-" + f.Product.String()}},
		{`INSERT INTO repositories (id, product_id, name, github_url, base_branch, enabled) VALUES ($1, $2, 'p3-repo', $3, $4, true)`,
			[]any{f.Repo, f.Product, "o/p3-" + f.Repo.String(), defaultBranch}},
		{`INSERT INTO codex_versions (product_id, branch, product_version, sections_changed, change_summary, change_source) VALUES ($1, $2, 1, '{patterns}', 'p3 fixture', 'onboarding')`,
			[]any{f.Product, defaultBranch}},
		{`INSERT INTO codex_patterns (id, name, product_id, branch) VALUES ($1, $2, $3, $4)`,
			[]any{uuid.New(), "p3-pattern-" + f.Product.String()[:8], f.Product, defaultBranch}},
		{`INSERT INTO work_items (id, type, state, title, description, product_id) VALUES ($1, 'feature', $3, 'P3 item', 'd', $2)`,
			[]any{f.Item, f.Product, state}},
	} {
		if _, err := pool.Exec(ctx, q.sql, q.args...); err != nil {
			t.Fatalf("%s: %v", q.sql, err)
		}
	}
	t.Cleanup(func() {
		ctx := context.Background()
		for _, q := range []string{
			`DELETE FROM outbox_events WHERE aggregate_id = $1`,
			`UPDATE work_items SET awaiting_prompt_id = NULL WHERE id = $1`,
			`DELETE FROM user_interactions WHERE work_item_id = $1`,
			`DELETE FROM work_items WHERE id = $1`,
		} {
			_, _ = pool.Exec(ctx, q, f.Item)
		}
		for _, q := range []string{
			`DELETE FROM codex_modules WHERE product_id = $1`,
			`DELETE FROM codex_patterns WHERE product_id = $1`,
			`DELETE FROM codex_versions WHERE product_id = $1`,
			`DELETE FROM repositories WHERE product_id = $1`,
			`DELETE FROM products WHERE id = $1`,
		} {
			_, _ = pool.Exec(ctx, q, f.Product)
		}
	})
	return f
}

// AddModule grounds the fixture's default branch with one module.
func (f Fixture) AddModule(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `INSERT INTO codex_modules (id, name, product_id, branch) VALUES ($1, $2, $3, $4)`,
		uuid.New(), "p3-module-"+uuid.NewString()[:8], f.Product, f.DefaultBranch); err != nil {
		t.Fatal(err)
	}
}

// AssertEscalated checks that item was refused by stage the escalating way:
// blocked, work.blocked enqueued with kind codex_ungrounded (the
// status-orchestrator then raises the Re-index / Retry / Cancel prompt),
// and a refusal recorded that re-runs rerunTopic from rerunState.
func AssertEscalated(t *testing.T, pool *pgxpool.Pool, item uuid.UUID, stage, rerunTopic, rerunState string) guards.StoredRefusal {
	t.Helper()
	ctx := context.Background()
	var state string
	if err := pool.QueryRow(ctx, `SELECT state::text FROM work_items WHERE id = $1`, item).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "blocked" {
		t.Errorf("state = %s, want blocked", state)
	}
	rows, err := pool.Query(ctx, `SELECT payload FROM outbox_events WHERE topic = $1 AND key = $2`, bus.TopicWorkBlocked, item.String())
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			t.Fatal(err)
		}
		var p guards.WorkBlockedPayload
		if err := json.Unmarshal(raw, &p); err != nil {
			t.Fatal(err)
		}
		if p.Kind != guards.BlockKindCodexUngrounded {
			t.Errorf("work.blocked kind = %q, want %q (a refusal must raise the Re-index escalation)", p.Kind, guards.BlockKindCodexUngrounded)
		}
		n++
	}
	if n != 1 {
		t.Errorf("%d work.blocked events, want 1: a refusal must escalate, not only change state", n)
	}
	r, ok, err := guards.LoadRefusal(ctx, pool, item)
	if err != nil || !ok {
		t.Fatalf("no refusal recorded (ok=%v err=%v)", ok, err)
	}
	if r.Stage != stage || r.RerunTopic != rerunTopic || r.RerunState != rerunState {
		t.Errorf("refusal = stage %q topic %q state %q; want %q %q %q", r.Stage, r.RerunTopic, r.RerunState, stage, rerunTopic, rerunState)
	}
	return r
}
