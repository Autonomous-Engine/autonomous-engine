package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/bus"
	"github.com/autonomousengine/autonomous-engine/internal/codex"
	"github.com/autonomousengine/autonomous-engine/internal/models"
	"github.com/autonomousengine/autonomous-engine/internal/zones"
	"github.com/google/uuid"
)

// Codex coherence Part 2 × Part 1 (design: "zone edit → codex version with
// zones → Part 1 re-plan of affected items"): a zone written through the
// zones store records a codex version with section:zones on the
// repository's product and base branch; section:zones is plan-relevant, so
// once the (short) CODEX_REPLAN_DEBOUNCE quiet window ends, a parent pinned
// to that product and branch and in implementation goes back to planning.
func TestIntegration_CodexReplan_ZoneEditReplansPinnedImplementation(t *testing.T) {
	pool := integrationPool(t)
	ctx := context.Background()
	t.Setenv("CODEX_REPLAN_DEBOUNCE", "300ms")
	cfg := replanConfigFromEnv()
	if cfg.Debounce != 300*time.Millisecond {
		t.Fatalf("debounce = %s, want the env value 300ms", cfg.Debounce)
	}
	cfg.Lease = time.Minute
	f := newReplanFixture(t, pool)
	r := newTestReplanner(pool, cfg)

	repo := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO repositories (id, product_id, name, github_url, base_branch, enabled) VALUES ($1, $2, 'zones-replan', $3, 'main', true)`,
		repo, f.product, "o/zones-replan-"+repo.String()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c := context.Background()
		_, _ = pool.Exec(c, `DELETE FROM code_zones WHERE repository_id = $1`, repo)
		_, _ = pool.Exec(c, `DELETE FROM codex_replan_pending WHERE product_id = $1`, f.product)
		_, _ = pool.Exec(c, `DELETE FROM repositories WHERE id = $1`, repo)
	})

	f.version("main", "module:a")
	parent, subs := f.parent(models.WorkItemStateImplementation, "main", 1, []string{"a"}, models.WorkItemStateImplementation)
	other, _ := f.parent(models.WorkItemStateImplementation, "release", 1, []string{"a"})

	// The zone edit (dashboard path): its codex version is the only
	// codex.indexed it emits, carrying section:zones.
	if _, err := zones.NewStore(pool).Create(ctx, repo, zones.Template{
		Name: "guarded", Description: "migrations need a human", Patterns: []string{"**/migrations/**"},
		Policy: zones.PolicyHumanReview, Source: zones.SourceDashboard,
	}); err != nil {
		t.Fatalf("create zone: %v", err)
	}
	var eventID uuid.UUID
	var payload []byte
	if err := pool.QueryRow(ctx, `SELECT id, payload FROM outbox_events WHERE topic = $1 AND aggregate_id = $2 AND payload->'sections_changed' ? $3 ORDER BY created_at DESC LIMIT 1`,
		bus.TopicCodexIndexed, f.product, zones.ZonesSection).Scan(&eventID, &payload); err != nil {
		t.Fatalf("the zone edit's codex.indexed (%s): %v", zones.ZonesSection, err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM processed_events WHERE event_id = $1`, eventID)
	})
	if ev, err := codex.DecodeIndexedEvent(payload); err != nil || ev.Branch != "main" || ev.ProductID != f.product {
		t.Fatalf("codex.indexed payload = %s (%v), want the product on the repository's base branch main", payload, err)
	}
	replay(t, codexHandler(pool, r), &bus.Message{Topic: bus.TopicCodexIndexed, Key: f.product.String(), Value: payload,
		Headers: map[string]string{bus.HeaderEventID: eventID.String()}}, 1)

	// Inside the quiet window nothing is re-planned.
	f.sweep(r)
	if s := itemState(t, pool, parent); s != "implementation" {
		t.Fatalf("parent = %s inside the debounce window, want implementation", s)
	}
	// After it, the sweep re-plans the pinned item.
	deadline := time.Now().Add(10 * time.Second)
	for itemState(t, pool, parent) != "planning" && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
		f.sweep(r)
	}
	if s := itemState(t, pool, parent); s != "planning" {
		t.Fatalf("parent = %s after the zone edit's debounce, want planning (re-planned)", s)
	}
	if reason := latestReason(t, pool, parent, "planning"); !strings.Contains(reason, "zones") {
		t.Errorf("re-plan reason = %q, want it to name the zones section", reason)
	}
	if s := itemState(t, pool, subs[0]); s != "blocked" {
		t.Errorf("in-progress subtask = %s, want blocked (superseded by the re-plan)", s)
	}
	if n := f.briefs(parent); n != 1 {
		t.Errorf("technical-brief.ready events = %d, want 1 (the planner plans again)", n)
	}
	if s := itemState(t, pool, other); s != "implementation" {
		t.Errorf("an item pinned to another branch = %s, want implementation (untouched)", s)
	}
}
