package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/bus"
	"github.com/autonomousengine/autonomous-engine/internal/codex"
	"github.com/autonomousengine/autonomous-engine/internal/codexgap"
	"github.com/autonomousengine/autonomous-engine/internal/guards"
	"github.com/autonomousengine/autonomous-engine/internal/guards/groundingtest"
	"github.com/autonomousengine/autonomous-engine/internal/idempotency"
	"github.com/autonomousengine/autonomous-engine/internal/interactions"
	"github.com/autonomousengine/autonomous-engine/internal/models"
	"github.com/autonomousengine/autonomous-engine/internal/workgraph"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// refused seeds a patterns-only product with an item in research refused by
// research-coordinator (the stage's own refusal transaction).
func refused(t *testing.T, pool *pgxpool.Pool) groundingtest.Fixture {
	t.Helper()
	f := groundingtest.PatternsOnly(t, pool, "research", "master")
	ctx := context.Background()
	check := guards.CheckCodexGrounded(ctx, pool, f.Product, "")
	if check.Valid {
		t.Fatal("fixture must be ungrounded")
	}
	if err := guards.RefuseUngrounded(ctx, pool, f.Item, f.Item, "research-coordinator", guards.Refusal{
		ProductID: f.Product, Stage: "research-coordinator", Reason: check.Message,
		RerunState: models.WorkItemStateResearch, RerunTopic: bus.TopicPRDReady, RerunKey: f.Item.String(),
		RerunPayload: []byte(`{"work_id":"` + f.Item.String() + `","prd_content":"p"}`),
	}); err != nil {
		t.Fatalf("refuse: %v", err)
	}
	return f
}

func indexedMsg(t *testing.T, product uuid.UUID, branch string, version int) *bus.Message {
	t.Helper()
	raw, _ := json.Marshal(codex.IndexedEvent{ProductID: product, Branch: branch, ProductVersion: version, VersionNumber: version,
		SectionsChanged: []string{"module:x"}, Trigger: codex.TriggerFullIndex})
	return &bus.Message{Topic: bus.TopicCodexIndexed, Key: product.String(), Value: raw,
		Headers: map[string]string{bus.HeaderEventID: uuid.NewString()}}
}

// A refusal's work.blocked raises the escalation with Re-index, Retry and
// Cancel: never a silent block.
func TestIntegration_CodexRefusal_EscalatesWithReindex(t *testing.T) {
	pool := integrationPool(t)
	ctx := context.Background()
	f := refused(t, pool)
	wg := workgraph.New(pool)
	rb := &recordingBus{j: &journal{}}

	var raw []byte
	if err := pool.QueryRow(ctx, `SELECT payload FROM outbox_events WHERE topic = $1 AND key = $2`, bus.TopicWorkBlocked, f.Item.String()).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	msg := &bus.Message{Topic: bus.TopicWorkBlocked, Key: f.Item.String(), Value: raw}
	if err := handleWorkBlocked(ctx, wg, rb, msg); err != nil {
		t.Fatal(err)
	}
	var actionsRaw []byte
	var trigger, ptype string
	if err := pool.QueryRow(ctx, `SELECT actions, trigger, prompt_type FROM user_interactions WHERE work_item_id = $1 AND state = 'pending'`, f.Item).
		Scan(&actionsRaw, &trigger, &ptype); err != nil {
		t.Fatalf("no pending escalation: %v", err)
	}
	var actions []interactions.Action
	_ = json.Unmarshal(actionsRaw, &actions)
	ids := map[string]bool{}
	for _, a := range actions {
		ids[a.ID] = true
	}
	if trigger != string(interactions.TriggerBlocked) || ptype != string(interactions.PromptTypeEscalation) ||
		!ids[guards.ActionReindex] || !ids[guards.ActionRetry] || !ids[guards.ActionCancel] || len(actions) != 3 {
		t.Errorf("escalation = %s/%s actions %v, want blocked/escalation with reindex, retry, cancel", trigger, ptype, actions)
	}
}

// Re-index: codex.indexed for another product, or for the product while the
// codex still has no module, does not re-run the item; once a module is
// indexed the item re-runs (back to research, prd.ready enqueued again).
// The pending re-run lives in Postgres: a fresh pool and handler (a
// restarted service) pick it up.
func TestIntegration_CodexIndexed_RerunsAfterReindex(t *testing.T) {
	pool := integrationPool(t)
	ctx := context.Background()
	f := refused(t, pool)
	if err := guards.MarkReindexRequested(ctx, pool, f.Item, "operator"); err != nil {
		t.Fatal(err)
	}
	wg := workgraph.New(pool)
	rb := &recordingBus{j: &journal{}}
	handler := idempotency.NewLedger(pool).Handler(codexIdempotencyConsumer, buildCodexHandler(wg, rb, codexgap.DefaultThreshold))
	prdReady := func() int {
		return count(t, pool, `SELECT count(*) FROM outbox_events WHERE topic = $1 AND key = $2`, bus.TopicPRDReady, f.Item.String())
	}

	// Another product: nothing happens.
	if err := handler(ctx, indexedMsg(t, uuid.New(), "master", 3)); err != nil {
		t.Fatal(err)
	}
	if got := itemState(t, pool, f.Item); got != "blocked" || prdReady() != 0 {
		t.Fatalf("another product's event: state %s, prd.ready %d", got, prdReady())
	}
	if r, ok, _ := guards.LoadRefusal(ctx, pool, f.Item); !ok || r.ReindexRequestedAt == nil {
		t.Fatal("another product's event withdrew the pending re-index")
	}

	// Another branch of this product, still no module: keep waiting.
	if err := handler(ctx, indexedMsg(t, f.Product, "develop", 2)); err != nil {
		t.Fatal(err)
	}
	if r, _, _ := guards.LoadRefusal(ctx, pool, f.Item); r.ReindexRequestedAt == nil || itemState(t, pool, f.Item) != "blocked" {
		t.Fatal("an index of another branch must not settle the refusal")
	}

	// "Restart": a new pool, work graph and handler (no state shared with
	// the ones above) find the pending row.
	fresh := integrationPool(t)
	wg = workgraph.New(fresh)
	handler = idempotency.NewLedger(fresh).Handler(codexIdempotencyConsumer, buildCodexHandler(wg, rb, codexgap.DefaultThreshold))

	f.AddModule(t, fresh)
	ev := indexedMsg(t, f.Product, "master", 4)
	if err := handler(ctx, ev); err != nil {
		t.Fatal(err)
	}
	if got := itemState(t, fresh, f.Item); got != "research" {
		t.Fatalf("state after the grounded index = %s, want research", got)
	}
	n := count(t, fresh, `SELECT count(*) FROM outbox_events WHERE topic = $1 AND key = $2`, bus.TopicPRDReady, f.Item.String())
	if n != 1 {
		t.Fatalf("prd.ready re-enqueued %d times, want 1", n)
	}
	if _, ok, _ := guards.LoadRefusal(ctx, fresh, f.Item); ok {
		t.Error("the refusal must be gone after the re-run")
	}
	// A redelivery of the same event changes nothing.
	if err := handler(ctx, ev); err != nil {
		t.Fatal(err)
	}
	if n := count(t, fresh, `SELECT count(*) FROM outbox_events WHERE topic = $1 AND key = $2`, bus.TopicPRDReady, f.Item.String()); n != 1 {
		t.Errorf("redelivery re-ran the item: %d prd.ready", n)
	}
}

// A grounded codex settles the Re-index only on a codex.indexed for the
// refusal's own product AND branch.
func TestIntegration_CodexIndexed_SettlesOnlyOnOwnBranch(t *testing.T) {
	pool := integrationPool(t)
	ctx := context.Background()
	f := refused(t, pool)
	if err := guards.MarkReindexRequested(ctx, pool, f.Item, "operator"); err != nil {
		t.Fatal(err)
	}
	f.AddModule(t, pool)
	wg := workgraph.New(pool)
	if err := handleCodexIndexed(ctx, pool, wg, &recordingBus{j: &journal{}}, indexedMsg(t, f.Product, "develop", 7)); err != nil {
		t.Fatal(err)
	}
	if got := itemState(t, pool, f.Item); got != "blocked" {
		t.Fatalf("an index of another branch re-ran the item (state %s)", got)
	}
	if err := handleCodexIndexed(ctx, pool, wg, &recordingBus{j: &journal{}}, indexedMsg(t, f.Product, "master", 8)); err != nil {
		t.Fatal(err)
	}
	if got := itemState(t, pool, f.Item); got != "research" {
		t.Fatalf("our branch's index: state %s, want research", got)
	}
}

// fakeIndexer answers /api/index/full with status and records the calls.
type fakeIndexer struct {
	mu     sync.Mutex
	status int
	calls  []string
}

func (f *fakeIndexer) server(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.calls = append(f.calls, r.URL.Path+"?"+r.URL.RawQuery)
		status := f.status
		f.mu.Unlock()
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// A Re-index the indexer refused with 409 (busy with any index) stays
// pending and is re-posted until accepted; an indexer failure withdraws it
// and escalates again.
func TestIntegration_ReindexPoster_RepostsWhileBusy(t *testing.T) {
	pool := integrationPool(t)
	ctx := context.Background()
	f := refused(t, pool)
	if err := guards.MarkReindexRequested(ctx, pool, f.Item, "operator"); err != nil {
		t.Fatal(err)
	}
	idx := &fakeIndexer{status: http.StatusConflict}
	url := idx.server(t)

	postPendingReindex(ctx, pool, url)
	if r, _, _ := guards.LoadRefusal(ctx, pool, f.Item); r.ReindexRequestedAt == nil || r.IndexPostedAt != nil {
		t.Fatalf("after a 409: requested %v posted %v, want still pending", r.ReindexRequestedAt, r.IndexPostedAt)
	}
	idx.mu.Lock()
	idx.status = http.StatusAccepted
	idx.mu.Unlock()
	postPendingReindex(ctx, pool, url)
	r, _, _ := guards.LoadRefusal(ctx, pool, f.Item)
	if r.IndexPostedAt == nil {
		t.Fatal("an accepted re-post must be recorded")
	}
	idx.mu.Lock()
	calls := append([]string(nil), idx.calls...)
	idx.mu.Unlock()
	want := "/api/index/full?branch=master&product_id=" + f.Product.String()
	if len(calls) != 2 || calls[1] != want {
		t.Errorf("indexer calls = %v, want two posts of %s", calls, want)
	}
	// Posted rows are not posted again.
	postPendingReindex(ctx, pool, url)
	idx.mu.Lock()
	again := len(idx.calls)
	idx.mu.Unlock()
	if again != 2 {
		t.Errorf("a posted request was posted again (%d calls)", again)
	}

	g := refused(t, pool)
	if err := guards.MarkReindexRequested(ctx, pool, g.Item, "operator"); err != nil {
		t.Fatal(err)
	}
	idx.mu.Lock()
	idx.status = http.StatusInternalServerError
	idx.mu.Unlock()
	postPendingReindex(ctx, pool, url)
	if r, _, _ := guards.LoadRefusal(ctx, pool, g.Item); r.ReindexRequestedAt != nil {
		t.Error("a failed post must withdraw the request")
	}
	if n := count(t, pool, `SELECT count(*) FROM user_interactions WHERE work_item_id = $1 AND state = 'pending' AND prompt_type = 'escalation'`, g.Item); n != 1 {
		t.Errorf("pending escalations after a failed post = %d, want 1", n)
	}
}

// A refusal whose block is no longer the item's latest (the item moved on
// and was blocked again for another reason) is not replayed by
// codex.indexed; the stale row is dropped.
func TestIntegration_CodexIndexed_StaleRefusalIsNotReplayed(t *testing.T) {
	pool := integrationPool(t)
	ctx := context.Background()
	f := refused(t, pool)
	if err := guards.MarkReindexRequested(ctx, pool, f.Item, "operator"); err != nil {
		t.Fatal(err)
	}
	wg := workgraph.New(pool)
	if err := wg.TransitionState(ctx, f.Item, models.WorkItemStateResearch, "someone", "retried elsewhere"); err != nil {
		t.Fatal(err)
	}
	if err := workgraph.WithTx(ctx, pool, func(tx pgx.Tx) error {
		return guards.BlockWorkItemTx(ctx, tx, f.Item, f.Item, "research-coordinator", "No LLM provider configured.")
	}); err != nil {
		t.Fatal(err)
	}
	f.AddModule(t, pool)
	if err := handleCodexIndexed(ctx, pool, wg, &recordingBus{j: &journal{}}, indexedMsg(t, f.Product, "master", 5)); err != nil {
		t.Fatal(err)
	}
	if got := itemState(t, pool, f.Item); got != "blocked" {
		t.Errorf("state = %s, want blocked (the unrelated block stands)", got)
	}
	if n := count(t, pool, `SELECT count(*) FROM outbox_events WHERE topic = $1 AND key = $2`, bus.TopicPRDReady, f.Item.String()); n != 0 {
		t.Errorf("stale refusal replayed: %d prd.ready", n)
	}
	if _, ok, _ := guards.LoadRefusal(ctx, pool, f.Item); ok {
		t.Error("the stale refusal must be dropped")
	}
}

// A prompt park and restore (blocked → awaiting_user → blocked) keeps the
// refusal's block current: it is still replayed.
func TestIntegration_CodexIndexed_ReplaysAfterPromptRoundTrip(t *testing.T) {
	pool := integrationPool(t)
	ctx := context.Background()
	f := refused(t, pool)
	if err := guards.MarkReindexRequested(ctx, pool, f.Item, "operator"); err != nil {
		t.Fatal(err)
	}
	if err := blockFromWorkBlockedKind(ctx, pool, workgraph.New(pool), &recordingBus{j: &journal{}}, f.Item, "no codex", guards.BlockKindCodexUngrounded); err != nil {
		t.Fatal(err)
	}
	var prompt uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT id FROM user_interactions WHERE work_item_id = $1 AND state = 'pending'`, f.Item).Scan(&prompt); err != nil {
		t.Fatal(err)
	}
	if _, _, err := parkOnPrompt(ctx, pool, f.Item, prompt); err != nil {
		t.Fatal(err)
	}
	if _, _, err := restoreFromPrompt(ctx, pool, f.Item, prompt); err != nil {
		t.Fatal(err)
	}
	f.AddModule(t, pool)
	if err := handleCodexIndexed(ctx, pool, workgraph.New(pool), &recordingBus{j: &journal{}}, indexedMsg(t, f.Product, "master", 5)); err != nil {
		t.Fatal(err)
	}
	if got := itemState(t, pool, f.Item); got != "research" {
		t.Errorf("state = %s, want research (re-run)", got)
	}
}

// An index of the refusal's branch that leaves the codex without a module
// withdraws the Re-index request and escalates again (no silent wait) —
// once our index was posted; an index that ran before ours was accepted
// (the indexer was busy) leaves it waiting.
func TestIntegration_CodexIndexed_StillEmptyEscalatesAgain(t *testing.T) {
	pool := integrationPool(t)
	ctx := context.Background()
	f := refused(t, pool)
	if err := guards.MarkReindexRequested(ctx, pool, f.Item, "operator"); err != nil {
		t.Fatal(err)
	}
	wg := workgraph.New(pool)
	rb := &recordingBus{j: &journal{}}
	if err := handleCodexIndexed(ctx, pool, wg, rb, indexedMsg(t, f.Product, "master", 2)); err != nil {
		t.Fatal(err)
	}
	if r, _, _ := guards.LoadRefusal(ctx, pool, f.Item); r.ReindexRequestedAt == nil {
		t.Fatal("an index that ran before ours was posted must not settle the request")
	}
	if err := guards.MarkIndexPosted(ctx, pool, f.Item); err != nil {
		t.Fatal(err)
	}
	if err := handleCodexIndexed(ctx, pool, wg, rb, indexedMsg(t, f.Product, "master", 3)); err != nil {
		t.Fatal(err)
	}
	if got := itemState(t, pool, f.Item); got != "blocked" {
		t.Fatalf("state = %s, want blocked", got)
	}
	r, ok, _ := guards.LoadRefusal(ctx, pool, f.Item)
	if !ok || r.ReindexRequestedAt != nil {
		t.Fatalf("refusal = %+v ok=%v, want kept with the request withdrawn", r, ok)
	}
	if n := count(t, pool, `SELECT count(*) FROM user_interactions WHERE work_item_id = $1 AND state = 'pending' AND trigger = 'blocked'`, f.Item); n != 1 {
		t.Errorf("pending escalations = %d, want 1", n)
	}
}

// A Re-index no codex.indexed settles (the index failed before writing a
// version) is withdrawn and escalated again after the timeout; a recent one
// is left waiting.
func TestIntegration_ReindexSweep_EscalatesStaleRequests(t *testing.T) {
	pool := integrationPool(t)
	ctx := context.Background()
	stale, fresh := refused(t, pool), refused(t, pool)
	for _, f := range []groundingtest.Fixture{stale, fresh} {
		if err := guards.MarkReindexRequested(ctx, pool, f.Item, "operator"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(ctx, `UPDATE codex_grounding_refusals SET reindex_requested_at = now() - interval '3 hours' WHERE work_item_id = $1`, stale.Item); err != nil {
		t.Fatal(err)
	}
	ids, err := sweepStaleReindex(ctx, pool, 2*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, id := range ids {
		if id == fresh.Item {
			t.Error("a recent request was swept")
		}
		found = found || id == stale.Item
	}
	if !found {
		t.Fatalf("swept %v, want %s", ids, stale.Item)
	}
	if r, _, _ := guards.LoadRefusal(ctx, pool, stale.Item); r.ReindexRequestedAt != nil {
		t.Error("the stale request must be withdrawn")
	}
	if r, _, _ := guards.LoadRefusal(ctx, pool, fresh.Item); r.ReindexRequestedAt == nil {
		t.Error("the recent request must stay pending")
	}
	if n := count(t, pool, `SELECT count(*) FROM user_interactions WHERE work_item_id = $1 AND state = 'pending' AND trigger = 'blocked'`, stale.Item); n != 1 {
		t.Errorf("escalations for the stale item = %d, want 1", n)
	}
}

// codex.gap through the consumer: more than CODEX_GAP_THRESHOLD empty
// lookups in the item's implementation run marks it ungrounded.
func TestIntegration_CodexGap_ThresholdMarksUngrounded(t *testing.T) {
	pool := integrationPool(t)
	ctx := context.Background()
	itemID, _ := seedItem(t, pool, models.WorkItemStateBlocked)
	if err := workgraph.New(pool).TransitionState(ctx, itemID, models.WorkItemStateImplementation, "test", "t"); err != nil {
		t.Fatal(err)
	}
	t.Setenv(codexgap.ThresholdEnv, "2")
	handler := idempotency.NewLedger(pool).Handler(codexIdempotencyConsumer, buildCodexHandler(workgraph.New(pool), &recordingBus{j: &journal{}}, codexgap.ThresholdFromEnv()))
	for i := 0; i < 3; i++ {
		raw, _ := json.Marshal(codexgap.Event{GapID: uuid.NewString(), WorkItemID: itemID.String(), CodexTool: "codex_find_module", Query: "q", FallbackTool: "search_code"})
		if err := handler(ctx, &bus.Message{Topic: bus.TopicCodexGap, Key: itemID.String(), Value: raw}); err != nil {
			t.Fatal(err)
		}
		u, _, err := codexgap.Ungrounded(ctx, pool, itemID)
		if err != nil {
			t.Fatal(err)
		}
		if want := i == 2; u != want {
			t.Errorf("after %d lookups ungrounded = %v, want %v", i+1, u, want)
		}
	}
	// Malformed events are dropped, not retried.
	if err := handler(ctx, &bus.Message{Topic: bus.TopicCodexGap, Value: []byte(`{"work_item_id":"nope","codex_tool":"x"}`)}); err != nil {
		t.Errorf("malformed event: %v", err)
	}
}
