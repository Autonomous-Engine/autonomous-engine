// cmd/knowledge-indexer/revoked_consumer_test.go
package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/bus"
	"github.com/autonomousengine/autonomous-engine/internal/interactions"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// cleanupTestProduct tears down a fixture product and every row that
// references it. Opens a FRESH pool from TEST_POSTGRES_DSN because
// t.Cleanup runs after the test's `defer p.Close()` — so the primary pool
// would already be closed by the time this fires.
func cleanupTestProduct(_ *pgxpool.Pool, productID uuid.UUID) {
	dsn := os.Getenv("TEST_POSTGRES_DSN")
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
	_, _ = pool.Exec(ctx, `DELETE FROM repositories WHERE product_id = $1`, productID)
	_, _ = pool.Exec(ctx, `DELETE FROM outbox_events WHERE aggregate_id = $1`, productID)
	_, _ = pool.Exec(ctx, `DELETE FROM products WHERE id = $1`, productID)
}

func revokeTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN not set — skipping DB-integration test")
	}
	p, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	return p
}

// qdrantPayloadRecorder records payload-set requests so tests can assert the
// consumer hit the right collection with the right point_id.
type qdrantPayloadRecorder struct {
	mu     sync.Mutex
	bodies []qdrantSetCall
}

type qdrantSetCall struct {
	Collection string
	Body       string
}

func newFakeQdrantForRevoke(rec *qdrantPayloadRecorder) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Accept any /collections/<name>/points/payload POST.
		if r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/points/payload") {
			var body [1024]byte
			n, _ := r.Body.Read(body[:])
			coll := strings.TrimPrefix(r.URL.Path, "/collections/")
			coll = strings.TrimSuffix(coll, "/points/payload")
			rec.mu.Lock()
			rec.bodies = append(rec.bodies, qdrantSetCall{Collection: coll, Body: string(body[:n])})
			rec.mu.Unlock()
			_, _ = w.Write([]byte(`{"result":{"status":"completed","operation_id":1}}`))
			return
		}
		// DescribeCollection 200 — avoid creation-on-first-call path.
		_, _ = w.Write([]byte(`{"result":{"config":{"params":{"vectors":{"size":768,"distance":"Cosine"}}}}}`))
	}))
}

func TestRevokedConsumer_MarksCodexRevokedAndFlipsQdrant(t *testing.T) {
	p := revokeTestPool(t)
	defer p.Close()
	ctx := context.Background()

	// Fixtures: product, work_item, user_interaction WITH codex_entry + qdrant_point_id.
	productID := uuid.New()
	_, err := p.Exec(ctx, `INSERT INTO products (id, name, created_at, updated_at) VALUES ($1, $2, NOW(), NOW())`, productID, "fx-rev-"+productID.String()[:8])
	if err != nil {
		t.Fatalf("product: %v", err)
	}
	t.Cleanup(func() { cleanupTestProduct(p, productID) })

	// Set up repository with base_branch so CreateVersion works.
	repoID := uuid.New()
	_, _ = p.Exec(ctx, `
		INSERT INTO repositories (id, product_id, name, github_url, base_branch, created_at, updated_at)
		VALUES ($1, $2, 'test-repo', 'https://example.com/repo.git', 'main', NOW(), NOW())
	`, repoID, productID)

	workItemID := uuid.New()
	if _, err := p.Exec(ctx, `
		INSERT INTO work_items (id, product_id, title, state, type, priority, created_at, updated_at)
		VALUES ($1, $2, 'fx', 'intake', 'feature', 'medium', NOW(), NOW())
	`, workItemID, productID); err != nil {
		t.Fatalf("wi: %v", err)
	}

	promptID := uuid.New()
	codexEntryID := uuid.New()
	pointID := "qpt-rev-" + uuid.New().String()[:8]
	if _, err := p.Exec(ctx, `
		INSERT INTO user_interactions (
			id, work_item_id, product_id, asking_agent, trigger, prompt_type,
			headline, question, state, requested_at, answered_at,
			answered_by, answered_via, answer_text,
			classified_as, codex_entry_id, codex_entry_table, qdrant_point_id
		) VALUES ($1, $2, $3, 'impl', 'agent_request', 'clarification',
			'h', 'q', 'answered', NOW(), NOW(),
			'admin@localhost', 'dashboard', 'use GUIDs',
			'codex_antipattern', $4, 'codex_anti_patterns', $5)
	`, promptID, workItemID, productID, codexEntryID, pointID); err != nil {
		t.Fatalf("interaction: %v", err)
	}
	// Create the codex entry that we'll be revoking.
	if _, err := p.Exec(ctx, `
		INSERT INTO codex_anti_patterns (id, product_id, name, source, source_interaction_id, state)
		VALUES ($1, $2, 'no int IDs', 'user_interaction', $3, 'active')
	`, codexEntryID, productID, promptID); err != nil {
		t.Fatalf("codex row: %v", err)
	}

	rec := &qdrantPayloadRecorder{}
	srv := newFakeQdrantForRevoke(rec)
	defer srv.Close()

	h := NewRevokedConsumer(p, srv.URL)

	payload := interactions.RevokedPayload{
		PromptID:                promptID,
		WorkItemID:              workItemID,
		CodexEntryIDToDeprecate: &codexEntryID,
		RevokedBy:               "admin@localhost",
		RevocationReason:        "wrong answer",
		RevokedAt:               time.Now().UTC().Format(time.RFC3339),
	}
	body, _ := json.Marshal(payload)
	if err := h.Handle(ctx, &bus.Message{Value: body}); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	// 1. Codex entry state flipped to revoked.
	var state string
	if err := p.QueryRow(ctx, `SELECT state FROM codex_anti_patterns WHERE id = $1`, codexEntryID).Scan(&state); err != nil {
		t.Fatalf("read codex state: %v", err)
	}
	if state != "revoked" {
		t.Errorf("codex state = %q, want revoked", state)
	}

	// 2. Qdrant hit with the right collection + point_id, payload.revoked=true.
	if len(rec.bodies) != 1 {
		t.Fatalf("qdrant payload set calls = %d, want 1", len(rec.bodies))
	}
	expectedColl := "product_" + productID.String() + "_user_decisions"
	if rec.bodies[0].Collection != expectedColl {
		t.Errorf("qdrant collection = %q, want %q", rec.bodies[0].Collection, expectedColl)
	}
	if !strings.Contains(rec.bodies[0].Body, pointID) {
		t.Errorf("qdrant body missing point_id %q; got %s", pointID, rec.bodies[0].Body)
	}
	if !strings.Contains(rec.bodies[0].Body, `"revoked"`) || !strings.Contains(rec.bodies[0].Body, `"true"`) {
		t.Errorf("qdrant body missing revoked=true; got %s", rec.bodies[0].Body)
	}

	// 3. Codex version bumped, typed (migration 060), with codex.indexed.
	var versionCount int
	_ = p.QueryRow(ctx, `SELECT COUNT(*) FROM codex_versions WHERE product_id = $1`, productID).Scan(&versionCount)
	if versionCount < 1 {
		t.Errorf("expected >=1 codex_versions for product, got %d", versionCount)
	}
	var sections []string
	_ = p.QueryRow(ctx, `SELECT sections_changed FROM codex_versions WHERE product_id = $1 ORDER BY product_version DESC LIMIT 1`, productID).Scan(&sections)
	if len(sections) != 1 || sections[0] != "anti_patterns" {
		t.Errorf("revoke version sections = %v, want [anti_patterns]", sections)
	}
	var trigger string
	if err := p.QueryRow(ctx, `SELECT payload->>'trigger' FROM outbox_events WHERE topic = $1 AND aggregate_id = $2`, bus.TopicCodexIndexed, productID).Scan(&trigger); err != nil || trigger != "interaction_revoked" {
		t.Errorf("codex.indexed trigger = %q, %v", trigger, err)
	}
}

func TestRevokedConsumer_NoCodexEntry_StillOK(t *testing.T) {
	// Revoke fires before triage wrote a codex entry — payload.CodexEntryIDToDeprecate is nil.
	// Handler must not fail.
	p := revokeTestPool(t)
	defer p.Close()
	ctx := context.Background()

	productID := uuid.New()
	_, _ = p.Exec(ctx, `INSERT INTO products (id, name, created_at, updated_at) VALUES ($1, $2, NOW(), NOW())`, productID, "fx-"+productID.String()[:8])
	t.Cleanup(func() { cleanupTestProduct(p, productID) })
	workItemID := uuid.New()
	_, _ = p.Exec(ctx, `
		INSERT INTO work_items (id, product_id, title, state, type, priority, created_at, updated_at)
		VALUES ($1, $2, 'fx', 'intake', 'feature', 'medium', NOW(), NOW())
	`, workItemID, productID)
	promptID := uuid.New()
	_, _ = p.Exec(ctx, `
		INSERT INTO user_interactions (id, work_item_id, product_id, asking_agent, trigger, prompt_type,
			headline, question, state, requested_at)
		VALUES ($1, $2, $3, 'impl', 'agent_request', 'clarification', 'h', 'q', 'pending', NOW())
	`, promptID, workItemID, productID)

	rec := &qdrantPayloadRecorder{}
	srv := newFakeQdrantForRevoke(rec)
	defer srv.Close()

	h := NewRevokedConsumer(p, srv.URL)
	payload := interactions.RevokedPayload{
		PromptID:                promptID,
		WorkItemID:              workItemID,
		CodexEntryIDToDeprecate: nil,
		RevokedBy:               "admin@localhost",
	}
	body, _ := json.Marshal(payload)
	if err := h.Handle(ctx, &bus.Message{Value: body}); err != nil {
		t.Errorf("revoke without codex entry must not error: %v", err)
	}
}

func TestRevokedConsumer_BadPayloadSilentlyDropped(t *testing.T) {
	// Ensures unparseable payloads don't cause Kafka redelivery.
	h := NewRevokedConsumer(nil, "http://unused")
	if err := h.Handle(context.Background(), &bus.Message{Value: []byte("not json")}); err != nil {
		t.Errorf("bad payload should not error: %v", err)
	}
}
