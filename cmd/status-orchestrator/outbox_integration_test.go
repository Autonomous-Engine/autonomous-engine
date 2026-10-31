package main

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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
	"github.com/segmentio/kafka-go"
)

// These tests drive status-orchestrator's M1-04a write paths against a real
// PostgreSQL (TEST_POSTGRES_DSN, a disposable database: they migrate it) and,
// for delivery, the compose Kafka (TEST_KAFKA_BROKERS). They prove what no
// unit test can: the transition row, the prompt/subtask rows and their
// outbox_events rows share one commit, a failure leaves none of them, and the
// dispatcher delivers the committed event to Kafka carrying its event_id.
//
// The delivery test enqueues to a fresh single-partition test topic rather
// than qa.ready, so the running stack's qa-agent never sees a fixture event.
//
// Run with the repo's docker command:
//
//	docker run ... -e TEST_POSTGRES_DSN=... -e TEST_KAFKA_BROKERS=kafka:9092 \
//	    golang:1.22-alpine sh -c "go test ./cmd/status-orchestrator/ -run Integration -v"

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

// seedItem creates a product and a work item in state, and removes both (and
// the item's children, transitions, prompts and outbox rows) afterwards.
func seedItem(t *testing.T, pool *pgxpool.Pool, state models.WorkItemState) (itemID, productID uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	itemID, productID = uuid.New(), uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO products (id, name) VALUES ($1, $2)`, productID, "m104a-test-"+productID.String()[:8]); err != nil {
		t.Fatalf("seed product: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO work_items (id, type, state, title, description, product_id)
		VALUES ($1, 'feature', $2, 'M1-04a item', 'acceptance criteria', $3)`, itemID, string(state), productID); err != nil {
		t.Fatalf("seed work item: %v", err)
	}
	t.Cleanup(func() {
		cctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		for _, sql := range []string{
			`DELETE FROM outbox_events WHERE aggregate_id = $1`,
			`UPDATE work_items SET awaiting_prompt_id = NULL WHERE id = $1 OR parent_id = $1`,
			`DELETE FROM user_interactions WHERE work_item_id = $1 OR work_item_id IN (SELECT id FROM work_items WHERE parent_id = $1)`,
			`DELETE FROM work_items WHERE parent_id = $1`,
			`DELETE FROM work_items WHERE id = $1`,
		} {
			if _, err := pool.Exec(cctx, sql, itemID); err != nil {
				t.Logf("cleanup %q: %v", sql, err)
			}
		}
		if _, err := pool.Exec(cctx, `DELETE FROM products WHERE id = $1`, productID); err != nil {
			t.Logf("cleanup product: %v", err)
		}
	})
	return itemID, productID
}

func itemState(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) string {
	t.Helper()
	var s string
	if err := pool.QueryRow(context.Background(), `SELECT state::text FROM work_items WHERE id = $1`, id).Scan(&s); err != nil {
		t.Fatalf("read state: %v", err)
	}
	return s
}

func count(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatalf("count (%s): %v", sql, err)
	}
	return n
}

type committedEvent struct {
	ID          uuid.UUID
	Aggregate   uuid.UUID
	Topic       string
	Key         string
	Payload     []byte
	PublishedAt *time.Time
	ClaimedBy   *string
}

func eventsFor(t *testing.T, pool *pgxpool.Pool, aggregateID uuid.UUID) []committedEvent {
	t.Helper()
	rows, err := pool.Query(context.Background(), `
		SELECT id, aggregate_id, topic, key, payload, published_at, claimed_by
		FROM outbox_events WHERE aggregate_id = $1 ORDER BY created_at, id`, aggregateID)
	if err != nil {
		t.Fatalf("select outbox: %v", err)
	}
	defer rows.Close()
	var out []committedEvent
	for rows.Next() {
		var e committedEvent
		if err := rows.Scan(&e.ID, &e.Aggregate, &e.Topic, &e.Key, &e.Payload, &e.PublishedAt, &e.ClaimedBy); err != nil {
			t.Fatalf("scan outbox: %v", err)
		}
		out = append(out, e)
	}
	return out
}

// kafkaBrokers returns TEST_KAFKA_BROKERS, or nil when unset.
func kafkaBrokers() []string {
	var out []string
	for _, b := range strings.Split(os.Getenv("TEST_KAFKA_BROKERS"), ",") {
		if b = strings.TrimSpace(b); b != "" {
			out = append(out, b)
		}
	}
	return out
}

// createTestTopic creates a fresh single-partition topic on the controller.
func createTestTopic(t *testing.T, brokers []string) string {
	t.Helper()
	topic := "m104a.test." + strings.ReplaceAll(uuid.NewString(), "-", "")
	conn, err := kafka.Dial("tcp", brokers[0])
	if err != nil {
		t.Fatalf("dial %s: %v", brokers[0], err)
	}
	ctrl, err := conn.Controller()
	conn.Close()
	if err != nil {
		t.Fatalf("controller: %v", err)
	}
	cc, err := kafka.Dial("tcp", net.JoinHostPort(ctrl.Host, strconv.Itoa(ctrl.Port)))
	if err != nil {
		t.Fatalf("dial controller: %v", err)
	}
	defer cc.Close()
	if err := cc.CreateTopics(kafka.TopicConfig{Topic: topic, NumPartitions: 1, ReplicationFactor: 1}); err != nil {
		t.Fatalf("create topic: %v", err)
	}
	t.Cleanup(func() {
		if c, err := kafka.Dial("tcp", brokers[0]); err == nil {
			_ = c.DeleteTopics(topic)
			c.Close()
		}
	})
	return topic
}

// TestIntegration_TransitionAndEventCommitTogetherAndDeliver drives
// transitionAndPublish: the work_item_transitions row and the outbox row
// commit together, state.changed is published after the commit, and the
// dispatcher delivers the event to Kafka with its event_id in the header
// and the payload.
func TestIntegration_TransitionAndEventCommitTogetherAndDeliver(t *testing.T) {
	pool := integrationPool(t)
	brokers := kafkaBrokers()
	if len(brokers) == 0 {
		t.Skip("TEST_KAFKA_BROKERS not set — skipping delivery test")
	}
	ctx := context.Background()
	wg := workgraph.New(pool)
	itemID, _ := seedItem(t, pool, models.WorkItemStateHumanReview)
	topic := createTestTopic(t, brokers)

	j := &journal{}
	rb := &recordingBus{j: j}
	msg := qaReadyMessage{WorkID: itemID.String(), Title: "M1-04a item", AcceptanceCriteria: "acceptance criteria", Branch: "feature/x", HeadSHA: "abc123"}
	if err := transitionAndPublish(ctx, pool, wg, rb, itemID, models.WorkItemStateQA, "review approved, auto-merge allowed",
		enqueueEffect(topic, itemID.String(), msg)); err != nil {
		t.Fatalf("transitionAndPublish: %v", err)
	}

	if got := itemState(t, pool, itemID); got != string(models.WorkItemStateQA) {
		t.Errorf("state = %s, want qa", got)
	}
	if n := count(t, pool, `SELECT count(*) FROM work_item_transitions WHERE work_item_id = $1 AND from_state = 'human_review' AND to_state = 'qa'`, itemID); n != 1 {
		t.Errorf("transition rows = %d, want 1", n)
	}
	evs := eventsFor(t, pool, itemID)
	if len(evs) != 1 || evs[0].Topic != topic || evs[0].Key != itemID.String() || evs[0].PublishedAt != nil {
		t.Fatalf("outbox rows = %+v, want one unpublished event for the test topic keyed by the item", evs)
	}
	if len(rb.sent) != 1 || rb.sent[0].Topic != topicStateChanged {
		t.Fatalf("direct publishes = %+v, want exactly state.changed", rb.sent)
	}

	kb, err := bus.NewKafkaBus(brokers)
	if err != nil {
		t.Fatalf("NewKafkaBus: %v", err)
	}
	defer kb.Close() //nolint:errcheck
	d := outbox.NewDispatcher(outbox.NewStore(pool), kb, outbox.Options{ClaimedBy: "status-orchestrator/m104a-test"})
	if _, err := d.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	r := kafka.NewReader(kafka.ReaderConfig{Brokers: brokers, Topic: topic, Partition: 0, MinBytes: 1, MaxBytes: 10e6})
	defer r.Close()
	if err := r.SetOffset(kafka.FirstOffset); err != nil {
		t.Fatalf("set offset: %v", err)
	}
	rctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	m, err := r.ReadMessage(rctx)
	if err != nil {
		t.Fatalf("read delivered message: %v", err)
	}
	if string(m.Key) != itemID.String() {
		t.Errorf("key = %q, want %s", m.Key, itemID)
	}
	header := ""
	for _, h := range m.Headers {
		if h.Key == outbox.HeaderEventID {
			header = string(h.Value)
		}
	}
	if header != evs[0].ID.String() {
		t.Errorf("event_id header = %q, want %s", header, evs[0].ID)
	}
	var delivered map[string]any
	if err := json.Unmarshal(m.Value, &delivered); err != nil {
		t.Fatalf("decode delivered: %v", err)
	}
	if delivered[outbox.FieldEventID] != evs[0].ID.String() || delivered["work_id"] != itemID.String() || delivered["head_sha"] != "abc123" {
		t.Errorf("delivered payload = %v", delivered)
	}
	after := eventsFor(t, pool, itemID)
	// MarkPublished releases the claim, so only published_at is left to see.
	if after[0].PublishedAt == nil {
		t.Errorf("after dispatch: published_at is still NULL")
	}
}

// TestIntegration_TransitionRollbackLeavesNeither: an effect that fails after
// the transition and an enqueue rolls both back, and nothing is published.
func TestIntegration_TransitionRollbackLeavesNeither(t *testing.T) {
	pool := integrationPool(t)
	ctx := context.Background()
	wg := workgraph.New(pool)
	itemID, _ := seedItem(t, pool, models.WorkItemStateHumanReview)

	rb := &recordingBus{j: &journal{}}
	boom := errors.New("later write failed")
	err := transitionAndPublish(ctx, pool, wg, rb, itemID, models.WorkItemStateQA, "r",
		enqueueEffect(bus.TopicQAReady, itemID.String(), qaReadyMessage{WorkID: itemID.String()}),
		func(context.Context, pgx.Tx, *models.WorkItem) error { return boom })
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want %v", err, boom)
	}
	if got := itemState(t, pool, itemID); got != string(models.WorkItemStateHumanReview) {
		t.Errorf("state = %s after rollback, want human_review", got)
	}
	if n := count(t, pool, `SELECT count(*) FROM work_item_transitions WHERE work_item_id = $1`, itemID); n != 0 {
		t.Errorf("transition rows = %d after rollback, want 0", n)
	}
	if evs := eventsFor(t, pool, itemID); len(evs) != 0 {
		t.Errorf("outbox rows = %d after rollback, want 0", len(evs))
	}
	if len(rb.sent) != 0 {
		t.Errorf("published %d messages after rollback, want 0", len(rb.sent))
	}
}

// TestIntegration_WorkBlockedIsIdempotent: the first work.blocked blocks the
// item and raises one prompt with its event in the same commit; a second
// delivery (or the event a converted guard block emits for an item it has
// already blocked) makes no transition and raises no second prompt.
func TestIntegration_WorkBlockedIsIdempotent(t *testing.T) {
	pool := integrationPool(t)
	ctx := context.Background()
	wg := workgraph.New(pool)
	itemID, _ := seedItem(t, pool, models.WorkItemStateImplementation)
	rb := &recordingBus{j: &journal{}}

	for i := 0; i < 2; i++ {
		if err := blockFromWorkBlocked(ctx, pool, wg, rb, itemID, "guard rejected the change"); err != nil {
			t.Fatalf("delivery %d: %v", i+1, err)
		}
	}
	if got := itemState(t, pool, itemID); got != string(models.WorkItemStateBlocked) {
		t.Errorf("state = %s, want blocked", got)
	}
	if n := count(t, pool, `SELECT count(*) FROM work_item_transitions WHERE work_item_id = $1 AND to_state = 'blocked'`, itemID); n != 1 {
		t.Errorf("blocked transitions = %d, want 1", n)
	}
	if n := count(t, pool, `SELECT count(*) FROM user_interactions WHERE work_item_id = $1 AND trigger = 'blocked' AND state = 'pending'`, itemID); n != 1 {
		t.Errorf("pending blocked prompts = %d, want 1", n)
	}
	evs := eventsFor(t, pool, itemID)
	if len(evs) != 1 || evs[0].Topic != bus.TopicUserPromptRequested || evs[0].Key != itemID.String() {
		t.Errorf("outbox rows = %+v, want one user_prompt.requested", evs)
	}
	if len(rb.sent) != 1 {
		t.Errorf("state.changed publishes = %d, want 1", len(rb.sent))
	}

	// An item another service already blocked, with no prompt yet, gets the
	// generic prompt but no transition.
	other, _ := seedItem(t, pool, models.WorkItemStateBlocked)
	if err := blockFromWorkBlocked(ctx, pool, wg, rb, other, "blocked by a converted guard"); err != nil {
		t.Fatalf("already blocked: %v", err)
	}
	if n := count(t, pool, `SELECT count(*) FROM work_item_transitions WHERE work_item_id = $1`, other); n != 0 {
		t.Errorf("transitions for an already-blocked item = %d, want 0", n)
	}
	if n := count(t, pool, `SELECT count(*) FROM user_interactions WHERE work_item_id = $1 AND state = 'pending'`, other); n != 1 {
		t.Errorf("prompts for an already-blocked item = %d, want 1", n)
	}
}

// TestIntegration_FixSubtaskAndEventCommitTogether: the review-fix / QA-fix
// subtask row and its implementation.tasks event share one commit.
func TestIntegration_FixSubtaskAndEventCommitTogether(t *testing.T) {
	pool := integrationPool(t)
	ctx := context.Background()
	wg := workgraph.New(pool)
	parentID, productID := seedItem(t, pool, models.WorkItemStateImplementation)

	fixID := uuid.New()
	fix := &models.WorkItem{ID: fixID, Type: models.WorkItemTypeSubtask, State: models.WorkItemStateImplementation,
		Title: "Address review feedback", Description: "fix it", Priority: models.PriorityHigh, ParentID: &parentID, ProductID: &productID}
	msg := map[string]interface{}{"task_id": fixID.String(), "work_id": parentID.String(), "repository_id": "", "is_review_fix": true}
	if err := dispatchFixSubtask(ctx, pool, wg, parentID, fix, msg); err != nil {
		t.Fatalf("dispatchFixSubtask: %v", err)
	}
	if n := count(t, pool, `SELECT count(*) FROM work_items WHERE id = $1 AND parent_id = $2`, fixID, parentID); n != 1 {
		t.Errorf("fix subtask rows = %d, want 1", n)
	}
	evs := eventsFor(t, pool, parentID)
	if len(evs) != 1 || evs[0].Topic != bus.TopicImplementationTasks || evs[0].Key != parentID.String() {
		t.Fatalf("outbox rows = %+v, want one implementation.tasks keyed by the parent", evs)
	}

	// A duplicate id fails the insert: neither a second row nor an event.
	if err := dispatchFixSubtask(ctx, pool, wg, parentID, fix, msg); err == nil {
		t.Fatal("dispatching the same subtask id twice succeeded")
	}
	if evs := eventsFor(t, pool, parentID); len(evs) != 1 {
		t.Errorf("outbox rows = %d after a failed dispatch, want still 1", len(evs))
	}
}
