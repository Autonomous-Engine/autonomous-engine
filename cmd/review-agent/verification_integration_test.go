package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/agentregistry"
	"github.com/autonomousengine/autonomous-engine/internal/bus"
	"github.com/autonomousengine/autonomous-engine/internal/evidence"
	"github.com/autonomousengine/autonomous-engine/internal/idempotency"
	"github.com/autonomousengine/autonomous-engine/internal/products"
	"github.com/autonomousengine/autonomous-engine/internal/verifier"
	"github.com/autonomousengine/autonomous-engine/internal/verifier/stage"
	"github.com/autonomousengine/autonomous-engine/internal/workgraph"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// R14 V1 against a real PostgreSQL (TEST_POSTGRES_DSN, migrated): the
// verification stage's handler, wrapped by the ledger exactly as main
// subscribes it, commits its record, the verification.complete event and the
// guard row together, and a replayed delivery records nothing more.

type stageFixture struct {
	product, repo, parent, subtask uuid.UUID
	head                           string
}

func seedStageFixture(t *testing.T, pool *pgxpool.Pool) stageFixture {
	t.Helper()
	ctx := context.Background()
	f := stageFixture{product: uuid.New(), repo: uuid.New(), parent: uuid.New(), subtask: uuid.New(), head: strings.Repeat("d", 40)}
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	t.Cleanup(func() {
		for _, q := range []struct {
			sql  string
			args []any
		}{
			{`DELETE FROM outbox_events WHERE aggregate_id = $1`, []any{f.parent}},
			{`DELETE FROM verification_results WHERE work_item_id = $1`, []any{f.parent}},
			{`DELETE FROM work_item_artifacts WHERE work_item_id = $1`, []any{f.parent}},
			{`DELETE FROM agent_assignments WHERE work_item_id = $1 OR work_item_id = $2`, []any{f.parent, f.subtask}},
			{`DELETE FROM work_item_transitions WHERE work_item_id = $1 OR work_item_id = $2`, []any{f.parent, f.subtask}},
			{`DELETE FROM work_items WHERE id = $1`, []any{f.subtask}},
			{`DELETE FROM work_items WHERE id = $1`, []any{f.parent}},
			{`DELETE FROM repositories WHERE id = $1`, []any{f.repo}},
			{`DELETE FROM products WHERE id = $1`, []any{f.product}},
		} {
			if _, err := pool.Exec(context.Background(), q.sql, q.args...); err != nil {
				t.Logf("cleanup %q: %v", q.sql, err)
			}
		}
	})
	exec(`INSERT INTO products (id, name) VALUES ($1, $2)`, f.product, "r14-stage-"+f.product.String()[:8])
	// No clone_path: a verification that gets past reuse must fail setup.
	exec(`INSERT INTO repositories (id, product_id, name, github_url, base_branch) VALUES ($1, $2, 'r14-repo', 'https://github.com/example/r14', 'main')`, f.repo, f.product)
	exec(`INSERT INTO work_items (id, type, state, title, description, product_id, repository_id) VALUES ($1, 'feature', 'implementation', 'R14 stage parent', 'd', $2, $3)`, f.parent, f.product, f.repo)
	exec(`INSERT INTO work_items (id, type, state, title, description, parent_id, product_id, repository_id) VALUES ($1, 'subtask', 'passed', 'R14 stage subtask', 'd', $2, $3, $4)`, f.subtask, f.parent, f.product, f.repo)
	return f
}

func stageMessage(t *testing.T, f stageFixture, outcome evidence.Outcome, success bool) *bus.Message {
	t.Helper()
	ev := stageEvidence(f.parent, f.subtask, f.repo, f.head, outcome)
	b, err := json.Marshal(implMessage(t, f.parent, f.subtask, &ev, success))
	if err != nil {
		t.Fatal(err)
	}
	return &bus.Message{Topic: bus.TopicImplementationComplete, Key: f.parent.String(), Value: b, Headers: map[string]string{bus.HeaderEventID: uuid.NewString()}}
}

func stageHandler(pool *pgxpool.Pool) bus.Handler {
	ctx := context.Background()
	return idempotency.NewLedger(pool).Handler(verificationIdempotencyConsumer, buildVerificationHandler(ctx, verificationDeps{
		pool: pool, wg: workgraph.New(pool), registry: agentregistry.New(pool), kb: nopBus{},
		window: evidence.CompatWindowLegacy, products: products.NewStore(pool),
	}))
}

func countRows(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return n
}

func verificationEvents(t *testing.T, pool *pgxpool.Pool, parent uuid.UUID) []stage.Message {
	t.Helper()
	rows, err := pool.Query(context.Background(), `SELECT payload FROM outbox_events WHERE aggregate_id = $1 AND topic = $2 ORDER BY created_at`, parent, bus.TopicVerificationComplete)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []stage.Message
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			t.Fatal(err)
		}
		var m stage.Message
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatal(err)
		}
		out = append(out, m)
	}
	return out
}

// A failed implementation is not verified: the not_run record, the event
// (carrying the implementation payload) and the guard commit once.
func TestIntegration_VerificationStage_NotRunCommitsOnceWithTheGuard(t *testing.T) {
	pool := integrationPool(t)
	f := seedStageFixture(t, pool)
	h := stageHandler(pool)
	msg := stageMessage(t, f, evidence.OutcomeFailed, false)
	for i := 0; i < 5; i++ {
		if err := h(context.Background(), msg); err != nil {
			t.Fatalf("delivery %d: %v", i+1, err)
		}
	}
	if n := countRows(t, pool, `SELECT count(*) FROM verification_results WHERE work_item_id = $1 AND task_id = $2 AND decision = 'not_run'`, f.parent, f.subtask); n != 1 {
		t.Errorf("not_run results = %d, want 1", n)
	}
	evs := verificationEvents(t, pool, f.parent)
	if len(evs) != 1 || evs[0].Decision != stage.DecisionNotRun || !strings.Contains(evs[0].Reason, "not releasable") {
		t.Fatalf("verification.complete events = %+v", evs)
	}
	if len(evs[0].Implementation) == 0 || !strings.Contains(string(evs[0].Implementation), f.subtask.String()) {
		t.Errorf("the event does not carry the implementation payload: %s", evs[0].Implementation)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM processed_events WHERE consumer = $1 AND event_id = $2`, verificationIdempotencyConsumer, msg.Headers[bus.HeaderEventID]); n != 1 {
		t.Errorf("ledger rows = %d, want 1", n)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM agent_assignments WHERE work_item_id = $1 AND status = 'completed'`, f.subtask); n != 1 {
		t.Errorf("completed stage assignments = %d, want 1 (the handler ran once)", n)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM work_item_artifacts WHERE work_item_id = $1`, f.parent); n != 0 {
		t.Errorf("artifacts = %d, want none for not_run", n)
	}
}

// A verdict already stored for the same head is reused: no new artifact, a
// new record pointing at the stored one, the decision re-derived from its
// gate (second pass on by default).
func TestIntegration_VerificationStage_ReusesTheVerdictForTheSameHead(t *testing.T) {
	pool := integrationPool(t)
	f := seedStageFixture(t, pool)
	ctx := context.Background()
	v := verifier.Verdict{SchemaVersion: 1, Overall: verifier.OverallApprove, Defects: []verifier.Defect{},
		PerCriterion: []verifier.CriterionResult{{ID: "AC-1", Status: verifier.StatusMet, Evidence: []verifier.Ref{"a.go:1"}}}}
	g := verifier.Gate(v)
	art := stage.NewArtifact(f.parent, agentID, stage.NewArtifactContent(v, g, string(stage.DecisionApprove), "agent/x", f.head, f.head, f.subtask.String()))
	if err := workgraph.New(pool).AddArtifact(ctx, art); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO verification_results (id, work_item_id, task_id, head_sha, decision, reason, verdict_artifact_id, verifier_version)
		VALUES ($1, $2, $3, $4, 'approve', $5, $6, $7)`, uuid.New(), f.parent, f.subtask, f.head, g.Reason, art.ID, verifier.Version); err != nil {
		t.Fatal(err)
	}
	t.Setenv(stage.SecondPassEnv, "")
	if err := stageHandler(pool)(ctx, stageMessage(t, f, evidence.OutcomeVerified, true)); err != nil {
		t.Fatal(err)
	}
	evs := verificationEvents(t, pool, f.parent)
	if len(evs) != 1 {
		t.Fatalf("events = %+v", evs)
	}
	m := evs[0]
	if m.Decision != stage.DecisionApprovePendingSecondPass || !m.Reused || m.VerdictArtifactID != art.ID.String() || m.HeadSHA != f.head {
		t.Fatalf("event = %+v, want a reused approve_pending_second_pass for %s naming artifact %s", m, f.head, art.ID)
	}
	if len(m.Criteria) != 1 || m.Criteria[0].Status != "met" {
		t.Errorf("criteria = %+v", m.Criteria)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM work_item_artifacts WHERE work_item_id = $1`, f.parent); n != 1 {
		t.Errorf("artifacts = %d, want the stored one only", n)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM verification_results WHERE work_item_id = $1 AND verdict_artifact_id = $2`, f.parent, art.ID); n != 2 {
		t.Errorf("results naming the stored artifact = %d, want 2", n)
	}
}

// A setup failure (here: no clone) is structured: the parent is blocked
// with work.blocked in the same transaction as the error record and event.
func TestIntegration_VerificationStage_SetupFailureBlocksTheParent(t *testing.T) {
	pool := integrationPool(t)
	f := seedStageFixture(t, pool)
	if err := stageHandler(pool)(context.Background(), stageMessage(t, f, evidence.OutcomeVerified, true)); err != nil {
		t.Fatal(err)
	}
	evs := verificationEvents(t, pool, f.parent)
	if len(evs) != 1 || evs[0].Decision != stage.DecisionError || !strings.Contains(evs[0].Reason, "no clone path") {
		t.Fatalf("events = %+v", evs)
	}
	var state string
	if err := pool.QueryRow(context.Background(), `SELECT state::text FROM work_items WHERE id = $1`, f.parent).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "blocked" {
		t.Errorf("parent state = %s, want blocked", state)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM outbox_events WHERE aggregate_id = $1 AND topic = 'work.blocked'`, f.parent); n != 1 {
		t.Errorf("work.blocked events = %d, want 1", n)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM agent_assignments WHERE work_item_id = $1 AND status = 'failed'`, f.subtask); n != 1 {
		t.Errorf("failed stage assignments = %d, want 1", n)
	}
	var reason string
	if err := pool.QueryRow(context.Background(), `SELECT reason FROM work_item_transitions WHERE work_item_id = $1 AND to_state = 'blocked'`, f.parent).Scan(&reason); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reason, "Retry re-runs this item from the start of the pipeline") {
		t.Errorf("block reason does not say what Retry costs: %q", reason)
	}
}

// Review minor 3: a parent that already ended is not blocked; the error
// result, the event and the guard row still commit.
func TestIntegration_VerificationStage_SetupFailureOnACancelledParent(t *testing.T) {
	pool := integrationPool(t)
	f := seedStageFixture(t, pool)
	if _, err := pool.Exec(context.Background(), `UPDATE work_items SET state = 'cancelled' WHERE id = $1`, f.parent); err != nil {
		t.Fatal(err)
	}
	msg := stageMessage(t, f, evidence.OutcomeVerified, true)
	if err := stageHandler(pool)(context.Background(), msg); err != nil {
		t.Fatal(err)
	}
	var state string
	_ = pool.QueryRow(context.Background(), `SELECT state::text FROM work_items WHERE id = $1`, f.parent).Scan(&state)
	if state != "cancelled" {
		t.Errorf("parent = %s, want it left cancelled", state)
	}
	if evs := verificationEvents(t, pool, f.parent); len(evs) != 1 || evs[0].Decision != stage.DecisionError {
		t.Errorf("events = %+v", evs)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM outbox_events WHERE aggregate_id = $1 AND topic = 'work.blocked'`, f.parent); n != 0 {
		t.Errorf("work.blocked = %d, want 0", n)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM processed_events WHERE consumer = $1 AND event_id = $2`, verificationIdempotencyConsumer, msg.Headers[bus.HeaderEventID]); n != 1 {
		t.Errorf("ledger rows = %d, want 1", n)
	}
}
