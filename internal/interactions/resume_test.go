package interactions_test

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/interactions"
	"github.com/google/uuid"
)

// jsonEqual compares two JSON byte slices semantically. Postgres JSONB
// reformats JSON during storage (e.g., inserts spaces after colons), so
// byte-identical comparisons fail after a round-trip even when the data
// is equivalent.
func jsonEqual(a, b []byte) bool {
	var ax, bx any
	if err := json.Unmarshal(a, &ax); err != nil {
		return false
	}
	if err := json.Unmarshal(b, &bx); err != nil {
		return false
	}
	return reflect.DeepEqual(ax, bx)
}

// mustInsertFixturePrompt inserts a minimal pending prompt and returns its id.
// Relies on the fixture helpers defined in store_test.go (same package).
func mustInsertFixturePrompt(t *testing.T, store *interactions.Store, workItemID, productID uuid.UUID) uuid.UUID {
	t.Helper()
	p := newPendingPrompt(workItemID, productID)
	if err := store.Insert(context.Background(), p); err != nil {
		t.Fatalf("fixture prompt insert: %v", err)
	}
	return p.ID
}

func TestResumption_PersistLoadDelete(t *testing.T) {
	p := pool(t)
	defer p.Close()
	store := interactions.NewStore(p)
	ctx := context.Background()

	productID := mustInsertFixtureProduct(t, p)
	workItemID := mustInsertFixtureWorkItem(t, p, productID)
	promptID := mustInsertFixturePrompt(t, store, workItemID, productID)

	rs := &interactions.ResumptionState{
		PromptID:       promptID,
		WorkItemID:     workItemID,
		AskingAgent:    "implementation-agent",
		MessageHistory: []byte(`[{"role":"user","content":"hi"}]`),
		ToolCallStack:  []byte(`[]`),
	}
	if err := store.PersistResumption(ctx, rs); err != nil {
		t.Fatalf("PersistResumption: %v", err)
	}

	got, err := store.LoadResumption(ctx, promptID)
	if err != nil {
		t.Fatalf("LoadResumption: %v", err)
	}
	if !jsonEqual(got.MessageHistory, rs.MessageHistory) {
		t.Errorf("MessageHistory round-trip mismatch: got %s, want %s", got.MessageHistory, rs.MessageHistory)
	}
	if got.AskingAgent != "implementation-agent" {
		t.Errorf("AskingAgent = %q", got.AskingAgent)
	}
	if got.SavedAt.IsZero() {
		t.Errorf("SavedAt should be set by DB default, got zero")
	}

	// Idempotent persist (upsert) — re-persist with different content should not error.
	rs.MessageHistory = []byte(`[{"role":"user","content":"updated"}]`)
	if err := store.PersistResumption(ctx, rs); err != nil {
		t.Fatalf("PersistResumption upsert: %v", err)
	}
	got2, _ := store.LoadResumption(ctx, promptID)
	if !jsonEqual(got2.MessageHistory, rs.MessageHistory) {
		t.Errorf("upsert did not overwrite MessageHistory: got %s, want %s", got2.MessageHistory, rs.MessageHistory)
	}

	// Delete.
	if err := store.DeleteResumption(ctx, promptID); err != nil {
		t.Fatalf("DeleteResumption: %v", err)
	}
	if _, err := store.LoadResumption(ctx, promptID); err != interactions.ErrNotFound {
		t.Errorf("Load after Delete = %v, want ErrNotFound", err)
	}

	// Unknown ID round-trips as ErrNotFound.
	if _, err := store.LoadResumption(ctx, uuid.New()); err != interactions.ErrNotFound {
		t.Errorf("Load(unknown) = %v, want ErrNotFound", err)
	}
}
