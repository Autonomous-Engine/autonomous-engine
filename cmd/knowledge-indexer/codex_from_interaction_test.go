package main

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/bus"
	"github.com/autonomousengine/autonomous-engine/internal/interactions"
	"github.com/autonomousengine/autonomous-engine/internal/vector"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func codexTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN not set — skipping DB-integration test")
	}
	p, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("pgxpool: %v", err)
	}
	return p
}

// mustInsertFixtureProductForCodex inserts a minimal products row + repositories row with a base_branch.
// Registers a t.Cleanup that tears down the product and all referencing rows.
func mustInsertFixtureProductForCodex(t *testing.T, p *pgxpool.Pool) uuid.UUID {
	t.Helper()
	id := uuid.New()
	ctx := context.Background()
	_, err := p.Exec(ctx, `INSERT INTO products (id, name, created_at, updated_at) VALUES ($1, $2, NOW(), NOW())`, id, "fx-"+id.String()[:8])
	if err != nil {
		t.Fatalf("fixture product: %v", err)
	}
	t.Cleanup(func() {
		// Fresh pool: test's primary pool may already be closed when
		// t.Cleanup runs (deferred p.Close() runs first).
		cleanupTestProduct(nil, id)
	})
	repoID := uuid.New()
	_, err = p.Exec(context.Background(), `
		INSERT INTO repositories (id, product_id, name, github_url, base_branch, merge_strategy, clone_path, created_at)
		VALUES ($1, $2, 'fx-repo', 'https://github.com/example/repo', 'main', 'squash', '/tmp/repo', NOW())
	`, repoID, id)
	if err != nil {
		t.Fatalf("fixture repository: %v", err)
	}
	return id
}

// mustInsertFixtureInteractionForCodex inserts minimal work_items + user_interactions rows
// so a codex entry's source_interaction_id FK resolves.
func mustInsertFixtureInteractionForCodex(t *testing.T, p *pgxpool.Pool, productID uuid.UUID) uuid.UUID {
	t.Helper()
	workItemID := uuid.New()
	if _, err := p.Exec(context.Background(), `
		INSERT INTO work_items (id, product_id, title, state, type, priority, created_at, updated_at)
		VALUES ($1, $2, $3, 'intake', 'feature', 'medium', NOW(), NOW())
	`, workItemID, productID, "fx wi"); err != nil {
		t.Fatalf("fixture wi: %v", err)
	}
	promptID := uuid.New()
	if _, err := p.Exec(context.Background(), `
		INSERT INTO user_interactions (id, work_item_id, product_id, asking_agent, trigger, prompt_type,
			headline, question, state, requested_at)
		VALUES ($1, $2, $3, 'implementation-agent', 'agent_request', 'clarification',
			'h', 'q', 'answered', NOW())
	`, promptID, workItemID, productID); err != nil {
		t.Fatalf("fixture interaction: %v", err)
	}
	return promptID
}

func TestCodexFromInteractionHandler_WritesAntiPatternAndBumpsVersion(t *testing.T) {
	p := codexTestPool(t)
	defer p.Close()
	ctx := context.Background()

	productID := mustInsertFixtureProductForCodex(t, p)
	interactionID := mustInsertFixtureInteractionForCodex(t, p, productID)

	// Publisher writes directly through the consumer's Handle method —
	// easiest integration without wiring Kafka in the test.
	h := NewCodexFromInteractionHandler(p)

	entryID := uuid.New()
	payload := CodexUpdateFromInteraction{
		Source:        "user_interaction",
		InteractionID: interactionID,
		ProductID:     productID,
		CodexEntryID:  entryID,
		Section:       "codex_anti_patterns",
		EntryType:     string(interactions.ClassifiedCodexAntipattern),
		EntryPayload: map[string]any{
			"name":      "no int IDs",
			"why_wrong": "we use GUIDs across the system",
		},
	}
	body, _ := json.Marshal(payload)
	if err := h.Handle(ctx, &bus.Message{Value: body}); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	// Assert the row landed.
	var gotName, gotSource, gotState string
	var gotSrcInteraction uuid.UUID
	err := p.QueryRow(ctx, `
		SELECT name, source, state, source_interaction_id
		FROM codex_anti_patterns WHERE id = $1
	`, entryID).Scan(&gotName, &gotSource, &gotState, &gotSrcInteraction)
	if err != nil {
		t.Fatalf("read back anti_pattern: %v", err)
	}
	if gotName != "no int IDs" {
		t.Errorf("name = %q", gotName)
	}
	if gotSource != "user_interaction" {
		t.Errorf("source = %q", gotSource)
	}
	if gotState != "active" {
		t.Errorf("state = %q", gotState)
	}
	if gotSrcInteraction != interactionID {
		t.Errorf("source_interaction_id = %v", gotSrcInteraction)
	}

	// Assert codex version bumped, typed (migration 060: anti_patterns, not
	// the table name), with its codex.indexed event.
	var versionCount int
	_ = p.QueryRow(ctx, `SELECT COUNT(*) FROM codex_versions WHERE product_id = $1`, productID).Scan(&versionCount)
	if versionCount < 1 {
		t.Errorf("expected at least 1 codex_versions row for product, got %d", versionCount)
	}
	var sections []string
	var productVersion int
	if err := p.QueryRow(ctx, `SELECT sections_changed, product_version FROM codex_versions WHERE product_id = $1 ORDER BY product_version DESC LIMIT 1`, productID).Scan(&sections, &productVersion); err != nil {
		t.Fatal(err)
	}
	if len(sections) != 1 || sections[0] != "anti_patterns" || productVersion != 1 {
		t.Errorf("version sections = %v v%d, want [anti_patterns] v1", sections, productVersion)
	}
	var indexed []byte
	if err := p.QueryRow(ctx, `SELECT payload FROM outbox_events WHERE topic = $1 AND aggregate_id = $2`, bus.TopicCodexIndexed, productID).Scan(&indexed); err != nil {
		t.Fatalf("codex.indexed not enqueued: %v", err)
	}
	var ev map[string]any
	_ = json.Unmarshal(indexed, &ev)
	if ev["trigger"] != "interaction_answer" || ev["product_version"] != float64(1) || ev["branch"] != "main" {
		t.Errorf("codex.indexed = %s", indexed)
	}
}

func TestCodexFromInteractionHandler_WritesPatternWithName(t *testing.T) {
	p := codexTestPool(t)
	defer p.Close()
	ctx := context.Background()

	productID := mustInsertFixtureProductForCodex(t, p)
	interactionID := mustInsertFixtureInteractionForCodex(t, p, productID)
	h := NewCodexFromInteractionHandler(p)

	entryID := uuid.New()
	payload := CodexUpdateFromInteraction{
		Source:        "user_interaction",
		InteractionID: interactionID,
		ProductID:     productID,
		CodexEntryID:  entryID,
		Section:       "codex_patterns",
		EntryType:     string(interactions.ClassifiedCodexPattern),
		EntryPayload:  map[string]any{"name": "server actions for forms"},
	}
	body, _ := json.Marshal(payload)
	if err := h.Handle(ctx, &bus.Message{Value: body}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	var got string
	if err := p.QueryRow(ctx, `SELECT name FROM codex_patterns WHERE id = $1`, entryID).Scan(&got); err != nil {
		t.Fatalf("read back pattern: %v", err)
	}
	if got != "server actions for forms" {
		t.Errorf("name = %q", got)
	}
}

func TestCodexFromInteractionHandler_SectionsTypeDerivedFromClassification(t *testing.T) {
	p := codexTestPool(t)
	defer p.Close()
	ctx := context.Background()

	productID := mustInsertFixtureProductForCodex(t, p)
	interactionID := mustInsertFixtureInteractionForCodex(t, p, productID)
	h := NewCodexFromInteractionHandler(p)

	entryID := uuid.New()
	// codex_domain_fact → section_type='domain' (per enum valid values)
	payload := CodexUpdateFromInteraction{
		Source:        "user_interaction",
		InteractionID: interactionID,
		ProductID:     productID,
		CodexEntryID:  entryID,
		Section:       "codex_sections",
		EntryType:     string(interactions.ClassifiedCodexDomainFact),
		EntryPayload:  map[string]any{"name": "VAT in EU only"},
	}
	body, _ := json.Marshal(payload)
	if err := h.Handle(ctx, &bus.Message{Value: body}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	var sectionType string
	if err := p.QueryRow(ctx, `SELECT section_type::text FROM codex_sections WHERE id = $1`, entryID).Scan(&sectionType); err != nil {
		t.Fatalf("read back section: %v", err)
	}
	if sectionType != "domain" {
		t.Errorf("section_type = %q, want domain", sectionType)
	}
}

// Sanity — vector import resolves (unused but keeps the import graph healthy).
var _ = vector.Document{}
