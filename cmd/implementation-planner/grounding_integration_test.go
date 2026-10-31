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
)

type groundingBus struct{ bus.MessageBus }

func (groundingBus) Publish(context.Context, string, string, []byte) error { return nil }

// Handler level: technical-brief.ready for a grounded product whose work
// item has no branch (work_items.branch is NULL until the PR) passes the
// gate and the codex context step; with no LLM configured planning then
// stops at the provider block, not at a codex refusal.
func TestIntegration_Handler_NullBranchOnGroundedProductProceeds(t *testing.T) {
	pool := integrationPool(t)
	ctx := context.Background()
	f := groundingtest.PatternsOnly(t, pool, "research", "master")
	f.AddModule(t, pool)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM agent_assignments WHERE work_item_id = $1`, f.Item)
	})
	b, _ := json.Marshal(map[string]any{"work_id": f.Item.String(), "product_id": f.Product.String(), "brief_content": "Add a phone field to the contact form."})
	msg := &bus.Message{Topic: bus.TopicTechnicalBriefReady, Key: f.Item.String(), Value: b}
	err := handleTechnicalBrief(ctx, pool, workgraph.New(pool), codex.New(pool), products.NewStore(pool), agentregistry.New(pool), nil, groundingBus{}, msg)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if _, ok, _ := guards.LoadRefusal(ctx, pool, f.Item); ok {
		t.Fatal("a grounded product with a NULL branch was refused")
	}
	var reason string
	if err := pool.QueryRow(ctx, `SELECT reason FROM work_item_transitions WHERE work_item_id = $1 AND to_state = 'blocked' ORDER BY timestamp DESC LIMIT 1`, f.Item).Scan(&reason); err != nil {
		t.Fatalf("no block: %v", err)
	}
	if strings.Contains(reason, "codex") {
		t.Errorf("block reason = %q, want the provider block after the codex context", reason)
	}
}

// Codex coherence Part 3: the planner does not plan on a codex with
// patterns but no module; the refusal escalates (work.blocked, kind
// codex_ungrounded) and keeps the technical-brief.ready message for the
// re-run.
func TestIntegration_CodexGate_PatternsOnlyEscalates(t *testing.T) {
	pool := integrationPool(t)
	ctx := context.Background()
	f := groundingtest.PatternsOnly(t, pool, "research", "master")
	pid := f.Product
	msg := &bus.Message{Topic: bus.TopicTechnicalBriefReady, Key: f.Item.String(),
		Value: []byte(`{"work_id":"` + f.Item.String() + `","product_id":"` + f.Product.String() + `","brief_content":"b"}`)}

	refused, err := codexGate(ctx, pool, f.Item, pid, "", msg)
	if err != nil {
		t.Fatalf("codexGate: %v", err)
	}
	if !refused {
		t.Fatal("patterns without a module must be refused")
	}
	groundingtest.AssertEscalated(t, pool, f.Item, agentID, bus.TopicTechnicalBriefReady, "research")

	g := groundingtest.PatternsOnly(t, pool, "research", "master")
	g.AddModule(t, pool)
	gid := g.Product
	if refused, err := codexGate(ctx, pool, g.Item, gid, "feature/x", msg); refused || err != nil {
		t.Fatalf("grounded: refused=%v err=%v", refused, err)
	}
}
