package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/bus"
	"github.com/autonomousengine/autonomous-engine/internal/db"
	"github.com/autonomousengine/autonomous-engine/internal/outbox"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// These tests drive the webhook handlers against a real PostgreSQL and a fake
// bus: an accepted webhook leaves a committed outbox row on the repository
// aggregate, and the dispatcher delivers exactly that message with its
// event_id.
//
//	docker run ... -e TEST_POSTGRES_DSN=... golang:1.22-alpine \
//	    sh -c "go test ./cmd/webhook-listener/ -run Integration -v"
//
// They skip when TEST_POSTGRES_DSN / TEST_DATABASE_URL is unset.

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

type recordingBus struct {
	mu   sync.Mutex
	sent []bus.Message
}

func (b *recordingBus) PublishMessage(_ context.Context, msg *bus.Message) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	c := *msg
	c.Headers = map[string]string{}
	for k, v := range msg.Headers {
		c.Headers[k] = v
	}
	b.sent = append(b.sent, c)
	return nil
}

func TestIntegration_WebhookEventsAreEnqueuedOnTheRepositoryAggregateAndDelivered(t *testing.T) {
	pool := integrationPool(t)
	ctx := context.Background()

	productID, repoID := uuid.New(), uuid.New()
	suffix := productID.String()[:8]
	fullName := "m104e/widgets-" + suffix
	if _, err := pool.Exec(ctx, `INSERT INTO products (id, name) VALUES ($1, $2)`, productID, "m104e-"+suffix); err != nil {
		t.Fatalf("seed product: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO repositories (id, product_id, name, github_url, clone_path, base_branch)
		VALUES ($1, $2, $3, $4, $5, 'main')`,
		repoID, productID, "m104e-repo-"+suffix, "https://github.com/"+fullName, "/tmp/m104e-"+suffix); err != nil {
		t.Fatalf("seed repository: %v", err)
	}
	t.Cleanup(func() {
		cctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, _ = pool.Exec(cctx, `DELETE FROM outbox_events WHERE aggregate_id = $1`, repoID)
		_, _ = pool.Exec(cctx, `DELETE FROM products WHERE id = $1`, productID)
	})

	post := func(event string, body any) int {
		raw, _ := json.Marshal(body)
		req := httptest.NewRequest(http.MethodPost, "/webhook/github", bytes.NewReader(raw))
		req.Header.Set("X-GitHub-Event", event)
		req.Header.Set("X-GitHub-Delivery", "m104e-it-"+event+"-"+suffix)
		signGitHub(req, raw, testWebhookSecret)
		rec := httptest.NewRecorder()
		githubWebhookHandler(ctx, pool, envSecrets(testWebhookSecret))(rec, req)
		return rec.Code
	}

	workID := uuid.New()
	pushBody := map[string]any{
		"ref": "refs/heads/main", "after": "sha-" + suffix,
		// Upper-case: the repository lookup ignores case.
		"repository":  map[string]any{"full_name": strings.ToUpper(fullName)},
		"head_commit": map[string]any{"message": "human change"},
	}
	if code := post("push", pushBody); code != http.StatusOK {
		t.Fatalf("push status = %d", code)
	}
	// GitHub redelivers the same delivery: same X-GitHub-Delivery, so the
	// same event id, so ON CONFLICT (id) DO NOTHING makes it a no-op.
	if code := post("push", pushBody); code != http.StatusOK {
		t.Fatalf("redelivered push status = %d", code)
	}
	if code := post("pull_request", map[string]any{
		"action": "closed",
		"pull_request": map[string]any{
			"number": 7, "title": "t", "merged": true,
			"head": map[string]any{"ref": "agent/" + workID.String()},
		},
		"repository": map[string]any{"full_name": fullName},
	}); code != http.StatusOK {
		t.Fatalf("pull_request status = %d", code)
	}

	// Both events are committed on the repository aggregate, in order.
	rows, err := pool.Query(ctx, `SELECT id, aggregate_type, topic, key FROM outbox_events
		WHERE aggregate_id = $1 ORDER BY created_at, id`, repoID)
	if err != nil {
		t.Fatalf("query outbox: %v", err)
	}
	type row struct {
		id                  uuid.UUID
		aggType, topic, key string
	}
	var got []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.aggType, &r.topic, &r.key); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, r)
	}
	rows.Close()
	if len(got) != 2 {
		t.Fatalf("outbox rows on the repository aggregate = %d, want 2: %+v", len(got), got)
	}
	if got[0].topic != bus.TopicExternalChangeDetected || got[0].key != "sha-"+suffix {
		t.Errorf("first row = %+v, want external-change.detected keyed by the sha", got[0])
	}
	if got[1].topic != bus.TopicPRMerged || got[1].key != "pr-7" {
		t.Errorf("second row = %+v, want pr.merged keyed pr-7", got[1])
	}
	for _, r := range got {
		if r.aggType != aggregateTypeRepository {
			t.Errorf("aggregate_type = %q, want %q", r.aggType, aggregateTypeRepository)
		}
	}

	// The dispatcher delivers both, each carrying its row id as event_id.
	fb := &recordingBus{}
	name := "webhook-listener-it/" + suffix
	d := outbox.NewDispatcher(outbox.NewStore(pool), fb, outbox.Options{ClaimedBy: name})
	for i := 0; i < 5 && len(fb.sent) < 2; i++ {
		if _, err := d.RunOnce(ctx); err != nil {
			t.Fatalf("RunOnce: %v", err)
		}
	}
	byID := map[string]bus.Message{}
	for _, m := range fb.sent {
		byID[m.Headers[outbox.HeaderEventID]] = m
	}
	for _, r := range got {
		m, ok := byID[r.id.String()]
		if !ok {
			t.Errorf("event %s (%s) was not delivered", r.id, r.topic)
			continue
		}
		if m.Topic != r.topic || m.Key != r.key {
			t.Errorf("delivered %s/%s, want %s/%s", m.Topic, m.Key, r.topic, r.key)
		}
		var payload map[string]any
		if err := json.Unmarshal(m.Value, &payload); err != nil {
			t.Fatalf("decode delivered payload: %v", err)
		}
		if payload[outbox.FieldEventID] != r.id.String() {
			t.Errorf("payload event_id = %v, want %s", payload[outbox.FieldEventID], r.id)
		}
		if r.topic == bus.TopicPRMerged && payload["work_id"] != workID.String() {
			t.Errorf("pr.merged work_id = %v, want %s", payload["work_id"], workID)
		}
	}
	var published int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM outbox_events
		WHERE aggregate_id = $1 AND published_at IS NOT NULL`, repoID).Scan(&published); err != nil {
		t.Fatalf("count published: %v", err)
	}
	if published != 2 {
		t.Errorf("published rows = %d, want 2", published)
	}
}

func TestIntegration_DeadLetterRetryEnqueuesAndResolvesTogether(t *testing.T) {
	pool := integrationPool(t)
	ctx := context.Background()

	var id string
	if err := pool.QueryRow(ctx, `INSERT INTO webhook_dead_letters (source, event_type, payload, error_message, next_retry_at)
		VALUES ('github', 'push', '{"ref":"refs/heads/main"}', 'it', NOW()) RETURNING id`).Scan(&id); err != nil {
		t.Fatalf("seed dead letter: %v", err)
	}
	dlID := uuid.MustParse(id)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM outbox_events WHERE aggregate_id = $1`, dlID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM webhook_dead_letters WHERE id = $1`, dlID)
	})

	processDeadLetters(ctx, pool)

	var resolved bool
	if err := pool.QueryRow(ctx, `SELECT resolved FROM webhook_dead_letters WHERE id = $1`, dlID).Scan(&resolved); err != nil {
		t.Fatalf("read dead letter: %v", err)
	}
	var topic, aggType string
	if err := pool.QueryRow(ctx, `SELECT topic, aggregate_type FROM outbox_events WHERE aggregate_id = $1`, dlID).Scan(&topic, &aggType); err != nil {
		t.Fatalf("read outbox row: %v", err)
	}
	if !resolved || topic != bus.TopicExternalChangeDetected || aggType != aggregateTypeDeadLetter {
		t.Errorf("resolved=%v topic=%s aggregate_type=%s", resolved, topic, aggType)
	}
}
