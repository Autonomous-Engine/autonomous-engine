package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/bus"
	"github.com/autonomousengine/autonomous-engine/internal/db"
	"github.com/autonomousengine/autonomous-engine/internal/evidence"
	"github.com/autonomousengine/autonomous-engine/internal/interactions"
	"github.com/autonomousengine/autonomous-engine/internal/logging"
	"github.com/autonomousengine/autonomous-engine/internal/models"
	"github.com/autonomousengine/autonomous-engine/internal/workgraph"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Finalization's outcome transaction against a real PostgreSQL and a real
// git fixture: the evidence row and the implementation.complete outbox row
// commit together, and a failed insert leaves neither. Skips without
// TEST_POSTGRES_DSN / TEST_DATABASE_URL; run with the repo's docker command
// (`go test ./cmd/implementation-agent/ -run Integration -v`).

func integrationPool(t *testing.T) *pgxpool.Pool {
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
		t.Fatalf("pgxpool.New: %v", err)
	}
	if err := db.Migrate(ctx, p, filepath.Join("..", "..", "migrations")); err != nil {
		p.Close()
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(p.Close)
	return p
}

type dbFixture struct {
	productID    uuid.UUID
	workID       uuid.UUID
	assignmentID uuid.UUID
}

func newDBFixture(t *testing.T, pool *pgxpool.Pool) dbFixture {
	t.Helper()
	ctx := context.Background()
	f := dbFixture{productID: uuid.New(), workID: uuid.New()}
	if _, err := pool.Exec(ctx, `INSERT INTO products (id, name) VALUES ($1, $2)`, f.productID, "m104c-test-"+f.productID.String()[:8]); err != nil {
		t.Fatalf("seed product: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO work_items (id, type, state, title, product_id) VALUES ($1, 'feature', 'implementation', 'M1-04c parent', $2)`, f.workID, f.productID); err != nil {
		t.Fatalf("seed work item: %v", err)
	}
	a := &models.AgentAssignment{WorkItemID: f.workID, AgentType: models.AgentTypeImplementation, AgentInstanceID: agentID, Status: models.AssignmentStatusActive}
	if err := workgraph.New(pool).AssignAgent(ctx, a); err != nil {
		t.Fatalf("seed assignment: %v", err)
	}
	f.assignmentID = a.ID
	t.Cleanup(func() {
		cctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		for _, q := range []string{
			`DELETE FROM outbox_events WHERE aggregate_id = $1`,
			`DELETE FROM work_items WHERE id = $1`,
		} {
			if _, err := pool.Exec(cctx, q, f.workID); err != nil {
				t.Logf("cleanup %q: %v", q, err)
			}
		}
		if _, err := pool.Exec(cctx, `DELETE FROM products WHERE id = $1`, f.productID); err != nil {
			t.Logf("cleanup product: %v", err)
		}
	})
	return f
}

func (f dbFixture) sink(pool *pgxpool.Pool, taskID string) *outboxCompletionSink {
	return &outboxCompletionSink{
		ctx: context.Background(), db: pool, wg: workgraph.New(pool),
		prompts: interactions.NewPublisher(nil),
		workID:  f.workID, assignmentID: f.assignmentID, taskID: taskID,
		logger: logging.New("implementation-agent-test"),
	}
}

type outboxRec struct {
	topic, key, aggregateType string
	aggregateID               uuid.UUID
	payload                   []byte
}

func outboxRows(t *testing.T, pool *pgxpool.Pool, workID uuid.UUID) []outboxRec {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT topic, key, aggregate_type, aggregate_id, payload FROM outbox_events WHERE aggregate_id = $1 ORDER BY created_at, id`, workID)
	if err != nil {
		t.Fatalf("query outbox: %v", err)
	}
	defer rows.Close()
	var out []outboxRec
	for rows.Next() {
		var r outboxRec
		if err := rows.Scan(&r.topic, &r.key, &r.aggregateType, &r.aggregateID, &r.payload); err != nil {
			t.Fatalf("scan outbox: %v", err)
		}
		out = append(out, r)
	}
	return out
}

func countRows(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return n
}

func stateAndAssignment(t *testing.T, pool *pgxpool.Pool, f dbFixture) (string, string) {
	t.Helper()
	var state, status string
	if err := pool.QueryRow(context.Background(), `SELECT state::text FROM work_items WHERE id = $1`, f.workID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(context.Background(), `SELECT status::text FROM agent_assignments WHERE id = $1`, f.assignmentID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	return state, status
}

func TestIntegration_FinalizeSuccessCommitsEvidenceAndEventTogether(t *testing.T) {
	requireSh(t)
	pool := integrationPool(t)
	dbf := newDBFixture(t, pool)
	g := newFinalizeFixture(t)
	head := g.commit(t, "feature.txt", "value=2\n", "feat: initial")
	in := g.inputs()
	in.task.WorkID = dbf.workID.String()
	in.evidenceStore = evidence.NewStore(pool)

	if !finalizeImplementation(context.Background(), in, dbf.sink(pool, in.task.TaskID)) {
		t.Fatal("finalize failed on a passing, pushable head")
	}
	var evHead, outcome string
	if err := pool.QueryRow(context.Background(), `SELECT head_sha, outcome FROM implementation_verifications WHERE work_item_id = $1`, dbf.workID).Scan(&evHead, &outcome); err != nil {
		t.Fatalf("evidence row: %v", err)
	}
	if evHead != head || outcome != string(evidence.OutcomeVerified) {
		t.Fatalf("evidence row = %s/%s, want %s/verified", evHead, outcome, head)
	}
	rows := outboxRows(t, pool, dbf.workID)
	if len(rows) != 1 || rows[0].topic != bus.TopicImplementationComplete || rows[0].key != in.task.TaskID || rows[0].aggregateType != "work_item" {
		t.Fatalf("outbox rows = %+v, want one implementation.complete keyed by the task", rows)
	}
	var msg implementationCompleteMessage
	if err := json.Unmarshal(rows[0].payload, &msg); err != nil {
		t.Fatal(err)
	}
	if !msg.Success || msg.HeadSHA != head || msg.Evidence == nil || msg.Evidence.HeadSHA != head {
		t.Fatalf("enqueued %+v, want success with evidence at %s", msg, head)
	}
	if state, status := stateAndAssignment(t, pool, dbf); state != "implementation" || status != string(models.AssignmentStatusCompleted) {
		t.Fatalf("state=%s assignment=%s, want implementation/completed", state, status)
	}
}

func TestIntegration_FinalizeFailureCommitsEvidenceBlockAndEvent(t *testing.T) {
	requireSh(t)
	pool := integrationPool(t)
	dbf := newDBFixture(t, pool)
	g := newFinalizeFixture(t)
	g.commit(t, "feature.txt", "value=3\n", "feat: wrong")
	in := g.inputs()
	in.task.WorkID = dbf.workID.String()
	in.evidenceStore = evidence.NewStore(pool)

	if finalizeImplementation(context.Background(), in, dbf.sink(pool, in.task.TaskID)) {
		t.Fatal("finalize succeeded on failing tests")
	}
	if n := countRows(t, pool, `SELECT count(*) FROM implementation_verifications WHERE work_item_id = $1 AND outcome = 'failed'`, dbf.workID); n != 1 {
		t.Fatalf("failed evidence rows = %d, want 1", n)
	}
	rows := outboxRows(t, pool, dbf.workID)
	if len(rows) != 2 || rows[0].topic != bus.TopicWorkBlocked || rows[1].topic != bus.TopicImplementationComplete {
		t.Fatalf("outbox rows = %+v, want work.blocked then implementation.complete", rows)
	}
	if state, status := stateAndAssignment(t, pool, dbf); state != "blocked" || status != string(models.AssignmentStatusFailed) {
		t.Fatalf("state=%s assignment=%s, want blocked/failed", state, status)
	}
}

// On the failure path an evidence insert that fails must not cost the
// parent its block: tier 1 commits the block, work.blocked and the failure
// event without the evidence row.
func TestIntegration_FinalizeFailureEvidenceInsertFailureStillBlocks(t *testing.T) {
	requireSh(t)
	pool := integrationPool(t)
	dbf := newDBFixture(t, pool)
	g := newFinalizeFixture(t)
	broken := g.commit(t, "feature.txt", "value=3\n", "feat: wrong")
	in := g.inputs()
	in.task.WorkID = dbf.workID.String()
	in.task.RepositoryID = uuid.NewString() // violates the repository_id FK
	in.evidenceStore = evidence.NewStore(pool)

	if finalizeImplementation(context.Background(), in, dbf.sink(pool, in.task.TaskID)) {
		t.Fatal("finalize succeeded on failing tests")
	}
	if n := countRows(t, pool, `SELECT count(*) FROM implementation_verifications WHERE work_item_id = $1`, dbf.workID); n != 0 {
		t.Fatalf("evidence rows = %d, want 0", n)
	}
	rows := outboxRows(t, pool, dbf.workID)
	if len(rows) != 2 || rows[0].topic != bus.TopicWorkBlocked || rows[1].topic != bus.TopicImplementationComplete {
		t.Fatalf("outbox rows = %+v, want work.blocked then implementation.complete", rows)
	}
	var msg implementationCompleteMessage
	if err := json.Unmarshal(rows[1].payload, &msg); err != nil {
		t.Fatal(err)
	}
	if msg.Success || msg.HeadSHA != broken || msg.Evidence == nil || msg.Evidence.Outcome != evidence.OutcomeFailed {
		t.Fatalf("event = %+v, want the unchanged failure at %s", msg, broken)
	}
	if state, status := stateAndAssignment(t, pool, dbf); state != "blocked" || status != string(models.AssignmentStatusFailed) {
		t.Fatalf("state=%s assignment=%s, want blocked/failed", state, status)
	}
}

func TestIntegration_FinalizeEvidenceInsertFailureLeavesNoSuccess(t *testing.T) {
	requireSh(t)
	pool := integrationPool(t)
	dbf := newDBFixture(t, pool)
	g := newFinalizeFixture(t)
	head := g.commit(t, "feature.txt", "value=2\n", "feat: initial")
	in := g.inputs()
	in.task.WorkID = dbf.workID.String()
	// An unknown repository violates implementation_verifications'
	// repository_id foreign key: the evidence insert fails inside the tx.
	in.task.RepositoryID = uuid.NewString()
	in.evidenceStore = evidence.NewStore(pool)

	if finalizeImplementation(context.Background(), in, dbf.sink(pool, in.task.TaskID)) {
		t.Fatal("finalize reported success although the evidence insert failed")
	}
	if n := countRows(t, pool, `SELECT count(*) FROM implementation_verifications WHERE work_item_id = $1`, dbf.workID); n != 0 {
		t.Fatalf("evidence rows = %d, want 0", n)
	}
	rows := outboxRows(t, pool, dbf.workID)
	if len(rows) != 2 || rows[0].topic != bus.TopicWorkBlocked || rows[1].topic != bus.TopicImplementationComplete {
		t.Fatalf("outbox rows = %+v, want only the fallback's work.blocked and implementation.complete", rows)
	}
	var msg implementationCompleteMessage
	if err := json.Unmarshal(rows[1].payload, &msg); err != nil {
		t.Fatal(err)
	}
	if msg.Success || msg.Verified || msg.Evidence == nil || msg.Evidence.Outcome != evidence.OutcomeVerified || msg.Evidence.HeadSHA != head {
		t.Fatalf("fallback event = %+v, want success=false carrying the unchanged verified evidence", msg)
	}
	if state, status := stateAndAssignment(t, pool, dbf); state != "blocked" || status != string(models.AssignmentStatusFailed) {
		t.Fatalf("state=%s assignment=%s, want blocked/failed", state, status)
	}
}
