package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

// Codex coherence Part 3: research is not dispatched on a codex with
// patterns but no module; the refusal escalates (work.blocked, kind
// codex_ungrounded) and keeps the prd.ready message for the re-run.
func TestIntegration_CodexGate_PatternsOnlyEscalates(t *testing.T) {
	pool := integrationPool(t)
	ctx := context.Background()
	f := groundingtest.PatternsOnly(t, pool, "research", "master")
	msg := &bus.Message{Topic: bus.TopicPRDReady, Key: f.Item.String(),
		Value: []byte(`{"work_id":"` + f.Item.String() + `","product_id":"` + f.Product.String() + `","prd_content":"p"}`)}
	pid := f.Product

	// The other context checks pass, so only the codex refuses.
	ok := guards.ValidationResult{Valid: true}
	refused, err := contextGate(ctx, pool, f.Item, f.Item, &pid, "", msg, ok)
	if err != nil {
		t.Fatalf("contextGate: %v", err)
	}
	if !refused {
		t.Fatal("patterns without a module must be refused")
	}
	groundingtest.AssertEscalated(t, pool, f.Item, agentID, bus.TopicPRDReady, "research")
}

type groundingBus struct{ bus.MessageBus }

func (groundingBus) Publish(context.Context, string, string, []byte) error { return nil }

// Handler level: prd.ready for a grounded product whose work item has no
// branch (work_items.branch is NULL until the PR) passes the gate and the
// codex context step and dispatches research (research.tasks enqueued).
func TestIntegration_Handler_NullBranchOnGroundedProductProceeds(t *testing.T) {
	pool := integrationPool(t)
	ctx := context.Background()
	f := groundingtest.PatternsOnly(t, pool, "research", "master")
	f.AddModule(t, pool)
	clone := t.TempDir()
	if err := os.Mkdir(filepath.Join(clone, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE repositories SET clone_path = $2 WHERE id = $1`, f.Repo, clone); err != nil {
		t.Fatal(err)
	}
	qdrant := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"result":{"points_count":3}}`))
	}))
	t.Cleanup(qdrant.Close)
	t.Cleanup(func() {
		ctx := context.Background()
		for _, q := range []string{
			`DELETE FROM agent_assignments WHERE work_item_id = $1`,
			`DELETE FROM research_round_tasks WHERE round_id IN (SELECT id FROM research_rounds WHERE work_item_id = $1)`,
			`DELETE FROM research_rounds WHERE work_item_id = $1`,
		} {
			_, _ = pool.Exec(ctx, q, f.Item)
		}
	})

	h := buildHandler(ctx, pool, qdrant.URL, workgraph.New(pool), codex.New(pool), products.NewStore(pool), agentregistry.New(pool), nil, groundingBus{}, newCoordinator())
	b, _ := json.Marshal(map[string]any{"work_id": f.Item.String(), "product_id": f.Product.String(), "prd_content": "## Overview\nAdd a phone field.\n## Acceptance Criteria\n- the form has a phone field", "work_type": "feature"})
	if err := h(ctx, &bus.Message{Topic: bus.TopicPRDReady, Key: f.Item.String(), Value: b}); err != nil {
		t.Fatalf("handler: %v", err)
	}
	if _, ok, _ := guards.LoadRefusal(ctx, pool, f.Item); ok {
		t.Fatal("a grounded product with a NULL branch was refused")
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM outbox_events WHERE aggregate_id = $1 AND topic = $2`, f.Item, bus.TopicResearchTasks).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		t.Error("no research.tasks enqueued: the handler did not get past the codex context")
	}
	var codexSections string
	_ = pool.QueryRow(ctx, `SELECT COALESCE(payload->>'codex_sections', '') FROM outbox_events WHERE aggregate_id = $1 AND topic = $2 LIMIT 1`, f.Item, bus.TopicResearchTasks).Scan(&codexSections)
	if !strings.Contains(codexSections, "p3-module-") {
		t.Errorf("research task codex context = %q, want the product's module", codexSections)
	}
}

// A failure of the other context checks (repository, vector store) on a
// grounded codex still escalates, through work.blocked without the codex
// kind: no silent block.
func TestIntegration_CodexGate_OtherChecksEscalateGenerically(t *testing.T) {
	pool := integrationPool(t)
	ctx := context.Background()
	f := groundingtest.PatternsOnly(t, pool, "research", "master")
	f.AddModule(t, pool)
	pid := f.Product
	msg := &bus.Message{Topic: bus.TopicPRDReady, Key: f.Item.String(), Value: []byte(`{"work_id":"` + f.Item.String() + `"}`)}
	repoDown := guards.ValidationResult{Valid: false, Missing: []string{"repository"}, Message: "repository not on disk"}
	refused, err := contextGate(ctx, pool, f.Item, f.Item, &pid, "", msg, repoDown)
	if err != nil || !refused {
		t.Fatalf("refused=%v err=%v", refused, err)
	}
	var kind *string
	if err := pool.QueryRow(ctx, `SELECT payload->>'kind' FROM outbox_events WHERE topic = $1 AND key = $2`, bus.TopicWorkBlocked, f.Item.String()).Scan(&kind); err != nil {
		t.Fatalf("work.blocked not enqueued: %v", err)
	}
	if kind != nil {
		t.Errorf("kind = %q, want none (the codex is grounded)", *kind)
	}

	// Grounded and every other check passing: no refusal.
	g := groundingtest.PatternsOnly(t, pool, "research", "master")
	g.AddModule(t, pool)
	gid := g.Product
	if refused, err := contextGate(ctx, pool, g.Item, g.Item, &gid, "feature/x", msg, guards.ValidationResult{Valid: true}); refused || err != nil {
		t.Fatalf("grounded: refused=%v err=%v", refused, err)
	}
}
