package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/agentregistry"
	"github.com/autonomousengine/autonomous-engine/internal/bus"
	"github.com/autonomousengine/autonomous-engine/internal/idempotency"
	"github.com/autonomousengine/autonomous-engine/internal/interactions"
	"github.com/autonomousengine/autonomous-engine/internal/models"
	"github.com/autonomousengine/autonomous-engine/internal/products"
	"github.com/autonomousengine/autonomous-engine/internal/snapshot"
	"github.com/autonomousengine/autonomous-engine/internal/workgraph"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// M2-02 against a real PostgreSQL (TEST_POSTGRES_DSN, migrated through 050).

type pinFixture struct {
	product, repo, item uuid.UUID
}

func newPinFixture(t *testing.T, pool *pgxpool.Pool, state string) pinFixture {
	t.Helper()
	ctx := context.Background()
	f := pinFixture{product: uuid.New(), repo: uuid.New(), item: uuid.New()}
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO products (id, name) VALUES ($1, $2)`, []any{f.product, "m202-" + f.product.String()}},
		{`INSERT INTO repositories (id, product_id, name, github_url, base_branch, enabled) VALUES ($1, $2, 'm202-repo', $3, 'main', true)`, []any{f.repo, f.product, "o/m202-" + f.repo.String()}},
		{`INSERT INTO work_items (id, type, state, title, description, product_id) VALUES ($1, 'feature', $3, 'M2-02 pin item', 'd', $2)`, []any{f.item, f.product, state}},
	} {
		if _, err := pool.Exec(ctx, q.sql, q.args...); err != nil {
			t.Fatalf("%s: %v", q.sql, err)
		}
	}
	t.Cleanup(func() {
		ctx := context.Background()
		for _, q := range []string{
			`DELETE FROM outbox_events WHERE aggregate_id = $1`,
			`DELETE FROM user_interactions WHERE work_item_id = $1`,
			`DELETE FROM work_items WHERE id = $1`,
		} {
			_, _ = pool.Exec(ctx, q, f.item)
		}
		_, _ = pool.Exec(ctx, `DELETE FROM products WHERE id = $1`, f.product)
	})
	return f
}

func countRows(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return n
}

// A repository whose base cannot be resolved blocks the item before any LLM
// work, with the escalation prompt and work.blocked in the same transaction,
// and writes no pin.
func TestIntegration_Pins_ResolutionFailureBlocksWithPrompt(t *testing.T) {
	pool := integrationPool(t)
	ctx := context.Background()
	f := newPinFixture(t, pool, "intake")
	eventID := uuid.New()
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM processed_events WHERE event_id = $1`, eventID)
	})

	res := &fakeResolver{errs: map[uuid.UUID]error{f.repo: errors.New("could not resolve main of repository m202-repo: fetch: denied; ls-remote: 401")}}
	pinner := &intakePinner{repos: products.NewStore(pool), resolver: res}
	h := idempotency.NewLedger(pool).Handler("product-agent",
		buildHandler(ctx, pool, workgraph.New(pool), nil, agentregistry.New(pool), nil, nopBus{}, pinner))
	b, _ := json.Marshal(map[string]any{"work_id": f.item.String(), "product_id": f.product.String(), "source": "m202"})
	msg := &bus.Message{Topic: bus.TopicIntake, Key: f.item.String(), Value: b, Headers: map[string]string{bus.HeaderEventID: eventID.String()}}
	if err := h(ctx, msg); err != nil {
		t.Fatal(err)
	}

	var state string
	_ = pool.QueryRow(ctx, `SELECT state::text FROM work_items WHERE id = $1`, f.item).Scan(&state)
	if state != string(models.WorkItemStateBlocked) {
		t.Fatalf("state = %s, want blocked", state)
	}
	var reason string
	_ = pool.QueryRow(ctx, `SELECT reason FROM work_item_transitions WHERE work_item_id = $1 AND to_state = 'blocked'`, f.item).Scan(&reason)
	if !strings.Contains(reason, "m202-repo") || !strings.Contains(reason, "ls-remote: 401") {
		t.Errorf("block reason %q does not name the repository and the error", reason)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM user_interactions WHERE work_item_id = $1 AND trigger = $2 AND state = 'pending'`, f.item, string(interactions.TriggerBlocked)); n != 1 {
		t.Errorf("pending blocked prompts = %d, want 1", n)
	}
	for _, topic := range []string{bus.TopicWorkBlocked, bus.TopicUserPromptRequested} {
		if n := countRows(t, pool, `SELECT count(*) FROM outbox_events WHERE aggregate_id = $1 AND topic = $2`, f.item, topic); n != 1 {
			t.Errorf("%s outbox rows = %d, want 1", topic, n)
		}
	}
	if n := countRows(t, pool, `SELECT count(*) FROM work_item_base_shas WHERE work_item_id = $1`, f.item); n != 0 {
		t.Errorf("%d pins written for a failed resolution", n)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM processed_events WHERE event_id = $1`, eventID); n != 1 {
		t.Errorf("guard rows = %d, want 1 (the block transaction carries it)", n)
	}
}

// The resolved pins and their resolved events commit with the PRD; a
// failing PRD transaction leaves none of them.
func TestIntegration_Pins_CommitWithThePRD(t *testing.T) {
	pool := integrationPool(t)
	ctx := context.Background()
	f := newPinFixture(t, pool, "prd")
	wg := workgraph.New(pool)
	pins := []resolvedPin{{
		pin:    snapshot.Pin{WorkItemID: f.item, RepositoryID: f.repo, BaseRef: "main", BaseSHA: pinSHA1, ResolvedAt: time.Now().UTC()},
		repo:   "m202-repo",
		detail: "intake: fake fetch",
	}}
	newPrompt := func() *interactions.Prompt {
		return &interactions.Prompt{ID: uuid.New(), WorkItemID: f.item, ProductID: f.product, AskingAgent: agentID,
			Trigger: interactions.TriggerApprovalPRD, PromptType: interactions.PromptTypeApproval,
			Headline: "PRD ready", Question: "q", State: interactions.StatePending}
	}
	artifact := func() *models.WorkItemArtifact {
		return &models.WorkItemArtifact{WorkItemID: f.item, Type: models.ArtifactTypePRD, Content: []byte(`{"prd":"x"}`), AgentID: agentID}
	}

	// A prompt that cannot be stored (unknown product) rolls everything back,
	// pins included.
	bad := newPrompt()
	bad.ProductID = uuid.New()
	if err := commitPRD(ctx, pool, wg, interactions.NewPublisher(nil), f.item, f.item, "T", artifact(), bad, uuid.New(), pins); err == nil {
		t.Fatal("commitPRD succeeded with an unstorable prompt")
	}
	if n := countRows(t, pool, `SELECT count(*) FROM work_item_base_shas WHERE work_item_id = $1`, f.item); n != 0 {
		t.Fatalf("%d pins survived a rolled-back PRD transaction", n)
	}

	if err := commitPRD(ctx, pool, wg, interactions.NewPublisher(nil), f.item, f.item, "T", artifact(), newPrompt(), uuid.New(), pins); err != nil {
		t.Fatal(err)
	}
	store := snapshot.NewStore(pool)
	pin, found, err := store.Get(ctx, f.item, f.repo)
	if err != nil || !found || pin.BaseSHA != pinSHA1 || pin.BaseRef != "main" {
		t.Fatalf("pin = %+v found=%v err=%v", pin, found, err)
	}
	evs, _ := store.Events(ctx, f.item)
	if len(evs) != 1 || evs[0].Kind != snapshot.EventResolved || evs[0].NewSHA != pinSHA1 || evs[0].Detail != "intake: fake fetch" {
		t.Fatalf("events = %+v", evs)
	}
	var state string
	_ = pool.QueryRow(ctx, `SELECT state::text FROM work_items WHERE id = $1`, f.item).Scan(&state)
	if state != string(models.WorkItemStatePRDApproval) {
		t.Fatalf("state = %s, want prd_approval", state)
	}
}
