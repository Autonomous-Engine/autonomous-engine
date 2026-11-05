package guards

import (
	"context"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/bus"
	"github.com/autonomousengine/autonomous-engine/internal/models"
	"github.com/autonomousengine/autonomous-engine/internal/workgraph"
	"github.com/google/uuid"
)

// A block raised through BlockWorkItem (the coordinators' non-transactional
// path) reached the dashboard only as state='blocked': no work.blocked
// event, so the orchestrator never parked the item behind a "Blocked:"
// prompt and the Approvals page stayed empty, while the same block raised
// through BlockWorkItemTx did (rerun NEW-7). Both paths must publish.
func TestIntegration_BlockWorkItemEnqueuesWorkBlocked(t *testing.T) {
	pool := integrationPool(t)
	ctx := context.Background()
	pid := groundingProduct(t, pool, "main")
	workID := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO work_items (id, type, state, title, product_id) VALUES ($1, 'feature', 'research', 'blocked via guard', $2)`, workID, pid); err != nil {
		t.Fatal(err)
	}
	wg := workgraph.New(pool)
	if err := BlockWorkItem(ctx, wg, nil, workID, "research-coordinator", "3 of 3 tasks failed"); err != nil {
		t.Fatalf("BlockWorkItem: %v", err)
	}
	item, err := wg.GetWorkItem(ctx, workID)
	if err != nil {
		t.Fatal(err)
	}
	if item.State != models.WorkItemStateBlocked {
		t.Fatalf("state = %q, want blocked", item.State)
	}
	var reason string
	err = pool.QueryRow(ctx, `SELECT payload->>'reason' FROM outbox_events WHERE aggregate_id = $1 AND topic = $2`, workID, bus.TopicWorkBlocked).Scan(&reason)
	if err != nil {
		t.Fatalf("work.blocked outbox event: %v", err)
	}
	if reason != "3 of 3 tasks failed" {
		t.Fatalf("work.blocked reason = %q", reason)
	}
	// Blocking an already-blocked item stays a no-op: one event, not two.
	if err := BlockWorkItem(ctx, wg, nil, workID, "research-coordinator", "again"); err != nil {
		t.Fatalf("second BlockWorkItem: %v", err)
	}
	var n int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM outbox_events WHERE aggregate_id = $1 AND topic = $2`, workID, bus.TopicWorkBlocked).Scan(&n)
	if n != 1 {
		t.Fatalf("work.blocked events = %d, want 1", n)
	}
}
