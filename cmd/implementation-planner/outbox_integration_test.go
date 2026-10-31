package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

// These tests drive the decomposition step's write path — the one M1-03
// converted — against a real PostgreSQL and a fake bus. What they are for is
// the claim the ticket makes and that no unit test can check: that the subtask
// row and the implementation.tasks event share a single commit, and that the
// dispatcher afterwards delivers exactly the message the direct publish used
// to send.
//
// Run them with the repo's docker command:
//
//	docker run ... -e TEST_POSTGRES_DSN=... golang:1.22-alpine \
//	    sh -c "go test ./cmd/implementation-planner/ -run Integration -v"
//
// They skip cleanly when TEST_POSTGRES_DSN / TEST_DATABASE_URL is unset.

// ─── Harness ─────────────────────────────────────────────────────────────────

// integrationDSN returns the test database DSN, following the repo convention
// (internal/evidence, internal/outbox): TEST_POSTGRES_DSN first, then
// TEST_DATABASE_URL.
func integrationDSN() string {
	if dsn := os.Getenv("TEST_POSTGRES_DSN"); dsn != "" {
		return dsn
	}
	return os.Getenv("TEST_DATABASE_URL")
}

// integrationPool opens the test database and applies the repository's
// migrations, so work_items and outbox_events (047) are both present.
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
	// Registered first so it closes last: the row cleanups registered below
	// still need a live pool.
	t.Cleanup(p.Close)
	return p
}

// fixture is the minimum graph a repo-scoped subtask needs: a product, a
// repository on it, a parent work item, and an agent definition for the
// subtask to inherit.
type fixture struct {
	productID    uuid.UUID
	repositoryID uuid.UUID
	parentWorkID uuid.UUID
	agentDefID   uuid.UUID
}

func newFixture(t *testing.T, pool *pgxpool.Pool) fixture {
	t.Helper()
	ctx := context.Background()
	f := fixture{
		productID:    uuid.New(),
		repositoryID: uuid.New(),
		parentWorkID: uuid.New(),
		agentDefID:   uuid.New(),
	}
	suffix := f.productID.String()[:8]

	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed (%s): %v", sql, err)
		}
	}
	exec(`INSERT INTO products (id, name) VALUES ($1, $2)`, f.productID, "m103-test-"+suffix)
	// base_branch is spelled out: a later migration dropped its default and
	// the column is NOT NULL.
	exec(`INSERT INTO repositories (id, product_id, name, github_url, clone_path, base_branch)
	      VALUES ($1, $2, $3, $4, $5, 'main')`,
		f.repositoryID, f.productID, "m103-repo-"+suffix,
		"https://example.invalid/m103-"+suffix, "/tmp/m103-"+suffix)
	exec(`INSERT INTO agent_definitions (id, name) VALUES ($1, $2)`, f.agentDefID, "m103-agent-"+suffix)
	exec(`INSERT INTO work_items (id, type, state, title, product_id, repository_id, agent_definition_id)
	      VALUES ($1, 'feature', 'planning', 'M1-03 parent', $2, $3, $4)`,
		f.parentWorkID, f.productID, f.repositoryID, f.agentDefID)

	t.Cleanup(func() {
		cctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		// outbox_events has no FK, so it is deleted explicitly; everything
		// else cascades from the product except the work items, which are
		// ON DELETE SET NULL, and the agent definition.
		for _, sql := range []string{
			`DELETE FROM outbox_events WHERE aggregate_id = $1`,
			`DELETE FROM work_items WHERE parent_id = $1`,
			`DELETE FROM work_items WHERE id = $1`,
		} {
			if _, err := pool.Exec(cctx, sql, f.parentWorkID); err != nil {
				t.Logf("cleanup %q: %v", sql, err)
			}
		}
		if _, err := pool.Exec(cctx, `DELETE FROM products WHERE id = $1`, f.productID); err != nil {
			t.Logf("cleanup product: %v", err)
		}
		if _, err := pool.Exec(cctx, `DELETE FROM agent_definitions WHERE id = $1`, f.agentDefID); err != nil {
			t.Logf("cleanup agent definition: %v", err)
		}
	})
	return f
}

// subtaskFor builds the work item the planner would build for one repo group.
func (f fixture) subtaskFor(taskID uuid.UUID) *models.WorkItem {
	repoID := f.repositoryID
	parentID := f.parentWorkID
	productID := f.productID
	return &models.WorkItem{
		ID:           taskID,
		Type:         models.WorkItemTypeSubtask,
		State:        models.WorkItemStateImplementation,
		Title:        "Implement in m103-repo: step one",
		Description:  "## Step 1: step one\n\ndo the thing\n",
		Priority:     models.PriorityMedium,
		CodexVersion: 7,
		ParentID:     &parentID,
		ProductID:    &productID,
		RepositoryID: &repoID,
	}
}

// taskMessageFor builds the implementation.tasks message for that subtask,
// with the same field set the planner sends.
func (f fixture) taskMessageFor(taskID uuid.UUID) implementationTaskMessage {
	return implementationTaskMessage{
		TaskID:          taskID.String(),
		WorkID:          f.parentWorkID.String(),
		ProductID:       f.productID.String(),
		RepositoryID:    f.repositoryID.String(),
		Title:           "Implement in m103-repo: step one",
		Description:     "## Step 1: step one\n\ndo the thing\n",
		FilesToChange:   []string{"src/app.ts"},
		CodexVersion:    7,
		TechnicalBrief:  "the brief prose",
		CodexPatterns:   "pattern text",
		AffectedModules: []string{"app"},
	}
}

// fakeBus records what the dispatcher publishes and can be told to fail. It
// satisfies outbox.Publisher, which is the only bus surface the planner's
// task path touches now.
type fakeBus struct {
	mu       sync.Mutex
	sent     []bus.Message
	failWith error
}

func (b *fakeBus) PublishMessage(_ context.Context, msg *bus.Message) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.failWith != nil {
		return b.failWith
	}
	copied := *msg
	copied.Headers = map[string]string{}
	for k, v := range msg.Headers {
		copied.Headers[k] = v
	}
	b.sent = append(b.sent, copied)
	return nil
}

func (b *fakeBus) messages() []bus.Message {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]bus.Message(nil), b.sent...)
}

// outboxRow is the slice of outbox_events these tests assert on.
type outboxRow struct {
	ID            uuid.UUID
	AggregateType string
	AggregateID   uuid.UUID
	Topic         string
	Key           string
	Payload       []byte
	Attempts      int
	PublishedAt   *time.Time
	DeadAt        *time.Time
	LastError     *string
}

func rowsForAggregate(t *testing.T, pool *pgxpool.Pool, aggregateID uuid.UUID) []outboxRow {
	t.Helper()
	rows, err := pool.Query(context.Background(), `
		SELECT id, aggregate_type, aggregate_id, topic, key, payload, attempts, published_at, dead_at, last_error
		FROM outbox_events WHERE aggregate_id = $1 ORDER BY created_at, id`, aggregateID)
	if err != nil {
		t.Fatalf("select outbox rows: %v", err)
	}
	defer rows.Close()

	var out []outboxRow
	for rows.Next() {
		var r outboxRow
		if err := rows.Scan(&r.ID, &r.AggregateType, &r.AggregateID, &r.Topic, &r.Key,
			&r.Payload, &r.Attempts, &r.PublishedAt, &r.DeadAt, &r.LastError); err != nil {
			t.Fatalf("scan outbox row: %v", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate outbox rows: %v", err)
	}
	return out
}

func workItemExists(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) bool {
	t.Helper()
	var exists bool
	if err := pool.QueryRow(context.Background(),
		`SELECT EXISTS(SELECT 1 FROM work_items WHERE id = $1)`, id).Scan(&exists); err != nil {
		t.Fatalf("check work item %s: %v", id, err)
	}
	return exists
}

// withEnqueueFailure swaps the outbox enqueue seam for the duration of a test.
func withEnqueueFailure(t *testing.T, cause error) {
	t.Helper()
	original := outboxEnqueueAll
	outboxEnqueueAll = func(context.Context, pgx.Tx, ...outbox.Event) error { return cause }
	t.Cleanup(func() { outboxEnqueueAll = original })
}

// planFor wraps one subtask and its message as a single-entry decomposition.
func (f fixture) planFor(taskID uuid.UUID) []plannedSubtask {
	return []plannedSubtask{{
		repoName: "m103-repo",
		steps:    1,
		titles:   []string{"step one"},
		item:     f.subtaskFor(taskID),
		msg:      f.taskMessageFor(taskID),
	}}
}

// ─── Tests ───────────────────────────────────────────────────────────────────

// TestIntegration_SubtaskAndEventCommitTogether is the positive half of the
// M1-03 claim: after commitDecomposition returns, the subtask, its
// inherited agent definition and the implementation.tasks event are all in the
// database, and the event is addressed exactly as the direct publish addressed
// it — topic implementation.tasks, key = task id — so no consumer can tell the
// difference.
func TestIntegration_SubtaskAndEventCommitTogether(t *testing.T) {
	pool := integrationPool(t)
	f := newFixture(t, pool)
	wg := workgraph.New(pool)

	taskID := uuid.New()
	subItem := f.subtaskFor(taskID)
	taskMsg := f.taskMessageFor(taskID)

	plan := []plannedSubtask{{repoName: "m103-repo", steps: 1, item: subItem, msg: taskMsg}}
	if err := commitDecomposition(context.Background(), pool, wg, f.parentWorkID, &f.agentDefID, plan); err != nil {
		t.Fatalf("commitDecomposition: %v", err)
	}

	if !workItemExists(t, pool, taskID) {
		t.Fatal("subtask work item is missing after a successful commit")
	}

	var gotAgentDef *uuid.UUID
	var gotParent *uuid.UUID
	var gotState string
	if err := pool.QueryRow(context.Background(),
		`SELECT agent_definition_id, parent_id, state::text FROM work_items WHERE id = $1`, taskID).
		Scan(&gotAgentDef, &gotParent, &gotState); err != nil {
		t.Fatalf("read subtask: %v", err)
	}
	if gotAgentDef == nil || *gotAgentDef != f.agentDefID {
		t.Errorf("agent_definition_id = %v, want %s — the inheritance must commit with the row", gotAgentDef, f.agentDefID)
	}
	if gotParent == nil || *gotParent != f.parentWorkID {
		t.Errorf("parent_id = %v, want %s", gotParent, f.parentWorkID)
	}
	if gotState != string(models.WorkItemStateImplementation) {
		t.Errorf("state = %q, want %q", gotState, models.WorkItemStateImplementation)
	}

	rows := rowsForAggregate(t, pool, f.parentWorkID)
	if len(rows) != 1 {
		t.Fatalf("outbox rows for the parent = %d, want exactly 1", len(rows))
	}
	row := rows[0]
	if row.AggregateType != "work_item" {
		t.Errorf("aggregate_type = %q, want %q", row.AggregateType, "work_item")
	}
	if row.AggregateID != f.parentWorkID {
		t.Errorf("aggregate_id = %s, want the PARENT work id %s — ordering is promised per aggregate, and the plan's tasks are the aggregate", row.AggregateID, f.parentWorkID)
	}
	if row.Topic != bus.TopicImplementationTasks {
		t.Errorf("topic = %q, want %q", row.Topic, bus.TopicImplementationTasks)
	}
	if row.Key != taskID.String() {
		t.Errorf("key = %q, want the task id %q (unchanged from the direct publish, so partitioning is unchanged)", row.Key, taskID)
	}
	if row.PublishedAt != nil {
		t.Errorf("published_at = %v on a freshly committed row; only the dispatcher may set it", row.PublishedAt)
	}
	if row.Attempts != 0 {
		t.Errorf("attempts = %d on a freshly committed row, want 0", row.Attempts)
	}

	// The payload is the unchanged implementationTaskMessage plus the
	// additive event_id (M1-03 requirement 3: consumers need no change).
	var decoded implementationTaskMessage
	if err := json.Unmarshal(row.Payload, &decoded); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if decoded.TaskID != taskMsg.TaskID || decoded.WorkID != taskMsg.WorkID ||
		decoded.RepositoryID != taskMsg.RepositoryID || decoded.Title != taskMsg.Title ||
		decoded.Description != taskMsg.Description || decoded.CodexVersion != taskMsg.CodexVersion ||
		decoded.TechnicalBrief != taskMsg.TechnicalBrief {
		t.Errorf("payload does not round-trip the task message:\n got %+v\nwant %+v", decoded, taskMsg)
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(row.Payload, &envelope); err != nil {
		t.Fatalf("decode payload envelope: %v", err)
	}
	var eventID string
	if raw, ok := envelope[outbox.FieldEventID]; !ok {
		t.Error("payload has no event_id field")
	} else if err := json.Unmarshal(raw, &eventID); err != nil {
		t.Errorf("event_id is not a string: %v", err)
	} else if eventID != row.ID.String() {
		t.Errorf("payload event_id = %s, want the row id %s", eventID, row.ID)
	}
}

// TestIntegration_EnqueueFailureRollsBackTheSubtask is the negative half, and
// the whole reason the outbox exists: if the event cannot be written, the
// subtask must not exist either. Before M1-03 the row was inserted and the
// publish attempted separately, so a failure here left a subtask in
// `implementation` with nothing on the bus to pick it up.
func TestIntegration_EnqueueFailureRollsBackTheSubtask(t *testing.T) {
	pool := integrationPool(t)
	f := newFixture(t, pool)
	wg := workgraph.New(pool)

	boom := errors.New("simulated outbox failure")
	withEnqueueFailure(t, boom)

	taskID := uuid.New()
	err := commitDecomposition(context.Background(), pool, wg, f.parentWorkID, &f.agentDefID, f.planFor(taskID))
	if err == nil {
		t.Fatal("commitDecomposition returned nil despite a failing enqueue")
	}
	if !errors.Is(err, boom) {
		t.Errorf("error = %v, want it to wrap the enqueue failure", err)
	}

	if workItemExists(t, pool, taskID) {
		t.Error("the subtask row survived a failed enqueue — the INSERT and the event are NOT sharing a transaction, which is exactly the orphaned-work defect this ticket closes")
	}
	if rows := rowsForAggregate(t, pool, f.parentWorkID); len(rows) != 0 {
		t.Errorf("outbox rows = %d after a rolled-back transaction, want 0", len(rows))
	}
	if n := planTaskCount(t, pool, f.parentWorkID); n != 0 {
		t.Errorf("plan_tasks rows = %d after a rolled-back transaction, want 0", n)
	}
}

// planTaskCount counts the plan_tasks rows of one plan (M1-06): they are
// written in the decomposition transaction and must roll back with it.
func planTaskCount(t *testing.T, pool *pgxpool.Pool, parentWorkID uuid.UUID) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM plan_tasks WHERE parent_work_id = $1`, parentWorkID).Scan(&n); err != nil {
		t.Fatalf("count plan_tasks: %v", err)
	}
	return n
}

// TestIntegration_DispatcherDeliversTheCommittedEvent runs the real dispatcher
// over the real table and asserts the message the fake bus receives is the one
// the planner used to publish directly, plus the event id in the header.
func TestIntegration_DispatcherDeliversTheCommittedEvent(t *testing.T) {
	pool := integrationPool(t)
	f := newFixture(t, pool)
	wg := workgraph.New(pool)

	taskID := uuid.New()
	taskMsg := f.taskMessageFor(taskID)
	plan := f.planFor(taskID)
	plan[0].msg = taskMsg
	if err := commitDecomposition(context.Background(), pool, wg, f.parentWorkID, &f.agentDefID, plan); err != nil {
		t.Fatalf("commitDecomposition: %v", err)
	}
	committed := rowsForAggregate(t, pool, f.parentWorkID)
	if len(committed) != 1 {
		t.Fatalf("outbox rows = %d, want 1", len(committed))
	}

	fb := &fakeBus{}
	d := outbox.NewDispatcher(outbox.NewStore(pool), fb, outbox.Options{
		ClaimedBy: "m103-test-" + taskID.String()[:8],
	})
	if _, err := d.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	sent := fb.messages()
	if len(sent) != 1 {
		t.Fatalf("published %d messages, want 1", len(sent))
	}
	got := sent[0]
	if got.Topic != bus.TopicImplementationTasks {
		t.Errorf("topic = %q, want %q", got.Topic, bus.TopicImplementationTasks)
	}
	if got.Key != taskID.String() {
		t.Errorf("key = %q, want %q", got.Key, taskID)
	}
	if got.Headers[outbox.HeaderEventID] != committed[0].ID.String() {
		t.Errorf("%s header = %q, want the row id %q", outbox.HeaderEventID, got.Headers[outbox.HeaderEventID], committed[0].ID)
	}
	if string(got.Value) != string(committed[0].Payload) {
		t.Errorf("published payload differs from the committed row:\n got %s\nwant %s", got.Value, committed[0].Payload)
	}
	var decoded implementationTaskMessage
	if err := json.Unmarshal(got.Value, &decoded); err != nil {
		t.Fatalf("decode published payload: %v", err)
	}
	if decoded.TaskID != taskMsg.TaskID || decoded.WorkID != taskMsg.WorkID {
		t.Errorf("published message identifies task %s / work %s, want %s / %s",
			decoded.TaskID, decoded.WorkID, taskMsg.TaskID, taskMsg.WorkID)
	}

	after := rowsForAggregate(t, pool, f.parentWorkID)
	if after[0].PublishedAt == nil {
		t.Error("published_at is still NULL after a successful publish")
	}
}

// TestIntegration_BusFailureKeepsTheRowPendingAndTheSubtask is the property
// that makes the conversion worth doing: when the broker refuses the message,
// the work is not lost. The row stays pending with its attempt counted, and
// the subtask — which is real, committed work — is still there waiting for the
// retry.
func TestIntegration_BusFailureKeepsTheRowPendingAndTheSubtask(t *testing.T) {
	pool := integrationPool(t)
	f := newFixture(t, pool)
	wg := workgraph.New(pool)

	taskID := uuid.New()
	if err := commitDecomposition(context.Background(), pool, wg, f.parentWorkID, &f.agentDefID, f.planFor(taskID)); err != nil {
		t.Fatalf("commitDecomposition: %v", err)
	}

	fb := &fakeBus{failWith: errors.New("broker is down")}
	name := "m103-test-" + taskID.String()[:8]
	d := outbox.NewDispatcher(outbox.NewStore(pool), fb, outbox.Options{ClaimedBy: name})
	if _, err := d.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	rows := rowsForAggregate(t, pool, f.parentWorkID)
	if len(rows) != 1 {
		t.Fatalf("outbox rows = %d, want 1", len(rows))
	}
	if rows[0].PublishedAt != nil {
		t.Error("published_at is set even though the bus refused the message")
	}
	if rows[0].DeadAt != nil {
		t.Error("the row was dead-lettered after a single failure")
	}
	if rows[0].Attempts != 1 {
		t.Errorf("attempts = %d after one failed publish, want 1", rows[0].Attempts)
	}
	if rows[0].LastError == nil || *rows[0].LastError == "" {
		t.Error("last_error is empty; an operator needs to know why it failed")
	}
	if !workItemExists(t, pool, taskID) {
		t.Error("the subtask disappeared because the publish failed — a bus failure must never undo committed work")
	}

	// And it recovers: with a healthy bus the same row is delivered.
	fb.mu.Lock()
	fb.failWith = nil
	fb.mu.Unlock()
	// The first attempt scheduled a backoff, so move next_attempt_at back
	// rather than sleeping through it; the backoff itself is covered by
	// internal/outbox's own tests.
	if _, err := pool.Exec(context.Background(),
		`UPDATE outbox_events SET next_attempt_at = now() - interval '1 minute' WHERE id = $1`, rows[0].ID); err != nil {
		t.Fatalf("clear backoff: %v", err)
	}
	if _, err := d.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce (recovery): %v", err)
	}
	if msgs := fb.messages(); len(msgs) != 1 {
		t.Fatalf("published %d messages after recovery, want 1", len(msgs))
	}
	recovered := rowsForAggregate(t, pool, f.parentWorkID)
	if recovered[0].PublishedAt == nil {
		t.Error("the row is still pending after the bus recovered")
	}
	if recovered[0].Attempts != 1 {
		t.Errorf("attempts = %d after recovery, want the failure history preserved at 1", recovered[0].Attempts)
	}
}

// multiGroupPlan builds a n-group decomposition the way phase 1 does.
func (f fixture) multiGroupPlan(n int) ([]plannedSubtask, []uuid.UUID) {
	plan := make([]plannedSubtask, 0, n)
	ids := make([]uuid.UUID, 0, n)
	for i := 0; i < n; i++ {
		taskID := uuid.New()
		ids = append(ids, taskID)
		sub := f.subtaskFor(taskID)
		sub.Title = fmt.Sprintf("subtask %d", i)
		msg := f.taskMessageFor(taskID)
		msg.Title = sub.Title
		plan = append(plan, plannedSubtask{
			repoName: fmt.Sprintf("repo-%d", i),
			steps:    1,
			titles:   []string{sub.Title},
			item:     sub,
			msg:      msg,
		})
	}
	return plan, ids
}

// TestIntegration_EventsForOnePlanKeepTheirOrder pins the consequence of using
// the parent work item as the aggregate: the tasks of one plan are delivered
// in the order they were planned, because the claim query lets only the oldest
// unpublished row per aggregate through at a time. EnqueueAll writes them with
// clock_timestamp(), so enqueue order survives sharing one transaction.
func TestIntegration_EventsForOnePlanKeepTheirOrder(t *testing.T) {
	pool := integrationPool(t)
	f := newFixture(t, pool)
	wg := workgraph.New(pool)

	plan, ids := f.multiGroupPlan(3)
	if err := commitDecomposition(context.Background(), pool, wg, f.parentWorkID, nil, plan); err != nil {
		t.Fatalf("commitDecomposition: %v", err)
	}

	fb := &fakeBus{}
	d := outbox.NewDispatcher(outbox.NewStore(pool), fb, outbox.Options{ClaimedBy: "m103-order"})
	for i := 0; i < len(ids); i++ {
		if _, err := d.RunOnce(context.Background()); err != nil {
			t.Fatalf("RunOnce %d: %v", i, err)
		}
	}

	sent := fb.messages()
	if len(sent) != len(ids) {
		t.Fatalf("published %d messages, want %d", len(sent), len(ids))
	}
	for i, want := range ids {
		if sent[i].Key != want.String() {
			t.Errorf("message %d key = %q, want %q — the plan's events left the outbox out of order", i, sent[i].Key, want)
		}
	}
}

// TestIntegration_WholeDecompositionCommitsAtomically is the reason the
// transaction is per plan rather than per repo group.
//
// It fails the enqueue, which happens AFTER every subtask INSERT has already
// run — the worst case, and the one a per-group transaction got wrong: there,
// groups 1..n-1 would be committed and already dispatched to the
// implementation agent while the parent was blocked telling an operator to
// retry, and the retry would create a duplicate subtask, agent run, branch and
// PR for every one of them. Nothing deduplicates subtasks, so the only safe
// answer is that a plan is all-or-nothing.
func TestIntegration_WholeDecompositionCommitsAtomically(t *testing.T) {
	pool := integrationPool(t)
	f := newFixture(t, pool)
	wg := workgraph.New(pool)

	plan, ids := f.multiGroupPlan(4)

	// Sanity: the same plan commits cleanly when nothing fails, so the
	// rollback below cannot be passing for the wrong reason.
	if err := commitDecomposition(context.Background(), pool, wg, f.parentWorkID, &f.agentDefID, plan); err != nil {
		t.Fatalf("commitDecomposition (control): %v", err)
	}
	for _, id := range ids {
		if !workItemExists(t, pool, id) {
			t.Fatalf("control run did not create subtask %s", id)
		}
	}
	if rows := rowsForAggregate(t, pool, f.parentWorkID); len(rows) != len(ids) {
		t.Fatalf("control run wrote %d outbox rows, want %d", len(rows), len(ids))
	}
	if n := planTaskCount(t, pool, f.parentWorkID); n != len(ids) {
		t.Fatalf("control run wrote %d plan_tasks rows, want %d", n, len(ids))
	}

	// Now the real case: a fresh plan whose enqueue fails on the last group.
	failing, failingIDs := f.multiGroupPlan(4)
	withEnqueueFailure(t, errors.New("simulated outbox failure on the last group"))

	if err := commitDecomposition(context.Background(), pool, wg, f.parentWorkID, &f.agentDefID, failing); err == nil {
		t.Fatal("commitDecomposition returned nil despite a failing enqueue")
	}

	for i, id := range failingIDs {
		if workItemExists(t, pool, id) {
			t.Errorf("subtask %d (%s) survived a failed decomposition — the plan is not committing atomically, so a retry would duplicate it", i, id)
		}
	}
	// Only the control run's rows remain; the failed plan added none.
	if rows := rowsForAggregate(t, pool, f.parentWorkID); len(rows) != len(ids) {
		t.Errorf("outbox rows = %d after a rolled-back decomposition, want the control run's %d", len(rows), len(ids))
	}
	if n := planTaskCount(t, pool, f.parentWorkID); n != len(ids) {
		t.Errorf("plan_tasks rows = %d after a rolled-back decomposition, want the control run's %d", n, len(ids))
	}
	for i, id := range failingIDs {
		var exists bool
		if err := pool.QueryRow(context.Background(), `SELECT EXISTS(SELECT 1 FROM plan_tasks WHERE id = $1)`, id).Scan(&exists); err != nil {
			t.Fatal(err)
		}
		if exists {
			t.Errorf("plan_tasks row for failed-plan subtask %d (%s) survived the rollback", i, id)
		}
	}
}
