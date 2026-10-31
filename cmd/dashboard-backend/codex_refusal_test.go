package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/bus"
	"github.com/autonomousengine/autonomous-engine/internal/db"
	"github.com/autonomousengine/autonomous-engine/internal/guards"
	"github.com/autonomousengine/autonomous-engine/internal/guards/groundingtest"
	"github.com/autonomousengine/autonomous-engine/internal/models"
	"github.com/autonomousengine/autonomous-engine/internal/workgraph"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Codex coherence Part 3: the dashboard side of a refusal's escalation
// (TEST_POSTGRES_DSN, migrated; -p 1).

func refusalPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN not set — skipping DB-integration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := db.Migrate(ctx, pool, filepath.Join("..", "..", "migrations")); err != nil {
		t.Fatal(err)
	}
	return pool
}

func refusedItem(t *testing.T, pool *pgxpool.Pool) groundingtest.Fixture {
	t.Helper()
	f := groundingtest.PatternsOnly(t, pool, "research", "master")
	ctx := context.Background()
	check := guards.CheckCodexGrounded(ctx, pool, f.Product, "")
	if err := guards.RefuseUngrounded(ctx, pool, f.Item, f.Item, "research-coordinator", guards.Refusal{
		ProductID: f.Product, Stage: "research-coordinator", Reason: check.Message,
		RerunState: models.WorkItemStateResearch, RerunTopic: bus.TopicPRDReady, RerunKey: f.Item.String(),
		RerunPayload: []byte(`{"work_id":"` + f.Item.String() + `","prd_content":"p"}`),
	}); err != nil {
		t.Fatal(err)
	}
	return f
}

// fakeIndexer records /api/index/full calls and answers with status.
type fakeIndexer struct {
	mu     sync.Mutex
	calls  []*http.Request
	status int
}

func (f *fakeIndexer) start(t *testing.T) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.calls = append(f.calls, r)
		f.mu.Unlock()
		w.WriteHeader(f.status)
	}))
	old := knowledgeIndexerURL
	knowledgeIndexerURL = srv.URL
	t.Cleanup(func() { knowledgeIndexerURL = old; srv.Close() })
}

// Re-index triggers the product's full index (on the branch the guard fell
// back to) and records the pending re-run; an indexer failure withdraws the
// request and fails the answer.
func TestIntegration_BlockedAnswer_ReindexCallsIndexer(t *testing.T) {
	pool := refusalPool(t)
	ctx := context.Background()
	f := refusedItem(t, pool)
	idx := &fakeIndexer{status: http.StatusAccepted}
	idx.start(t)
	h := NewHandlers(workgraph.New(pool), nil, nil, nil, nil, nil)

	if err := h.HandleBlockedPromptAnswer(ctx, f.Item, guards.ActionReindex, "", "operator"); err != nil {
		t.Fatalf("reindex answer: %v", err)
	}
	if len(idx.calls) != 1 {
		t.Fatalf("indexer calls = %d, want 1", len(idx.calls))
	}
	call := idx.calls[0]
	if call.Method != http.MethodPost || call.URL.Path != "/api/index/full" ||
		call.URL.Query().Get("product_id") != f.Product.String() || call.URL.Query().Get("branch") != "master" {
		t.Errorf("indexer call = %s %s", call.Method, call.URL)
	}
	r, ok, _ := guards.LoadRefusal(ctx, pool, f.Item)
	if !ok || r.ReindexRequestedAt == nil || r.ReindexRequestedBy != "operator" {
		t.Fatalf("refusal = %+v, want the re-index pending", r)
	}
	if got := itemStateOf(t, pool, f.Item); got != "blocked" {
		t.Errorf("state = %s: Re-index must not re-run the item before the index lands", got)
	}
	if r.IndexPostedAt == nil {
		t.Error("an accepted index request must be recorded as posted")
	}
}

// 409: knowledge-indexer is busy (its single flight covers every product),
// so nothing is indexing ours yet. The request stays pending and unposted
// for status-orchestrator to re-post; the answer succeeds.
func TestIntegration_BlockedAnswer_ReindexBusyStaysPending(t *testing.T) {
	pool := refusalPool(t)
	ctx := context.Background()
	f := refusedItem(t, pool)
	idx := &fakeIndexer{status: http.StatusConflict}
	idx.start(t)
	h := NewHandlers(workgraph.New(pool), nil, nil, nil, nil, nil)
	if err := h.HandleBlockedPromptAnswer(ctx, f.Item, guards.ActionReindex, "", "operator"); err != nil {
		t.Fatalf("409: %v", err)
	}
	r, _, _ := guards.LoadRefusal(ctx, pool, f.Item)
	if r.ReindexRequestedAt == nil || r.IndexPostedAt != nil {
		t.Errorf("refusal = requested %v posted %v, want requested and not posted", r.ReindexRequestedAt, r.IndexPostedAt)
	}
}

// A failed index request (404, 500, unreachable) withdraws the request and
// raises the codex escalation again: the item is never left blocked without
// a pending prompt.
func TestIntegration_BlockedAnswer_ReindexFailureEscalatesAgain(t *testing.T) {
	pool := refusalPool(t)
	ctx := context.Background()
	h := NewHandlers(workgraph.New(pool), nil, nil, nil, nil, nil)
	for _, status := range []int{http.StatusNotFound, http.StatusInternalServerError, 0} {
		f := refusedItem(t, pool)
		if status == 0 {
			old := knowledgeIndexerURL
			knowledgeIndexerURL = "http://127.0.0.1:1"
			t.Cleanup(func() { knowledgeIndexerURL = old })
		} else {
			(&fakeIndexer{status: status}).start(t)
		}
		if err := h.HandleBlockedPromptAnswer(ctx, f.Item, guards.ActionReindex, "", "operator"); err == nil {
			t.Errorf("status %d: an indexer failure must fail the answer", status)
		}
		if r, _, _ := guards.LoadRefusal(ctx, pool, f.Item); r.ReindexRequestedAt != nil {
			t.Errorf("status %d: a failed index request must withdraw the pending re-run", status)
		}
		var actions string
		if err := pool.QueryRow(ctx, `SELECT actions::text FROM user_interactions WHERE work_item_id = $1 AND state = 'pending' AND prompt_type = 'escalation'`, f.Item).Scan(&actions); err != nil {
			t.Errorf("status %d: no pending escalation after the failed re-index: %v", status, err)
			continue
		}
		if !strings.Contains(actions, `"reindex"`) {
			t.Errorf("status %d: escalation actions = %s, want Re-index", status, actions)
		}
	}
}

// Retry on a refusal re-runs the refused stage at once from the recorded
// message; Cancel drops the refusal.
func TestIntegration_BlockedAnswer_RetryAndCancelOnRefusal(t *testing.T) {
	pool := refusalPool(t)
	ctx := context.Background()
	h := NewHandlers(workgraph.New(pool), nil, nil, nil, nil, nil)

	f := refusedItem(t, pool)
	if err := h.HandleBlockedPromptAnswer(ctx, f.Item, guards.ActionRetry, "", "operator"); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if got := itemStateOf(t, pool, f.Item); got != "research" {
		t.Errorf("state after retry = %s, want research", got)
	}
	var n int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM outbox_events WHERE topic = $1 AND key = $2`, bus.TopicPRDReady, f.Item.String()).Scan(&n)
	if n != 1 {
		t.Errorf("prd.ready enqueued %d times, want 1", n)
	}
	if _, ok, _ := guards.LoadRefusal(ctx, pool, f.Item); ok {
		t.Error("retry must consume the refusal")
	}

	g := refusedItem(t, pool)
	if err := h.HandleBlockedPromptAnswer(ctx, g.Item, guards.ActionCancel, "", "operator"); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if got := itemStateOf(t, pool, g.Item); got != "cancelled" {
		t.Errorf("state after cancel = %s", got)
	}
	if _, ok, _ := guards.LoadRefusal(ctx, pool, g.Item); ok {
		t.Error("cancel must drop the refusal")
	}
}

// reblockElsewhere moves a refused item on (retried outside the refusal)
// and blocks it again for an unrelated reason, leaving the refusal row
// behind as it was.
func reblockElsewhere(t *testing.T, pool *pgxpool.Pool, f groundingtest.Fixture) {
	t.Helper()
	ctx := context.Background()
	wg := workgraph.New(pool)
	id := f.Item
	if err := wg.TransitionState(ctx, id, models.WorkItemStateResearch, "someone", "retried elsewhere"); err != nil {
		t.Fatal(err)
	}
	if err := workgraph.WithTx(ctx, pool, func(tx pgx.Tx) error {
		return guards.BlockWorkItemTx(ctx, tx, id, id, "research-coordinator", "No LLM provider configured.")
	}); err != nil {
		t.Fatal(err)
	}
}

// A refusal row left behind by an earlier codex refusal must not hijack the
// Retry of a later, unrelated block: the generic Retry runs (here: it needs
// a PRD and says so), nothing is replayed, and the stale row is dropped.
func TestIntegration_BlockedAnswer_StaleRefusalDoesNotHijackRetry(t *testing.T) {
	pool := refusalPool(t)
	ctx := context.Background()
	h := NewHandlers(workgraph.New(pool), nil, nil, nil, nil, nil)
	f := refusedItem(t, pool)
	reblockElsewhere(t, pool, f)

	err := h.HandleBlockedPromptAnswer(ctx, f.Item, guards.ActionRetry, "", "operator")
	if err == nil || !strings.Contains(err.Error(), "no PRD") {
		t.Errorf("retry = %v, want the generic Retry path (no PRD)", err)
	}
	var n int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM outbox_events WHERE topic = $1 AND key = $2`, bus.TopicPRDReady, f.Item.String()).Scan(&n)
	if n != 0 {
		t.Errorf("the stale refusal replayed prd.ready %d times", n)
	}
	if _, ok, _ := guards.LoadRefusal(ctx, pool, f.Item); ok {
		t.Error("the stale refusal must be dropped")
	}
}

// Every other retry / cancel path drops the refusal (RetryWorkItem here).
func TestIntegration_RetryWorkItem_DropsRefusal(t *testing.T) {
	pool := refusalPool(t)
	ctx := context.Background()
	h := NewHandlers(workgraph.New(pool), nil, nil, nil, nil, nil)
	f := refusedItem(t, pool)
	rec := httptest.NewRecorder()
	h.RetryWorkItem(rec, httptest.NewRequest(http.MethodPost, "/api/work-items/retry?id="+f.Item.String(), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("retry: %d %s", rec.Code, rec.Body.String())
	}
	if _, ok, _ := guards.LoadRefusal(ctx, pool, f.Item); ok {
		t.Error("RetryWorkItem must drop the refusal")
	}
}

// Re-index on an item without a refusal fails rather than do nothing.
func TestIntegration_BlockedAnswer_ReindexWithoutRefusal(t *testing.T) {
	pool := refusalPool(t)
	f := groundingtest.PatternsOnly(t, pool, "blocked", "master")
	idx := &fakeIndexer{status: http.StatusAccepted}
	idx.start(t)
	h := NewHandlers(workgraph.New(pool), nil, nil, nil, nil, nil)
	if err := h.HandleBlockedPromptAnswer(context.Background(), f.Item, guards.ActionReindex, "", "operator"); err == nil {
		t.Fatal("Re-index without a codex refusal must fail")
	}
	if len(idx.calls) != 0 {
		t.Errorf("indexer called %d times", len(idx.calls))
	}
}

func itemStateOf(t *testing.T, pool *pgxpool.Pool, id interface{}) string {
	t.Helper()
	var s string
	if err := pool.QueryRow(context.Background(), `SELECT state::text FROM work_items WHERE id = $1`, id).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}
