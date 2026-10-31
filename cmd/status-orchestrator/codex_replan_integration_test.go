package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/agentregistry"
	"github.com/autonomousengine/autonomous-engine/internal/bus"
	"github.com/autonomousengine/autonomous-engine/internal/codex"
	"github.com/autonomousengine/autonomous-engine/internal/idempotency"
	"github.com/autonomousengine/autonomous-engine/internal/models"
	"github.com/autonomousengine/autonomous-engine/internal/workgraph"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Codex coherence Part 1 against a real PostgreSQL (TEST_POSTGRES_DSN,
// migrated): codex.indexed → durable debounce → sweep → re-plan, note or
// escalation, and the escalation's three answers.

func testReplanConfig() replanConfig {
	return replanConfig{Debounce: 10 * time.Minute, Max: 2, Lease: time.Minute, TokenBudget: 500_000, MinPlanTokens: 50_000}
}

func newTestReplanner(pool *pgxpool.Pool, cfg replanConfig) *codexReplanner {
	return newCodexReplanner(pool, workgraph.New(pool), nopBus{}, agentregistry.New(pool), cfg)
}

// codexHandler is the guarded codex.indexed consumer main subscribes.
func codexHandler(pool *pgxpool.Pool, r *codexReplanner) bus.Handler {
	return idempotency.NewLedger(pool).Handler(codexReplanConsumer, r.handleCodexIndexed)
}

type replanFixture struct {
	t       *testing.T
	pool    *pgxpool.Pool
	product uuid.UUID
}

// newReplanFixture creates a product (cleaned up with everything under it).
func newReplanFixture(t *testing.T, pool *pgxpool.Pool) *replanFixture {
	t.Helper()
	ctx := context.Background()
	pid := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO products (id, name) VALUES ($1, $2)`, pid, "p1-replan-"+pid.String()[:8]); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c := context.Background()
		for _, sql := range []string{
			`DELETE FROM outbox_events WHERE aggregate_id IN (SELECT id FROM work_items WHERE product_id = $1) OR aggregate_id = $1`,
			`DELETE FROM inference_logs WHERE work_item_id IN (SELECT id FROM work_items WHERE product_id = $1)`,
			`UPDATE work_items SET awaiting_prompt_id = NULL WHERE product_id = $1`,
			`DELETE FROM work_item_codex_events WHERE product_id = $1`,
			`DELETE FROM user_interactions WHERE product_id = $1`,
			`DELETE FROM pending_pr_changes WHERE work_item_id IN (SELECT id FROM work_items WHERE product_id = $1)`,
			`DELETE FROM work_items WHERE product_id = $1 AND parent_id IS NOT NULL`,
			`DELETE FROM work_items WHERE product_id = $1`,
			`DELETE FROM products WHERE id = $1`,
		} {
			if _, err := pool.Exec(c, sql, pid); err != nil {
				t.Logf("cleanup %q: %v", sql, err)
			}
		}
	})
	return &replanFixture{t: t, pool: pool, product: pid}
}

// parent creates a pinned parent in state with one subtask per subState
// (each with the parent's pin), and a technical brief.
func (f *replanFixture) parent(state models.WorkItemState, branch string, version int, modules []string, subStates ...models.WorkItemState) (uuid.UUID, []uuid.UUID) {
	f.t.Helper()
	ctx := context.Background()
	id := uuid.New()
	if _, err := f.pool.Exec(ctx, `INSERT INTO work_items (id, type, state, title, description, product_id, branch) VALUES ($1, 'feature', $2, 'replan parent', 'd', $3, $4)`,
		id, string(state), f.product, branch); err != nil {
		f.t.Fatal(err)
	}
	f.pin(id, branch, version, modules)
	brief, _ := json.Marshal(map[string]any{"narrative": "the brief prose", "brief": "the brief prose", "affected_modules": modules})
	if _, err := f.pool.Exec(ctx, `INSERT INTO work_item_artifacts (id, work_item_id, artifact_type, content, created_at, agent_id) VALUES ($1, $2, 'technical_brief', $3, now(), 'research-coordinator')`,
		uuid.New(), id, brief); err != nil {
		f.t.Fatal(err)
	}
	var subs []uuid.UUID
	for _, st := range subStates {
		sub := uuid.New()
		if _, err := f.pool.Exec(ctx, `INSERT INTO work_items (id, type, state, title, description, parent_id, product_id) VALUES ($1, 'subtask', $2, 'replan subtask', 'd', $3, $4)`,
			sub, string(st), id, f.product); err != nil {
			f.t.Fatal(err)
		}
		if _, err := f.pool.Exec(ctx, `INSERT INTO plan_tasks (id, parent_work_id, title, status, message) VALUES ($1, $2, 'replan subtask', 'released', '{}')`, sub, id); err != nil {
			f.t.Fatal(err)
		}
		f.pin(sub, branch, version, modules)
		subs = append(subs, sub)
	}
	return id, subs
}

func (f *replanFixture) pin(id uuid.UUID, branch string, version int, modules []string) {
	f.t.Helper()
	if _, err := f.pool.Exec(context.Background(), `INSERT INTO work_item_codex_pins (work_item_id, product_id, branch, product_version, modules) VALUES ($1, $2, $3, $4, $5)`,
		id, f.product, branch, version, modules); err != nil {
		f.t.Fatal(err)
	}
}

// version records a codex version on branch and returns its codex.indexed
// as the bus delivers it (the outbox row's payload and event id).
func (f *replanFixture) version(branch string, sections ...string) *bus.Message {
	f.t.Helper()
	ctx := context.Background()
	rec, err := codex.New(f.pool).RecordVersion(ctx, codex.NewVersion{
		ProductID: f.product, Branch: branch, SectionsChanged: sections,
		Summary: "test", Source: models.CodexChangeSourceAutomatedReanalysis, Trigger: codex.TriggerIncrementalIndex,
	})
	if err != nil {
		f.t.Fatal(err)
	}
	var id uuid.UUID
	var payload []byte
	if err := f.pool.QueryRow(ctx, `SELECT id, payload FROM outbox_events WHERE topic = $1 AND aggregate_id = $2 AND payload->>'product_version' = $3`,
		bus.TopicCodexIndexed, f.product, itoa(rec.ProductVersion)).Scan(&id, &payload); err != nil {
		f.t.Fatal(err)
	}
	msg := &bus.Message{Topic: bus.TopicCodexIndexed, Key: f.product.String(), Value: payload, Headers: map[string]string{bus.HeaderEventID: id.String()}}
	f.t.Cleanup(func() {
		_, _ = f.pool.Exec(context.Background(), `DELETE FROM processed_events WHERE event_id = $1`, id)
	})
	return msg
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

// due ends the product's quiet window now.
func (f *replanFixture) due() {
	f.t.Helper()
	if _, err := f.pool.Exec(context.Background(), `UPDATE codex_replan_pending SET due_at = now() - interval '1 second' WHERE product_id = $1`, f.product); err != nil {
		f.t.Fatal(err)
	}
}

func (f *replanFixture) sweep(r *codexReplanner) int {
	f.t.Helper()
	n, err := r.processDue(context.Background(), f.product)
	if err != nil {
		f.t.Fatalf("processDue: %v", err)
	}
	return n
}

func (f *replanFixture) count(sql string, args ...any) int {
	f.t.Helper()
	return count(f.t, f.pool, sql, args...)
}

func (f *replanFixture) briefs(parent uuid.UUID) int {
	return f.count(`SELECT count(*) FROM outbox_events WHERE topic = 'technical-brief.ready' AND aggregate_id = $1`, parent)
}

func (f *replanFixture) events(id uuid.UUID, kind codex.ItemEventKind) int {
	return f.count(`SELECT count(*) FROM work_item_codex_events WHERE work_item_id = $1 AND kind = $2`, id, string(kind))
}

// Only parents pinned to the same product and branch whose modules the
// change touched are re-planned: the in-progress subtask is blocked
// (codex_changed), the parent goes back to planning with the change as its
// reason, the activity entry is written and the planner's input event
// (technical-brief.ready, carrying the existing brief) is enqueued.
func TestIntegration_CodexReplan_OnlyIntersectingModulesOnTheSameProductAndBranch(t *testing.T) {
	pool := integrationPool(t)
	f := newReplanFixture(t, pool)
	other := newReplanFixture(t, pool)
	r := newTestReplanner(pool, testReplanConfig())
	h := codexHandler(pool, r)

	f.version("main", "module:auth") // v1: the version everything is pinned to
	other.version("main", "module:auth")
	touched, subs := f.parent(models.WorkItemStateImplementation, "main", 1, []string{"auth", "billing"},
		models.WorkItemStateImplementation, models.WorkItemStatePassed)
	untouched, untouchedSubs := f.parent(models.WorkItemStateImplementation, "main", 1, []string{"search"}, models.WorkItemStateImplementation)
	otherBranch, _ := f.parent(models.WorkItemStateImplementation, "release", 0, []string{"billing"})
	otherProduct, _ := other.parent(models.WorkItemStateImplementation, "main", 1, []string{"billing"})

	msg := f.version("main", "module:billing", "module:payments") // v2
	replay(t, h, msg, 3)
	f.due()
	if n := f.sweep(r); n != 1 {
		t.Fatalf("swept %d, want 1", n)
	}

	if s := itemState(t, pool, touched); s != "planning" {
		t.Fatalf("touched parent = %s, want planning", s)
	}
	if r := latestReason(t, pool, touched, "planning"); r != "codex_changed: modules billing (v1 → v2)" {
		t.Errorf("re-plan reason = %q", r)
	}
	if s := itemState(t, pool, subs[0]); s != "blocked" {
		t.Errorf("in-progress subtask = %s, want blocked", s)
	}
	if r := latestReason(t, pool, subs[0], "blocked"); !strings.HasPrefix(r, "codex_changed") {
		t.Errorf("subtask block reason = %q", r)
	}
	if s := itemState(t, pool, subs[1]); s != "passed" {
		t.Errorf("passed subtask = %s, want passed (untouched)", s)
	}
	if n := f.count(`SELECT count(*) FROM plan_tasks WHERE id = $1 AND status = 'failed'`, subs[0]); n != 1 {
		t.Errorf("the blocked subtask's plan task was not closed")
	}
	if n := f.briefs(touched); n != 1 {
		t.Fatalf("technical-brief.ready events = %d, want 1", n)
	}
	var payload []byte
	if err := pool.QueryRow(context.Background(), `SELECT payload FROM outbox_events WHERE topic = 'technical-brief.ready' AND aggregate_id = $1`, touched).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	var brief map[string]any
	_ = json.Unmarshal(payload, &brief)
	if brief["work_id"] != touched.String() || brief["product_id"] != f.product.String() || brief["brief_content"] != "the brief prose" || brief["replan"] != true {
		t.Errorf("brief payload = %v", brief)
	}
	if n := f.events(touched, codex.EventReplanned); n != 1 {
		t.Errorf("replanned events = %d, want 1", n)
	}
	evs, _ := codex.ListItemEvents(context.Background(), pool, touched)
	if len(evs) != 1 || evs[0].FromVersion != 1 || evs[0].ToVersion != 2 || strings.Join(evs[0].Modules, ",") != "billing" || !evs[0].Automatic {
		t.Errorf("activity entry = %+v", evs)
	}

	for name, id := range map[string]uuid.UUID{"other modules": untouched, "other branch": otherBranch, "other product": otherProduct, "other modules' subtask": untouchedSubs[0]} {
		if s := itemState(t, pool, id); s != "implementation" {
			t.Errorf("%s: state = %s, want implementation", name, s)
		}
		if n := f.count(`SELECT count(*) FROM work_item_codex_events WHERE work_item_id = $1`, id); n != 0 {
			t.Errorf("%s: %d codex events, want 0", name, n)
		}
	}

	// A second sweep has nothing left to do: the change was acted on.
	replay(t, h, f.version("main", "module:unrelated"), 1)
	f.due()
	f.sweep(r)
	if n := f.events(touched, codex.EventReplanned); n != 1 {
		t.Errorf("replanned events after an unrelated change = %d, want 1", n)
	}
}

// Three codex.indexed events within the quiet window make one re-plan.
func TestIntegration_CodexReplan_DebounceCollapsesThreeEventsIntoOneReplan(t *testing.T) {
	pool := integrationPool(t)
	f := newReplanFixture(t, pool)
	r := newTestReplanner(pool, testReplanConfig())
	h := codexHandler(pool, r)
	f.version("main", "module:a")
	parent, _ := f.parent(models.WorkItemStateImplementation, "main", 1, []string{"a"}, models.WorkItemStateImplementation)

	for i := 0; i < 3; i++ {
		replay(t, h, f.version("main", "module:a"), 1)
	}
	if n := f.sweep(r); n != 0 {
		t.Fatalf("swept %d inside the quiet window, want 0", n)
	}
	if s := itemState(t, pool, parent); s != "implementation" {
		t.Fatalf("parent = %s before the quiet window ended", s)
	}
	f.due()
	if n := f.sweep(r); n != 1 {
		t.Fatalf("swept %d, want 1", n)
	}
	if n := f.events(parent, codex.EventReplanned); n != 1 {
		t.Fatalf("re-plans = %d, want 1", n)
	}
	if n := transitionsTo(t, pool, parent, "implementation", "planning"); n != 1 {
		t.Fatalf("implementation→planning transitions = %d, want 1", n)
	}
	if n := f.briefs(parent); n != 1 {
		t.Fatalf("technical-brief.ready events = %d, want 1", n)
	}
	if r := latestReason(t, pool, parent, "planning"); r != "codex_changed: modules a (v1 → v4)" {
		t.Errorf("reason = %q", r)
	}
}

// A process restarted during the quiet window (a new replanner with no
// memory) still re-plans when the window ends.
func TestIntegration_CodexReplan_RestartDuringDebounceStillReplans(t *testing.T) {
	pool := integrationPool(t)
	f := newReplanFixture(t, pool)
	f.version("main", "module:a")
	parent, _ := f.parent(models.WorkItemStatePlanning, "main", 1, []string{"a"})

	first := newTestReplanner(pool, testReplanConfig())
	replay(t, codexHandler(pool, first), f.version("main", "module:a"), 1)
	// "Restart": the first replanner is gone; a new one starts.
	restarted := newTestReplanner(pool, testReplanConfig())
	f.due()
	if n := f.sweep(restarted); n != 1 {
		t.Fatalf("swept %d after the restart, want 1", n)
	}
	if n := transitionsTo(t, pool, parent, "planning", "planning"); n != 1 {
		t.Fatalf("planning re-entries = %d, want 1", n)
	}
	if n := f.briefs(parent); n != 1 {
		t.Fatalf("technical-brief.ready events = %d, want 1", n)
	}
}

// A plan-relevant section key re-plans any pinned item, whatever modules it
// relies on; an unrelated section does not.
func TestIntegration_CodexReplan_SectionKeyTrigger(t *testing.T) {
	pool := integrationPool(t)
	f := newReplanFixture(t, pool)
	r := newTestReplanner(pool, testReplanConfig())
	h := codexHandler(pool, r)
	f.version("main", "module:a")
	parent, _ := f.parent(models.WorkItemStateImplementation, "main", 1, []string{"a"})

	replay(t, h, f.version("main", "section:codebase_architecture"), 1)
	f.due()
	f.sweep(r)
	if s := itemState(t, pool, parent); s != "implementation" {
		t.Fatalf("an unrelated section moved the parent to %s", s)
	}
	replay(t, h, f.version("main", "section:review_triggers"), 1)
	f.due()
	f.sweep(r)
	if s := itemState(t, pool, parent); s != "planning" {
		t.Fatalf("parent = %s, want planning", s)
	}
	if r := latestReason(t, pool, parent, "planning"); r != "codex_changed: sections review_triggers (v1 → v3)" {
		t.Errorf("reason = %q", r)
	}
}

// Items past planning — in verification, or with an open PR or pending
// change — are not touched; the change is noted on them instead, once.
func TestIntegration_CodexReplan_VerificationAndOpenPRAreOnlyNoted(t *testing.T) {
	pool := integrationPool(t)
	f := newReplanFixture(t, pool)
	r := newTestReplanner(pool, testReplanConfig())
	h := codexHandler(pool, r)
	f.version("main", "module:a")
	verifying, vsubs := f.parent(models.WorkItemStateVerification, "main", 1, []string{"a"}, models.WorkItemStatePassed)
	withPR, prSubs := f.parent(models.WorkItemStateImplementation, "main", 1, []string{"a"}, models.WorkItemStateImplementation)
	if _, err := pool.Exec(context.Background(), `INSERT INTO outbox_events (aggregate_type, aggregate_id, topic, key, payload) VALUES ('work_item', $1, 'pr.ready', $2, $3)`,
		withPR, withPR.String(), `{"pr_url":"https://example.invalid/pr/1","branch":"agent/x"}`); err != nil {
		t.Fatal(err)
	}
	pending, _ := f.parent(models.WorkItemStateImplementation, "main", 1, []string{"a"})
	if _, err := pool.Exec(context.Background(), `INSERT INTO pending_pr_changes (work_item_id, repository_id, branch) VALUES ($1, $2, 'agent/y')`, pending, uuid.New()); err != nil {
		t.Fatal(err)
	}

	replay(t, h, f.version("main", "module:a"), 1)
	f.due()
	f.sweep(r)
	for name, tc := range map[string]struct {
		id    uuid.UUID
		state string
	}{
		"verification":         {verifying, "verification"},
		"verification subtask": {vsubs[0], "passed"},
		"open PR":              {withPR, "implementation"},
		"open PR subtask":      {prSubs[0], "implementation"},
		"pending change":       {pending, "implementation"},
	} {
		if s := itemState(t, pool, tc.id); s != tc.state {
			t.Errorf("%s: state = %s, want %s", name, s, tc.state)
		}
		if n := f.briefs(tc.id); n != 0 {
			t.Errorf("%s: %d technical-brief.ready events", name, n)
		}
	}
	for name, id := range map[string]uuid.UUID{"verification": verifying, "open PR": withPR, "pending change": pending} {
		if n := f.events(id, codex.EventNoted); n != 1 {
			t.Errorf("%s: noted events = %d, want 1", name, n)
		}
	}
	// The same change (an intersecting module) is not noted twice: a second
	// sweep at the same version adds nothing.
	if err := r.sweep(context.Background(), codex.PendingSweep{ProductID: f.product, Branch: "main"}); err != nil {
		t.Fatal(err)
	}
	if n := f.events(verifying, codex.EventNoted); n != 1 {
		t.Errorf("noted events after a second sweep of the same change = %d, want 1", n)
	}
	// A further change to the pinned module is a new note.
	replay(t, codexHandler(pool, r), f.version("main", "module:a"), 1)
	f.due()
	f.sweep(r)
	if n := f.events(verifying, codex.EventNoted); n != 2 {
		t.Errorf("noted events after a new change = %d, want 2", n)
	}
}

// The cap reached: an escalation (Re-plan / Continue as is / Cancel) is
// raised and recorded, and nothing is re-planned.
func TestIntegration_CodexReplan_CapReachedEscalatesInsteadOfReplanning(t *testing.T) {
	pool := integrationPool(t)
	f := newReplanFixture(t, pool)
	cfg := testReplanConfig()
	r := newTestReplanner(pool, cfg)
	h := codexHandler(pool, r)
	f.version("main", "module:a")
	parent, subs := f.parent(models.WorkItemStateImplementation, "main", 1, []string{"a"}, models.WorkItemStateImplementation)
	for i := 0; i < cfg.Max; i++ {
		if _, err := pool.Exec(context.Background(), `INSERT INTO work_item_codex_events (work_item_id, product_id, branch, kind, automatic, from_version, to_version) VALUES ($1, $2, 'main', 'replanned', true, 0, 1)`, parent, f.product); err != nil {
			t.Fatal(err)
		}
	}

	replay(t, h, f.version("main", "module:a"), 1)
	f.due()
	f.sweep(r)
	if s := itemState(t, pool, parent); s != "implementation" {
		t.Fatalf("parent = %s, want implementation (no re-plan)", s)
	}
	if s := itemState(t, pool, subs[0]); s != "implementation" {
		t.Fatalf("subtask = %s, want implementation", s)
	}
	if n := f.briefs(parent); n != 0 {
		t.Fatalf("technical-brief.ready events = %d, want 0", n)
	}
	if n := f.events(parent, codex.EventEscalated); n != 1 {
		t.Fatalf("escalated events = %d, want 1", n)
	}
	var trigger, ptype, question string
	var actions []byte
	if err := pool.QueryRow(context.Background(), `SELECT trigger, prompt_type, question, actions FROM user_interactions WHERE work_item_id = $1`, parent).Scan(&trigger, &ptype, &question, &actions); err != nil {
		t.Fatalf("escalation prompt: %v", err)
	}
	if trigger != "codex:replan" || ptype != "escalation" || !strings.Contains(question, "modules a (v1 → v2)") || !strings.Contains(question, "re-planned automatically 2 time(s)") {
		t.Errorf("prompt = %s/%s %q", trigger, ptype, question)
	}
	for _, id := range []string{`"replan"`, `"continue"`, `"cancel"`} {
		if !strings.Contains(string(actions), id) {
			t.Errorf("prompt actions %s lack %s", actions, id)
		}
	}
	if n := f.count(`SELECT count(*) FROM outbox_events WHERE topic = 'user_prompt.requested' AND aggregate_id = $1`, parent); n != 1 {
		t.Errorf("user_prompt.requested events = %d, want 1", n)
	}
	// No second escalation for the same change.
	replay(t, h, f.version("main", "module:other"), 1)
	f.due()
	f.sweep(r)
	if n := f.events(parent, codex.EventEscalated); n != 1 {
		t.Errorf("escalated events after a second sweep = %d, want 1", n)
	}
}

// An item without budget left for a plan escalates; the budget is never
// raised.
func TestIntegration_CodexReplan_BudgetExhaustedEscalates(t *testing.T) {
	pool := integrationPool(t)
	f := newReplanFixture(t, pool)
	cfg := testReplanConfig()
	cfg.TokenBudget, cfg.MinPlanTokens = 1000, 200
	r := newTestReplanner(pool, cfg)
	h := codexHandler(pool, r)
	f.version("main", "module:a")
	parent, _ := f.parent(models.WorkItemStatePlanning, "main", 1, []string{"a"})
	if _, err := pool.Exec(context.Background(), `INSERT INTO inference_logs (model_provider, model_id, intent_type, input_tokens, output_tokens, work_item_id) VALUES ('ollama', 'm', 'plan', 600, 250, $1)`, parent); err != nil {
		t.Fatal(err)
	}
	replay(t, h, f.version("main", "module:a"), 1)
	f.due()
	f.sweep(r)
	if n := f.briefs(parent); n != 0 {
		t.Fatalf("re-planned with 150 of 1000 tokens left")
	}
	if n := f.events(parent, codex.EventEscalated); n != 1 {
		t.Fatalf("escalated events = %d, want 1", n)
	}
	var question string
	if err := pool.QueryRow(context.Background(), `SELECT question FROM user_interactions WHERE work_item_id = $1`, parent).Scan(&question); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(question, "150 of its 1000-token budget") {
		t.Errorf("question = %q", question)
	}
}

// escalated raises the cap escalation for a fresh parent and parks it on
// the prompt the way user_prompt.requested does; it returns the prompt.
func escalated(t *testing.T, f *replanFixture, r *codexReplanner, state models.WorkItemState) (parent, sub, prompt uuid.UUID) {
	t.Helper()
	cfg := r.cfg
	f.version("main", "module:a")
	parent, subs := f.parent(state, "main", 1, []string{"a"}, models.WorkItemStateImplementation)
	for i := 0; i < cfg.Max; i++ {
		if _, err := f.pool.Exec(context.Background(), `INSERT INTO work_item_codex_events (work_item_id, product_id, branch, kind, automatic, from_version, to_version) VALUES ($1, $2, 'main', 'replanned', true, 0, 1)`, parent, f.product); err != nil {
			t.Fatal(err)
		}
	}
	replay(t, codexHandler(f.pool, r), f.version("main", "module:a"), 1)
	f.due()
	f.sweep(r)
	if err := f.pool.QueryRow(context.Background(), `SELECT id FROM user_interactions WHERE work_item_id = $1 AND trigger = 'codex:replan'`, parent).Scan(&prompt); err != nil {
		t.Fatalf("escalation prompt: %v", err)
	}
	if _, _, err := parkOnPrompt(context.Background(), f.pool, parent, prompt); err != nil {
		t.Fatal(err)
	}
	return parent, subs[0], prompt
}

func answer(t *testing.T, f *replanFixture, parent, prompt uuid.UUID, action string) {
	t.Helper()
	if _, err := f.pool.Exec(context.Background(), `UPDATE user_interactions SET state = 'answered', action_chosen = $2, answered_at = now() WHERE id = $1`, prompt, action); err != nil {
		t.Fatal(err)
	}
	msg := eventMessage(t, bus.TopicUserPromptAnswered, map[string]any{"prompt_id": prompt, "work_item_id": parent, "action_chosen": action})
	cleanupLedgerRow(t, f.pool, msg)
	replay(t, guardedOrchestrator(t, f.pool), msg, 2)
}

func TestIntegration_CodexReplan_EscalationAnswers(t *testing.T) {
	pool := integrationPool(t)

	t.Run("re-plan", func(t *testing.T) {
		f := newReplanFixture(t, pool)
		r := newTestReplanner(pool, testReplanConfig())
		parent, sub, prompt := escalated(t, f, r, models.WorkItemStateImplementation)
		answer(t, f, parent, prompt, "replan")
		if s := itemState(t, pool, parent); s != "planning" {
			t.Fatalf("parent = %s, want planning", s)
		}
		if s := itemState(t, pool, sub); s != "blocked" {
			t.Errorf("subtask = %s, want blocked", s)
		}
		if n := f.briefs(parent); n != 1 {
			t.Errorf("technical-brief.ready events = %d, want 1", n)
		}
		if n := f.count(`SELECT count(*) FROM work_item_codex_events WHERE work_item_id = $1 AND kind = 'replanned' AND NOT automatic`, parent); n != 1 {
			t.Errorf("operator re-plans = %d, want 1", n)
		}
	})

	t.Run("continue as is", func(t *testing.T) {
		f := newReplanFixture(t, pool)
		r := newTestReplanner(pool, testReplanConfig())
		parent, sub, prompt := escalated(t, f, r, models.WorkItemStateImplementation)
		answer(t, f, parent, prompt, "continue")
		if s := itemState(t, pool, parent); s != "implementation" {
			t.Fatalf("parent = %s, want implementation", s)
		}
		if s := itemState(t, pool, sub); s != "implementation" {
			t.Errorf("subtask = %s, want implementation", s)
		}
		if n := f.briefs(parent); n != 0 {
			t.Errorf("technical-brief.ready events = %d, want 0", n)
		}
		if n := f.events(parent, codex.EventContinued); n != 1 {
			t.Errorf("continued events = %d, want 1", n)
		}
		// The acknowledged change does not escalate again.
		replay(t, codexHandler(pool, r), f.version("main", "module:unrelated"), 1)
		f.due()
		f.sweep(r)
		if n := f.events(parent, codex.EventEscalated); n != 1 {
			t.Errorf("escalations after continue = %d, want 1", n)
		}
	})

	t.Run("cancel", func(t *testing.T) {
		f := newReplanFixture(t, pool)
		r := newTestReplanner(pool, testReplanConfig())
		parent, _, prompt := escalated(t, f, r, models.WorkItemStatePlanning)
		answer(t, f, parent, prompt, "cancel")
		if s := itemState(t, pool, parent); s != "cancelled" {
			t.Fatalf("parent = %s, want cancelled", s)
		}
	})
}
