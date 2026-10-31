package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/agentregistry"
	"github.com/autonomousengine/autonomous-engine/internal/bus"
	"github.com/autonomousengine/autonomous-engine/internal/codex"
	"github.com/autonomousengine/autonomous-engine/internal/guards"
	"github.com/autonomousengine/autonomous-engine/internal/guards/groundingtest"
	"github.com/autonomousengine/autonomous-engine/internal/products"
	"github.com/autonomousengine/autonomous-engine/internal/workgraph"
	"github.com/google/uuid"
)

// Codex coherence Part 3: a product whose codex has patterns but no module
// is refused before any LLM work, with the escalation (work.blocked, kind
// codex_ungrounded) and a recorded re-run of the intake message.
func TestIntegration_CodexGate_PatternsOnlyEscalates(t *testing.T) {
	pool := integrationPool(t)
	ctx := context.Background()
	f := groundingtest.PatternsOnly(t, pool, "intake", "master")
	msg := &bus.Message{Topic: bus.TopicIntake, Key: f.Item.String(),
		Value: []byte(`{"work_id":"` + f.Item.String() + `","product_id":"` + f.Product.String() + `","description":"d"}`)}

	refused, err := codexGate(ctx, pool, f.Item, f.Item, f.Product, "", msg)
	if err != nil {
		t.Fatalf("codexGate: %v", err)
	}
	if !refused {
		t.Fatal("patterns without a module must be refused")
	}
	r := groundingtest.AssertEscalated(t, pool, f.Item, agentID, bus.TopicIntake, "intake")
	if !strings.Contains(string(r.RerunPayload), f.Item.String()) || r.IndexBranch != "master" {
		t.Errorf("refusal = %+v", r)
	}
}

// Handler level: an intake for a grounded product whose work item has no
// branch (work_items.branch is NULL until the PR) passes the gate AND the
// codex context step; with no LLM configured it then stops at the provider
// block, not at a codex refusal.
func TestIntegration_Handler_NullBranchOnGroundedProductProceeds(t *testing.T) {
	pool := integrationPool(t)
	ctx := context.Background()
	f := groundingtest.PatternsOnly(t, pool, "intake", "master")
	f.AddModule(t, pool)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM work_item_base_shas WHERE work_item_id = $1`, f.Item)
		_, _ = pool.Exec(context.Background(), `DELETE FROM agent_assignments WHERE work_item_id = $1`, f.Item)
	})
	pinner := &intakePinner{repos: products.NewStore(pool), resolver: &fakeResolver{shas: map[uuid.UUID]string{f.Repo: "0de2b26812bfc966ac61835e1b6aae2f3282cfcd"}}}
	h := buildHandler(ctx, pool, workgraph.New(pool), codex.New(pool), agentregistry.New(pool), nil, nopBus{}, pinner)
	b, _ := json.Marshal(map[string]any{"work_id": f.Item.String(), "product_id": f.Product.String(), "description": "add a phone field"})
	if err := h(ctx, &bus.Message{Topic: bus.TopicIntake, Key: f.Item.String(), Value: b}); err != nil {
		t.Fatalf("handler: %v", err)
	}
	if _, ok, _ := guards.LoadRefusal(ctx, pool, f.Item); ok {
		t.Fatal("a grounded product with a NULL branch was refused")
	}
	var reason string
	if err := pool.QueryRow(ctx, `SELECT reason FROM work_item_transitions WHERE work_item_id = $1 ORDER BY timestamp DESC LIMIT 1`, f.Item).Scan(&reason); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reason, "No LLM provider") {
		t.Errorf("last transition reason = %q, want the provider block after the codex context", reason)
	}
}

func TestIntegration_CodexGate_GroundedPasses(t *testing.T) {
	pool := integrationPool(t)
	f := groundingtest.PatternsOnly(t, pool, "intake", "master")
	f.AddModule(t, pool)
	msg := &bus.Message{Topic: bus.TopicIntake, Key: f.Item.String(), Value: []byte(`{"work_id":"` + f.Item.String() + `"}`)}
	refused, err := codexGate(context.Background(), pool, f.Item, f.Item, f.Product, "feature/x", msg)
	if err != nil || refused {
		t.Fatalf("a grounded product must pass: refused=%v err=%v", refused, err)
	}
}
