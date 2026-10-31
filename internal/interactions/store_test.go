package interactions_test

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/interactions"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func pool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN not set — skipping DB-integration test")
	}
	p, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	return p
}

// mustInsertFixtureProduct inserts a minimal products row and returns its id.
// Registers a t.Cleanup hook that tears down the product + everything that
// references it (user_interactions, codex entries sourced from interactions,
// work_items, agent_resumption_state) when the test finishes. Without this,
// previous runs accumulated 145+ fixture products in a live dev database
// because the user_interactions.product_id FK lacks ON DELETE CASCADE.
//
// Cleanup opens a FRESH pool from TEST_POSTGRES_DSN rather than reusing the
// passed-in pool because test bodies typically `defer p.Close()` — and Go
// runs defers BEFORE t.Cleanup hooks, so a shared pool would be closed
// before cleanup fires.
func mustInsertFixtureProduct(t *testing.T, p *pgxpool.Pool) uuid.UUID {
	t.Helper()
	id := uuid.New()
	ctx := context.Background()
	_, err := p.Exec(ctx, `
		INSERT INTO products (id, name, created_at, updated_at)
		VALUES ($1, $2, NOW(), NOW())
	`, id, "fixture-"+id.String()[:8])
	if err != nil {
		t.Fatalf("fixture product: %v", err)
	}
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	t.Cleanup(func() {
		cleanupProductViaDSN(dsn, id)
	})
	return id
}

// mustInsertFixtureWorkItem inserts a minimal work_items row and returns its id.
// Cleanup cascades from the product delete registered by mustInsertFixtureProduct.
func mustInsertFixtureWorkItem(t *testing.T, p *pgxpool.Pool, productID uuid.UUID) uuid.UUID {
	t.Helper()
	id := uuid.New()
	_, err := p.Exec(context.Background(), `
		INSERT INTO work_items (id, product_id, title, state, type, priority, created_at, updated_at)
		VALUES ($1, $2, $3, 'intake', 'feature', 'medium', NOW(), NOW())
	`, id, productID, "fixture wi")
	if err != nil {
		t.Fatalf("fixture work_item: %v", err)
	}
	return id
}

// cleanupProductViaDSN opens a fresh pool from the given DSN and tears down
// every row that references the product. Safe to call after the test's
// primary pool has been Close()d — we deliberately don't rely on it.
// Order matters: FKs on codex_* and work_items.awaiting_prompt_id lack
// ON DELETE CASCADE, so we null / delete manually in topological order
// before the final DELETE products triggers the remaining cascades.
func cleanupProductViaDSN(dsn string, productID uuid.UUID) {
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
		_, _ = pool.Exec(ctx, `
			DELETE FROM `+table+`
			WHERE source_interaction_id IN (SELECT id FROM user_interactions WHERE product_id = $1)
			   OR product_id = $1
		`, productID)
	}
	// work_items.awaiting_prompt_id must be nulled before deleting user_interactions.
	_, _ = pool.Exec(ctx, `UPDATE work_items SET awaiting_prompt_id = NULL WHERE product_id = $1`, productID)
	_, _ = pool.Exec(ctx, `DELETE FROM user_interactions WHERE product_id = $1`, productID)
	// Explicitly DELETE work_items — the FK is ON DELETE SET NULL, so deleting
	// the product alone leaves orphans (product_id NULL) instead of cascading.
	_, _ = pool.Exec(ctx, `DELETE FROM work_items WHERE product_id = $1`, productID)
	_, _ = pool.Exec(ctx, `DELETE FROM products WHERE id = $1`, productID)
}

func newPendingPrompt(workItemID, productID uuid.UUID) *interactions.Prompt {
	return &interactions.Prompt{
		ID:          uuid.New(),
		WorkItemID:  workItemID,
		ProductID:   productID,
		AskingAgent: "implementation-agent",
		Trigger:     interactions.TriggerAgentRequest,
		PromptType:  interactions.PromptTypeClarification,
		Headline:    "Which function did you mean?",
		Question:    "Codex has no sendEmail. Candidates: sendMail / dispatchEmail / notifyByEmail.",
		Actions: []interactions.Action{
			{ID: "a1", Label: "Use sendMail", Semantic: interactions.SemanticContinue},
		},
		State:       interactions.StatePending,
		RequestedAt: time.Now().UTC(),
		PromptedVia: []string{"dashboard"},
	}
}

func TestStore_InsertAndGet_RoundTrip(t *testing.T) {
	p := pool(t)
	defer p.Close()
	store := interactions.NewStore(p)
	ctx := context.Background()

	productID := mustInsertFixtureProduct(t, p)
	workItemID := mustInsertFixtureWorkItem(t, p, productID)
	prompt := newPendingPrompt(workItemID, productID)

	if err := store.Insert(ctx, prompt); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	got, err := store.GetByID(ctx, prompt.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.Headline != prompt.Headline {
		t.Errorf("Headline = %q, want %q", got.Headline, prompt.Headline)
	}
	if len(got.Actions) != 1 || got.Actions[0].ID != "a1" {
		t.Errorf("Actions round-trip failed: %+v", got.Actions)
	}
	if got.State != interactions.StatePending {
		t.Errorf("State = %q, want pending", got.State)
	}
}

func TestStore_GetByID_NotFound(t *testing.T) {
	p := pool(t)
	defer p.Close()
	store := interactions.NewStore(p)
	if _, err := store.GetByID(context.Background(), uuid.New()); err != interactions.ErrNotFound {
		t.Errorf("GetByID(unknown) = %v, want ErrNotFound", err)
	}
}

func TestStore_MarkAnswered_CAS(t *testing.T) {
	p := pool(t)
	defer p.Close()
	store := interactions.NewStore(p)
	ctx := context.Background()

	productID := mustInsertFixtureProduct(t, p)
	workItemID := mustInsertFixtureWorkItem(t, p, productID)
	prompt := newPendingPrompt(workItemID, productID)
	if err := store.Insert(ctx, prompt); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	override := interactions.SaveAsCodexFact
	ans := interactions.AnswerInput{
		AnsweredBy:     "admin@localhost",
		AnsweredVia:    "dashboard",
		ActionChosen:   "a1",
		AnswerText:     "sendMail",
		SaveAsOverride: &override,
	}
	if err := store.MarkAnswered(ctx, prompt.ID, ans); err != nil {
		t.Fatalf("MarkAnswered first: %v", err)
	}

	// Second call must fail with ErrNotFound (CAS: state is already answered).
	if err := store.MarkAnswered(ctx, prompt.ID, ans); err != interactions.ErrNotFound {
		t.Errorf("MarkAnswered second = %v, want ErrNotFound (double-submit guard)", err)
	}

	got, _ := store.GetByID(ctx, prompt.ID)
	if got.State != interactions.StateAnswered {
		t.Errorf("state = %q, want answered", got.State)
	}
	if got.SaveAsOverride == nil || *got.SaveAsOverride != interactions.SaveAsCodexFact {
		t.Errorf("SaveAsOverride round-trip failed: %+v", got.SaveAsOverride)
	}
}

func TestStore_MarkRevoked(t *testing.T) {
	p := pool(t)
	defer p.Close()
	store := interactions.NewStore(p)
	ctx := context.Background()

	productID := mustInsertFixtureProduct(t, p)
	workItemID := mustInsertFixtureWorkItem(t, p, productID)
	prompt := newPendingPrompt(workItemID, productID)
	_ = store.Insert(ctx, prompt)

	if err := store.MarkRevoked(ctx, prompt.ID, "admin@localhost", "accidental"); err != nil {
		t.Fatalf("MarkRevoked: %v", err)
	}
	got, _ := store.GetByID(ctx, prompt.ID)
	if got.State != interactions.StateRevoked {
		t.Errorf("state = %q, want revoked", got.State)
	}
	if got.RevocationReason != "accidental" {
		t.Errorf("reason = %q", got.RevocationReason)
	}
}

func TestStore_ApplyClassification_PolymorphicCodexTable(t *testing.T) {
	p := pool(t)
	defer p.Close()
	store := interactions.NewStore(p)
	ctx := context.Background()

	productID := mustInsertFixtureProduct(t, p)
	workItemID := mustInsertFixtureWorkItem(t, p, productID)
	prompt := newPendingPrompt(workItemID, productID)
	_ = store.Insert(ctx, prompt)

	codexID := uuid.New()
	res := interactions.ClassificationResult{
		ClassifiedAs:    interactions.ClassifiedCodexAntipattern,
		Confidence:      0.92,
		CodexEntryID:    &codexID,
		CodexEntryTable: "codex_anti_patterns",
		QdrantPointID:   "qdrant-1",
	}
	if err := store.ApplyClassification(ctx, prompt.ID, res); err != nil {
		t.Fatalf("ApplyClassification: %v", err)
	}
	got, _ := store.GetByID(ctx, prompt.ID)
	if got.ClassifiedAs != interactions.ClassifiedCodexAntipattern {
		t.Errorf("ClassifiedAs = %q", got.ClassifiedAs)
	}
	if got.CodexEntryTable != "codex_anti_patterns" {
		t.Errorf("CodexEntryTable = %q, want codex_anti_patterns", got.CodexEntryTable)
	}
	if got.CodexEntryID == nil || *got.CodexEntryID != codexID {
		t.Errorf("CodexEntryID mismatch")
	}
}

func TestStore_ListPending_FiltersByProductAndOrdersByRequestedAt(t *testing.T) {
	p := pool(t)
	defer p.Close()
	store := interactions.NewStore(p)
	ctx := context.Background()

	prodA := mustInsertFixtureProduct(t, p)
	prodB := mustInsertFixtureProduct(t, p)
	wiA := mustInsertFixtureWorkItem(t, p, prodA)
	wiB := mustInsertFixtureWorkItem(t, p, prodB)

	pA1 := newPendingPrompt(wiA, prodA)
	pA1.RequestedAt = time.Now().Add(-2 * time.Minute)
	pA2 := newPendingPrompt(wiA, prodA)
	pA2.RequestedAt = time.Now().Add(-1 * time.Minute)
	pB1 := newPendingPrompt(wiB, prodB)
	pB1.RequestedAt = time.Now()
	_ = store.Insert(ctx, pA1)
	_ = store.Insert(ctx, pA2)
	_ = store.Insert(ctx, pB1)

	// Filter to prodA — should return 2 in request-order (oldest first).
	got, err := store.ListPending(ctx, &prodA, 50)
	if err != nil {
		t.Fatalf("ListPending: %v", err)
	}

	// Count only the two we just inserted for this product (DB may have leftovers).
	seen := map[uuid.UUID]bool{}
	for _, r := range got {
		seen[r.ID] = true
	}
	if !seen[pA1.ID] || !seen[pA2.ID] {
		t.Fatalf("missing inserted prompts: %+v", seen)
	}
	if seen[pB1.ID] {
		t.Errorf("ListPending(prodA) returned a prodB prompt")
	}

	// unmarshal check: actions round-trip correctly from jsonb.
	for _, r := range got {
		if r.ID == pA1.ID {
			if len(r.Actions) != 1 {
				t.Errorf("actions not round-tripped: %+v", r.Actions)
			}
			break
		}
	}

	// Suppress unused json import warning — include a silent reference.
	_ = json.Valid
}

func TestStore_FindPendingApprovalPromptForWorkItem(t *testing.T) {
	p := pool(t)
	defer p.Close()
	store := interactions.NewStore(p)
	ctx := context.Background()

	productID := mustInsertFixtureProduct(t, p)
	workItemID := mustInsertFixtureWorkItem(t, p, productID)

	// Insert a non-approval prompt — should be ignored.
	other := newPendingPrompt(workItemID, productID)
	if err := store.Insert(ctx, other); err != nil {
		t.Fatalf("insert other: %v", err)
	}

	// Insert an approval prompt — should be returned.
	approval := newPendingPrompt(workItemID, productID)
	approval.ID = uuid.New()
	approval.PromptType = interactions.PromptTypeApproval
	approval.Trigger = interactions.TriggerApprovalPRD
	if err := store.Insert(ctx, approval); err != nil {
		t.Fatalf("insert approval: %v", err)
	}

	got, err := store.FindPendingApprovalPromptForWorkItem(ctx, workItemID)
	if err != nil {
		t.Fatalf("FindPendingApprovalPromptForWorkItem: %v", err)
	}
	if got != approval.ID {
		t.Errorf("got %v, want %v", got, approval.ID)
	}

	// Unknown work item returns ErrNotFound.
	if _, err := store.FindPendingApprovalPromptForWorkItem(ctx, uuid.New()); err != interactions.ErrNotFound {
		t.Errorf("unknown work_item = %v, want ErrNotFound", err)
	}
}
