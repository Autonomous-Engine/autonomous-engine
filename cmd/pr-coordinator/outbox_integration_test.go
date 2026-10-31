package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/bus"
	"github.com/autonomousengine/autonomous-engine/internal/db"
	"github.com/autonomousengine/autonomous-engine/internal/interactions"
	"github.com/autonomousengine/autonomous-engine/internal/outbox"
	"github.com/autonomousengine/autonomous-engine/internal/workgraph"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// These tests drive pr-coordinator's two commit paths against a real
// PostgreSQL: the manual gate's pending row, pr.gated and pr-gate prompt, and
// auto mode's branch + pr.ready. They prove the rows and their outbox events
// commit together, that a failure leaves none of them, and that the
// dispatcher then delivers the unchanged message with event_id.
//
//	docker run ... -e TEST_POSTGRES_DSN=... golang:1.22-alpine \
//	    sh -c "go test ./cmd/pr-coordinator/ -run Integration -v"
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

// seedWorkItem creates a product and a parent work item and removes both (and
// everything hanging off them, including outbox rows) afterwards.
func seedWorkItem(t *testing.T, pool *pgxpool.Pool) (productID, workID uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	productID, workID = uuid.New(), uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO products (id, name) VALUES ($1, $2)`, productID, "m104b-test-"+productID.String()[:8]); err != nil {
		t.Fatalf("seed product: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO work_items (id, type, state, title, product_id) VALUES ($1, 'feature', 'implementation', 'M1-04b parent', $2)`, workID, productID); err != nil {
		t.Fatalf("seed work item: %v", err)
	}
	t.Cleanup(func() {
		cctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		for _, sql := range []string{
			`DELETE FROM outbox_events WHERE aggregate_id = $1`,
			`DELETE FROM user_interactions WHERE work_item_id = $1`,
			`DELETE FROM pending_pr_changes WHERE work_item_id = $1`,
			`DELETE FROM work_item_transitions WHERE work_item_id = $1`,
			`DELETE FROM work_items WHERE id = $1`,
		} {
			if _, err := pool.Exec(cctx, sql, workID); err != nil {
				t.Logf("cleanup %q: %v", sql, err)
			}
		}
		if _, err := pool.Exec(cctx, `DELETE FROM products WHERE id = $1`, productID); err != nil {
			t.Logf("cleanup product: %v", err)
		}
	})
	return productID, workID
}

func integrationGatedChange(productID, workID uuid.UUID) gatedChange {
	branch := "agent/" + workID.String() + "/abcd1234"
	sha := "0123456789abcdef0123456789abcdef01234567"
	return gatedChange{
		WorkID: workID, RepoID: uuid.New().String(), Branch: branch, BaseBranch: "main",
		Files: []string{"src/a.go"}, PRTitle: "[repo] M1-04b", PRDesc: "body", HeadSHA: sha,
		Gated: prReadyMessage{
			WorkID: workID.String(), ProductID: productID.String(), Branch: branch,
			FilesChanged: []string{"src/a.go"}, Timestamp: "2026-10-03T00:00:00Z", HeadSHA: sha,
		},
		Prompt: &interactions.Prompt{
			ID: uuid.New(), WorkItemID: workID, ProductID: productID,
			AskingAgent: "pr-coordinator", Trigger: interactions.TriggerApprovalPRGate,
			PromptType: interactions.PromptTypeApproval, Headline: "PR ready for review",
			Question: "Branch is ready", State: interactions.StatePending,
			Actions:     []interactions.Action{{ID: "approve", Label: "Create PR", Semantic: interactions.SemanticContinue}},
			PromptedVia: []string{"dashboard"},
		},
	}
}

func count(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return n
}

type recordingBus struct {
	mu   sync.Mutex
	sent []bus.Message
}

func (b *recordingBus) PublishMessage(_ context.Context, msg *bus.Message) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.sent = append(b.sent, *msg)
	return nil
}

func TestIntegration_GatedPR_PendingRowEventAndPromptCommitTogether(t *testing.T) {
	pool := integrationPool(t)
	productID, workID := seedWorkItem(t, pool)
	c := integrationGatedChange(productID, workID)

	if err := commitGatedPR(context.Background(), pool, interactions.NewPublisher(nil), c); err != nil {
		t.Fatalf("commitGatedPR: %v", err)
	}

	var headSHA, status string
	if err := pool.QueryRow(context.Background(),
		`SELECT COALESCE(head_sha, ''), status FROM pending_pr_changes WHERE work_item_id = $1`, workID).Scan(&headSHA, &status); err != nil {
		t.Fatalf("pending row: %v", err)
	}
	if headSHA != c.HeadSHA || status != "pending" {
		t.Errorf("pending row head_sha=%q status=%q", headSHA, status)
	}
	if n := count(t, pool, `SELECT count(*) FROM user_interactions WHERE id = $1`, c.Prompt.ID); n != 1 {
		t.Errorf("prompt rows = %d, want 1", n)
	}
	if n := count(t, pool, `SELECT count(*) FROM outbox_events WHERE aggregate_type = 'work_item' AND aggregate_id = $1 AND topic = $2 AND key = $3 AND published_at IS NULL`,
		workID, bus.TopicPRGated, workID.String()); n != 1 {
		t.Errorf("pr.gated outbox rows = %d, want 1", n)
	}
	if n := count(t, pool, `SELECT count(*) FROM outbox_events WHERE aggregate_id = $1 AND topic = $2`, workID, bus.TopicUserPromptRequested); n != 1 {
		t.Errorf("user_prompt.requested outbox rows = %d, want 1", n)
	}

	// The dispatcher delivers both, pr.gated first (enqueue order within
	// the aggregate), with the unchanged key and payload plus event_id.
	fb := &recordingBus{}
	d := outbox.NewDispatcher(outbox.NewStore(pool), fb, outbox.Options{ClaimedBy: "pr-coordinator/m104b-test"})
	// One claim takes the head of each aggregate only, so the two events of
	// this work item need two passes.
	for i := 0; i < 2; i++ {
		if _, err := d.RunOnce(context.Background()); err != nil {
			t.Fatalf("RunOnce: %v", err)
		}
	}
	if len(fb.sent) != 2 || fb.sent[0].Topic != bus.TopicPRGated || fb.sent[1].Topic != bus.TopicUserPromptRequested {
		t.Errorf("delivered %d message(s); want pr.gated then user_prompt.requested", len(fb.sent))
	}
	var gated *bus.Message
	for i := range fb.sent {
		if fb.sent[i].Topic == bus.TopicPRGated {
			gated = &fb.sent[i]
		}
	}
	if gated == nil {
		t.Fatalf("dispatcher sent %d message(s), none on pr.gated", len(fb.sent))
	}
	if gated.Key != workID.String() {
		t.Errorf("pr.gated key = %q", gated.Key)
	}
	var body map[string]any
	if err := json.Unmarshal(gated.Value, &body); err != nil {
		t.Fatal(err)
	}
	if body["head_sha"] != c.HeadSHA || body["branch"] != c.Branch || body[outbox.FieldEventID] == nil {
		t.Errorf("pr.gated payload = %v", body)
	}
	if n := count(t, pool, `SELECT count(*) FROM outbox_events WHERE aggregate_id = $1 AND published_at IS NOT NULL`, workID); n != 2 {
		t.Errorf("published rows = %d, want 2", n)
	}
}

// A failure anywhere in the gate's transaction leaves no pending row, no
// prompt and no event. Here the prompt row fails its product foreign key.
func TestIntegration_GatedPR_FailureLeavesNothing(t *testing.T) {
	pool := integrationPool(t)
	productID, workID := seedWorkItem(t, pool)
	c := integrationGatedChange(productID, workID)
	c.Prompt.ProductID = uuid.New() // no such product

	if err := commitGatedPR(context.Background(), pool, interactions.NewPublisher(nil), c); err == nil {
		t.Fatal("commitGatedPR succeeded with an invalid prompt product")
	}
	if n := count(t, pool, `SELECT count(*) FROM pending_pr_changes WHERE work_item_id = $1`, workID); n != 0 {
		t.Errorf("pending rows = %d after rollback", n)
	}
	if n := count(t, pool, `SELECT count(*) FROM outbox_events WHERE aggregate_id = $1`, workID); n != 0 {
		t.Errorf("outbox rows = %d after rollback", n)
	}
	if n := count(t, pool, `SELECT count(*) FROM user_interactions WHERE work_item_id = $1`, workID); n != 0 {
		t.Errorf("prompt rows = %d after rollback", n)
	}
}

func TestIntegration_PRReady_BranchAndEventCommitTogether(t *testing.T) {
	pool := integrationPool(t)
	productID, workID := seedWorkItem(t, pool)
	wg := workgraph.New(pool)
	branch := "agent/" + workID.String() + "/abcd1234"
	msg := prReadyMessage{WorkID: workID.String(), ProductID: productID.String(), PRNumber: 7, PRURL: "https://github.invalid/pr/7", Branch: branch}

	if err := commitPRReady(context.Background(), pool, wg, workID, branch, msg); err != nil {
		t.Fatalf("commitPRReady: %v", err)
	}
	var got string
	if err := pool.QueryRow(context.Background(), `SELECT COALESCE(branch, '') FROM work_items WHERE id = $1`, workID).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != branch {
		t.Errorf("work item branch = %q, want %q", got, branch)
	}
	if n := count(t, pool, `SELECT count(*) FROM outbox_events WHERE aggregate_id = $1 AND topic = $2 AND key = $3 AND payload->>'pr_url' = $4`,
		workID, bus.TopicPRReady, workID.String(), msg.PRURL); n != 1 {
		t.Errorf("pr.ready outbox rows = %d, want 1", n)
	}
}
