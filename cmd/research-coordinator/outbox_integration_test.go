package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/bus"
	"github.com/autonomousengine/autonomous-engine/internal/db"
	"github.com/autonomousengine/autonomous-engine/internal/models"
	"github.com/autonomousengine/autonomous-engine/internal/outbox"
	"github.com/autonomousengine/autonomous-engine/internal/workgraph"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Env-gated integration tests for the M1-04 research-coordinator conversion:
// the research fan-out commits the transition and all N research.tasks
// events in one transaction, and the dispatcher delivers them in order. Run
// with the common3 docker command against a disposable database:
//
//	go test ./cmd/research-coordinator/ -run Integration -v
//
// They skip when TEST_POSTGRES_DSN / TEST_DATABASE_URL is unset.

func integrationDSN() string {
	if dsn := os.Getenv("TEST_POSTGRES_DSN"); dsn != "" {
		return dsn
	}
	return os.Getenv("TEST_DATABASE_URL")
}

func integrationPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := integrationDSN()
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

// newWorkItem seeds a product and one work item in state; both are removed,
// with every outbox row under the item's aggregate, when the test ends.
func newWorkItem(t *testing.T, pool *pgxpool.Pool, state models.WorkItemState) (workID, productID uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	workID, productID = uuid.New(), uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO products (id, name) VALUES ($1, $2)`, productID, "m104d-"+productID.String()[:8]); err != nil {
		t.Fatalf("seed product: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO work_items (id, type, state, title, product_id) VALUES ($1, 'feature', $2, 'M1-04d research', $3)`,
		workID, state, productID); err != nil {
		t.Fatalf("seed work item: %v", err)
	}
	t.Cleanup(func() {
		cctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		for _, sql := range []string{
			`DELETE FROM outbox_events WHERE aggregate_id = $1`,
			`DELETE FROM work_items WHERE id = $1`,
		} {
			if _, err := pool.Exec(cctx, sql, workID); err != nil {
				t.Logf("cleanup %q: %v", sql, err)
			}
		}
		if _, err := pool.Exec(cctx, `DELETE FROM products WHERE id = $1`, productID); err != nil {
			t.Logf("cleanup product: %v", err)
		}
	})
	return workID, productID
}

func tasksFor(workID, productID uuid.UUID, n int) []researchTaskMessage {
	types := []string{"impact_assessment", "pattern_match", "code_trace", "history_analysis", "bug_archaeology"}
	out := make([]researchTaskMessage, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, researchTaskMessage{
			TaskID:          uuid.NewString(),
			WorkID:          workID.String(),
			ProductID:       productID.String(),
			ResearchType:    types[i%len(types)],
			StartingContext: "the PRD",
		})
	}
	return out
}

type outboxRow struct {
	ID          uuid.UUID
	AggType     string
	AggID       uuid.UUID
	Topic       string
	Key         string
	Payload     []byte
	XactID      int64
	PublishedAt *time.Time
	ClaimedBy   *string
}

// rowsFor returns the outbox rows under aggregateID in insertion order, with
// the id of the transaction that wrote each (xmin), so "one transaction" is
// checked directly rather than inferred.
func rowsFor(t *testing.T, pool *pgxpool.Pool, aggregateID uuid.UUID) []outboxRow {
	t.Helper()
	rows, err := pool.Query(context.Background(), `
		SELECT id, aggregate_type, aggregate_id, topic, key, payload, xmin::text::bigint, published_at, claimed_by
		FROM outbox_events WHERE aggregate_id = $1 ORDER BY created_at, id`, aggregateID)
	if err != nil {
		t.Fatalf("select outbox rows: %v", err)
	}
	defer rows.Close()
	var out []outboxRow
	for rows.Next() {
		var r outboxRow
		if err := rows.Scan(&r.ID, &r.AggType, &r.AggID, &r.Topic, &r.Key, &r.Payload, &r.XactID, &r.PublishedAt, &r.ClaimedBy); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return out
}

func stateOf(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) (models.WorkItemState, int64, int) {
	t.Helper()
	var st models.WorkItemState
	var xmin int64
	if err := pool.QueryRow(context.Background(), `SELECT state, xmin::text::bigint FROM work_items WHERE id = $1`, id).Scan(&st, &xmin); err != nil {
		t.Fatalf("select state: %v", err)
	}
	var transitions int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM work_item_transitions WHERE work_item_id = $1`, id).Scan(&transitions); err != nil {
		t.Fatalf("count transitions: %v", err)
	}
	return st, xmin, transitions
}

type fakeBus struct {
	mu   sync.Mutex
	sent []bus.Message
}

func (b *fakeBus) PublishMessage(_ context.Context, msg *bus.Message) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.sent = append(b.sent, *msg)
	return nil
}

func (b *fakeBus) messages() []bus.Message {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]bus.Message(nil), b.sent...)
}

// TestIntegration_ResearchFanOutCommitsInOneTransaction: the transition to
// research and all N research.tasks events share one commit, and the
// dispatcher delivers them in dispatch order with event_id and the original
// key and payload.
func TestIntegration_ResearchFanOutCommitsInOneTransaction(t *testing.T) {
	pool := integrationPool(t)
	wg := workgraph.New(pool)
	workID, productID := newWorkItem(t, pool, models.WorkItemStatePRD)
	tasks := tasksFor(workID, productID, 5)

	if err := commitResearchDispatch(context.Background(), pool, wg, workID, workID, tasks, nil); err != nil {
		t.Fatalf("commitResearchDispatch: %v", err)
	}

	st, itemXact, transitions := stateOf(t, pool, workID)
	if st != models.WorkItemStateResearch {
		t.Errorf("state = %s, want research", st)
	}
	if transitions != 1 {
		t.Errorf("transition rows = %d, want 1", transitions)
	}
	rows := rowsFor(t, pool, workID)
	if len(rows) != len(tasks) {
		t.Fatalf("outbox rows = %d, want %d", len(rows), len(tasks))
	}
	for i, r := range rows {
		if r.XactID != itemXact {
			t.Errorf("row %d written by transaction %d, the transition by %d — not one transaction", i, r.XactID, itemXact)
		}
		if r.AggType != "work_item" || r.AggID != workID || r.Topic != bus.TopicResearchTasks || r.Key != tasks[i].TaskID {
			t.Errorf("row %d addressed %s/%s/%s/%s, want work_item/%s/research.tasks/%s", i, r.AggType, r.AggID, r.Topic, r.Key, workID, tasks[i].TaskID)
		}
	}

	fb := &fakeBus{}
	d := outbox.NewDispatcher(outbox.NewStore(pool), fb, outbox.Options{ClaimedBy: "research-coordinator/m104d-test"})
	for i := 0; i < len(tasks); i++ {
		if _, err := d.RunOnce(context.Background()); err != nil {
			t.Fatalf("RunOnce %d: %v", i, err)
		}
	}
	sent := fb.messages()
	if len(sent) != len(tasks) {
		t.Fatalf("delivered %d messages, want %d", len(sent), len(tasks))
	}
	for i, m := range sent {
		if m.Topic != bus.TopicResearchTasks || m.Key != tasks[i].TaskID {
			t.Errorf("message %d = %s/%s, want research.tasks/%s — out of order", i, m.Topic, m.Key, tasks[i].TaskID)
		}
		if m.Headers[outbox.HeaderEventID] != rows[i].ID.String() {
			t.Errorf("message %d event_id header = %q, want %s", i, m.Headers[outbox.HeaderEventID], rows[i].ID)
		}
		var got researchTaskMessage
		if err := json.Unmarshal(m.Value, &got); err != nil {
			t.Fatalf("decode message %d: %v", i, err)
		}
		if got.TaskID != tasks[i].TaskID || got.ResearchType != tasks[i].ResearchType || got.WorkID != workID.String() {
			t.Errorf("message %d payload = %+v, want %+v", i, got, tasks[i])
		}
	}
	for i, r := range rowsFor(t, pool, workID) {
		if r.PublishedAt == nil {
			t.Errorf("row %d published_at is NULL after delivery", i)
		}
	}
}

// The approval paths move the item to research before prd.ready; the
// dispatch must still enqueue its tasks without writing a second transition.
func TestIntegration_ResearchFanOutAlreadyInResearch(t *testing.T) {
	pool := integrationPool(t)
	wg := workgraph.New(pool)
	workID, productID := newWorkItem(t, pool, models.WorkItemStateResearch)
	tasks := tasksFor(workID, productID, 4)

	if err := commitResearchDispatch(context.Background(), pool, wg, workID, workID, tasks, nil); err != nil {
		t.Fatalf("commitResearchDispatch: %v", err)
	}
	st, _, transitions := stateOf(t, pool, workID)
	if st != models.WorkItemStateResearch || transitions != 0 {
		t.Errorf("state %s with %d transition rows, want research with 0", st, transitions)
	}
	if n := len(rowsFor(t, pool, workID)); n != len(tasks) {
		t.Errorf("outbox rows = %d, want %d", n, len(tasks))
	}
}

// A cancelled item must not get research tasks: the transition fails and
// nothing is enqueued.
func TestIntegration_ResearchFanOutRejectsCancelledItem(t *testing.T) {
	pool := integrationPool(t)
	wg := workgraph.New(pool)
	workID, productID := newWorkItem(t, pool, models.WorkItemStateCancelled)

	err := commitResearchDispatch(context.Background(), pool, wg, workID, workID, tasksFor(workID, productID, 4), nil)
	if !errors.Is(err, workgraph.ErrInvalidTransition) {
		t.Fatalf("err = %v, want ErrInvalidTransition", err)
	}
	if n := len(rowsFor(t, pool, workID)); n != 0 {
		t.Errorf("outbox rows = %d, want 0", n)
	}
}

// The enqueue runs after the transition has been written; failing it must
// roll the transition back too.
func TestIntegration_ResearchFanOutEnqueueFailureRollsBackTransition(t *testing.T) {
	pool := integrationPool(t)
	wg := workgraph.New(pool)
	workID, productID := newWorkItem(t, pool, models.WorkItemStatePRD)

	cause := errors.New("m104d: enqueue refused")
	original := outboxEnqueueAll
	outboxEnqueueAll = func(context.Context, pgx.Tx, ...outbox.Event) error { return cause }
	t.Cleanup(func() { outboxEnqueueAll = original })

	if err := commitResearchDispatch(context.Background(), pool, wg, workID, workID, tasksFor(workID, productID, 4), nil); !errors.Is(err, cause) {
		t.Fatalf("err = %v, want %v", err, cause)
	}
	st, _, transitions := stateOf(t, pool, workID)
	if st != models.WorkItemStatePRD || transitions != 0 {
		t.Errorf("state %s with %d transition rows, want prd with 0 — the transition outlived its events", st, transitions)
	}
	if n := len(rowsFor(t, pool, workID)); n != 0 {
		t.Errorf("outbox rows = %d, want 0", n)
	}
}

// The technical brief artifact, technical-brief.ready and the assignment
// completion commit together.
func TestIntegration_TechnicalBriefCommitsArtifactEventAndAssignment(t *testing.T) {
	pool := integrationPool(t)
	wg := workgraph.New(pool)
	ctx := context.Background()
	workID, productID := newWorkItem(t, pool, models.WorkItemStateResearch)

	assignment := &models.AgentAssignment{
		WorkItemID:      workID,
		AgentType:       models.AgentTypeResearchCoordinator,
		AgentInstanceID: agentID,
		Status:          models.AssignmentStatusActive,
	}
	if err := wg.AssignAgent(ctx, assignment); err != nil {
		t.Fatalf("AssignAgent: %v", err)
	}
	artifact := &models.WorkItemArtifact{WorkItemID: workID, Type: models.ArtifactTypeTechnicalBrief, Content: []byte(`{"brief":"b","narrative":"b"}`), AgentID: agentID}
	msg := technicalBriefReadyMessage{WorkID: workID.String(), ProductID: productID.String(), BriefContent: "b", Timestamp: time.Now().UTC().Format(time.RFC3339)}

	if err := commitTechnicalBrief(ctx, pool, wg, workID, artifact, msg, assignment.ID, uuid.Nil, nil); err != nil {
		t.Fatalf("commitTechnicalBrief: %v", err)
	}
	var artifacts int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM work_item_artifacts WHERE work_item_id = $1 AND artifact_type = 'technical_brief'`, workID).Scan(&artifacts); err != nil {
		t.Fatalf("count artifacts: %v", err)
	}
	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM agent_assignments WHERE id = $1`, assignment.ID).Scan(&status); err != nil {
		t.Fatalf("select assignment: %v", err)
	}
	rows := rowsFor(t, pool, workID)
	if artifacts != 1 || status != string(models.AssignmentStatusCompleted) || len(rows) != 1 {
		t.Fatalf("artifacts=%d assignment=%s outbox rows=%d, want 1/completed/1", artifacts, status, len(rows))
	}
	if rows[0].Topic != bus.TopicTechnicalBriefReady || rows[0].Key != workID.String() {
		t.Errorf("row addressed %s/%s, want technical-brief.ready/%s", rows[0].Topic, rows[0].Key, workID)
	}
}
