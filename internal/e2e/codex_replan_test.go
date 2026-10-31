package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/codex"
	"github.com/autonomousengine/autonomous-engine/internal/models"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TestE2ECodexReplanMidImplementation is codex coherence Part 1 end to end:
// a codex change to a module a parent was planned against, indexed while
// the parent is in implementation, sends the parent back to planning with
// its in-progress subtask blocked, and the planner plans again.
//
// The fixture writes the codex version the way an incremental index does
// (codex.RecordVersion: the codex_versions row and its codex.indexed in the
// outbox, delivered by any running service's dispatcher). Everything after
// that is the live stack: status-orchestrator's codex.indexed consumer, its
// debounce table and sweep, and implementation-planner on the re-emitted
// technical-brief.ready.
//
// Required environment (the test skips without all three):
//
//	TEST_POSTGRES_DSN      the database the services under test read
//	TEST_KAFKA_BROKERS     comma-separated brokers (only checked, unused)
//	E2E_CODEX_REPLAN       true — status-orchestrator must run with a short
//	                       CODEX_REPLAN_DEBOUNCE (e.g. 5s) and
//	                       CODEX_REPLAN_TICK (e.g. 2s); with the 10m default
//	                       the wait below times out
//
// Optional:
//
//	E2E_CODEX_REPLAN_WAIT  how long to wait for each step (default 2m)
//	E2E_CODEX_REPLAN_PLAN  true — also wait for a new subtask from the
//	                       re-plan (needs a planner model, local ollama
//	                       only); otherwise the planner picking the brief up
//	                       (its assignment) is enough, since a fixture product
//	                       has no onboarding or grounded codex and the
//	                       planner then escalates instead of planning
func TestE2ECodexReplanMidImplementation(t *testing.T) {
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" || os.Getenv("TEST_KAFKA_BROKERS") == "" {
		t.Skip("TEST_POSTGRES_DSN and TEST_KAFKA_BROKERS must both be set — skipping end-to-end codex re-plan fixture")
	}
	if on, _ := strconv.ParseBool(os.Getenv("E2E_CODEX_REPLAN")); !on {
		t.Skip("E2E_CODEX_REPLAN is not true — status-orchestrator must run with a short CODEX_REPLAN_DEBOUNCE for this fixture")
	}
	wait := 2 * time.Minute
	if d, err := time.ParseDuration(os.Getenv("E2E_CODEX_REPLAN_WAIT")); err == nil && d > 0 {
		wait = d
	}
	expectPlan, _ := strconv.ParseBool(os.Getenv("E2E_CODEX_REPLAN_PLAN"))

	ctx := context.Background()
	db, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	const prefix = "e2e-codex-replan-fixture"
	sweep := func() {
		c := context.Background()
		like := prefix + "%"
		_, _ = db.Exec(c, `UPDATE work_items SET awaiting_prompt_id = NULL WHERE product_id IN (SELECT id FROM products WHERE name LIKE $1)`, like)
		_, _ = db.Exec(c, `DELETE FROM user_interactions WHERE product_id IN (SELECT id FROM products WHERE name LIKE $1)`, like)
		_, _ = db.Exec(c, `DELETE FROM work_items WHERE product_id IN (SELECT id FROM products WHERE name LIKE $1) AND parent_id IS NOT NULL`, like)
		_, _ = db.Exec(c, `DELETE FROM work_items WHERE product_id IN (SELECT id FROM products WHERE name LIKE $1)`, like)
		_, _ = db.Exec(c, `DELETE FROM products WHERE name LIKE $1`, like)
	}
	sweep()
	t.Cleanup(sweep)

	product, repo, parent, sub := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	module := prefix + "-module"
	mustExec(t, db, ctx, `INSERT INTO products (id, name) VALUES ($1, $2)`, product, prefix+"-"+product.String()[:8])
	mustExec(t, db, ctx, `INSERT INTO repositories (id, product_id, name, github_url, base_branch) VALUES ($1, $2, $3, $4, 'main')`,
		repo, product, prefix+"-repo", "https://github.com/example/"+prefix+"-"+product.String()[:8])
	cx := codex.New(db)
	record := func(sections ...string) codex.RecordedVersion {
		t.Helper()
		v, err := cx.RecordVersion(ctx, codex.NewVersion{ProductID: product, Branch: "main", SectionsChanged: sections,
			Summary: prefix, Source: models.CodexChangeSourceAutomatedReanalysis, Trigger: codex.TriggerIncrementalIndex})
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	v1 := record(codex.ModuleChange(module))
	mustExec(t, db, ctx, `INSERT INTO work_items (id, type, state, title, description, priority, product_id, repository_id, branch) VALUES ($1, 'feature', 'implementation', $2, 'codex re-plan fixture parent', 'medium', $3, $4, 'main')`,
		parent, prefix+" parent", product, repo)
	mustExec(t, db, ctx, `INSERT INTO work_items (id, type, state, title, description, priority, parent_id, product_id, repository_id) VALUES ($1, 'subtask', 'implementation', $2, 'codex re-plan fixture subtask', 'medium', $3, $4, $5)`,
		sub, prefix+" subtask", parent, product, repo)
	for _, id := range []uuid.UUID{parent, sub} {
		mustExec(t, db, ctx, `INSERT INTO work_item_codex_pins (work_item_id, product_id, branch, product_version, modules) VALUES ($1, $2, 'main', $3, $4)`,
			id, product, v1.ProductVersion, []string{module})
	}
	brief, _ := json.Marshal(map[string]any{"narrative": prefix + " brief", "brief": prefix + " brief", "affected_modules": []string{module}})
	mustExec(t, db, ctx, `INSERT INTO work_item_artifacts (id, work_item_id, artifact_type, content, created_at, agent_id) VALUES ($1, $2, 'technical_brief', $3, now(), 'research-coordinator')`,
		uuid.New(), parent, brief)

	// The codex changes under the running implementation.
	changed := record(codex.ModuleChange(module))
	t.Logf("fixture: product=%s parent=%s subtask=%s pinned v%d, changed v%d", product, parent, sub, v1.ProductVersion, changed.ProductVersion)
	replannedAt := time.Time{}

	poll := func(what string, cond func() (bool, string)) {
		t.Helper()
		deadline := time.Now().Add(wait)
		for {
			ok, detail := cond()
			if ok {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("timed out after %s waiting for %s (last: %s)", wait, what, detail)
			}
			time.Sleep(time.Second)
		}
	}
	state := func(id uuid.UUID) string {
		var s string
		_ = db.QueryRow(ctx, `SELECT state::text FROM work_items WHERE id = $1`, id).Scan(&s)
		return s
	}
	poll("the parent's codex re-plan", func() (bool, string) {
		var reason string
		err := db.QueryRow(ctx, `SELECT reason, created_at FROM work_item_codex_events WHERE work_item_id = $1 AND kind = 'replanned'`, parent).Scan(&reason, &replannedAt)
		return err == nil, fmt.Sprintf("parent %s, subtask %s, err %v", state(parent), state(sub), err)
	})
	want := fmt.Sprintf("codex_changed: modules %s (v%d → v%d)", module, v1.ProductVersion, changed.ProductVersion)
	var reason string
	if err := db.QueryRow(ctx, `SELECT reason FROM work_item_transitions WHERE work_item_id = $1 AND from_state = 'implementation' AND to_state = 'planning'`, parent).Scan(&reason); err != nil || reason != want {
		t.Fatalf("parent implementation→planning reason = %q (%v), want %q", reason, err, want)
	}
	if s := state(sub); s != "blocked" {
		t.Fatalf("subtask = %s, want blocked", s)
	}

	// The planner takes the re-emitted brief.
	poll("implementation-planner on the re-plan's technical-brief.ready", func() (bool, string) {
		var n int
		_ = db.QueryRow(ctx, `SELECT count(*) FROM agent_assignments WHERE work_item_id = $1 AND agent_type = 'implementation_planner' AND assigned_at >= $2`, parent, replannedAt.Add(-time.Second)).Scan(&n)
		return n > 0, fmt.Sprintf("parent %s", state(parent))
	})
	if expectPlan {
		poll("a new subtask from the re-plan", func() (bool, string) {
			var n int
			_ = db.QueryRow(ctx, `SELECT count(*) FROM work_items WHERE parent_id = $1 AND id <> $2 AND created_at >= $3`, parent, sub, replannedAt).Scan(&n)
			return n > 0, fmt.Sprintf("parent %s", state(parent))
		})
	}
}
