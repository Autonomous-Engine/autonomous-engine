package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/evidence"
	"github.com/autonomousengine/autonomous-engine/internal/idempotency"
	"github.com/autonomousengine/autonomous-engine/internal/interactions"
	"github.com/autonomousengine/autonomous-engine/internal/products"
	"github.com/autonomousengine/autonomous-engine/internal/workgraph"
)

// Live (ciblame item eebeeb1e, gate auto, no GitHub token): github-service
// returned no PR, pr-coordinator logged "skipping pr.ready publish" and
// moved on — no pending change, no prompt, no block; the parent sat in
// verification with nothing for anyone to answer. A verified branch that
// github-service cannot open a PR for goes through the manual gate instead,
// exactly as a non-GitHub remote does: a pending change and a "Create PR"
// prompt whose text says why, so a person can retry once the cause (a
// missing token, a GitHub outage) is fixed.
func TestIntegration_NoPRFromGitHubServiceRoutesThroughManualGate(t *testing.T) {
	pool := integrationPool(t)
	ctx := context.Background()
	workID, taskID := seedGatedParent(t, pool, "implementation")
	if _, err := pool.Exec(ctx, `UPDATE work_items SET pr_gate_mode = 'auto' WHERE id = $1`, workID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE repositories SET github_url = 'https://github.com/example/nopr.git' WHERE id = (SELECT repository_id FROM work_items WHERE id = $1)`, taskID); err != nil {
		t.Fatal(err)
	}
	var githubCalls int32
	github := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&githubCalls, 1)
		http.Error(w, `{"error":"no GitHub credential for this repository"}`, http.StatusBadGateway)
	}))
	defer github.Close()
	h := idempotency.NewLedger(pool).Handler(idempotencyConsumer, buildHandler(workgraph.New(pool),
		interactions.NewPublisher(nil), newTracker(), github.URL,
		products.NewStore(pool), evidence.NewStore(pool), evidence.CompatWindowLegacy))
	msg := completionMessage(t, pool, workID, taskID)
	msg.Value = verifiedCompletion(t, pool, workID, taskID)
	if err := h(ctx, msg); err != nil {
		t.Fatalf("delivery: %v", err)
	}

	if n := atomic.LoadInt32(&githubCalls); n != 1 {
		t.Errorf("github-service calls = %d, want 1 (auto mode asks GitHub first)", n)
	}
	if n := count(t, pool, `SELECT count(*) FROM pending_pr_changes WHERE work_item_id = $1 AND status = 'pending'`, workID); n != 1 {
		t.Errorf("pending PR rows = %d, want 1 — the branch is gated when GitHub returns no PR", n)
	}
	if n := count(t, pool, `SELECT count(*) FROM outbox_events WHERE aggregate_id = $1 AND topic = 'pr.gated'`, workID); n != 1 {
		t.Errorf("pr.gated events = %d, want 1", n)
	}
	if n := count(t, pool, `SELECT count(*) FROM outbox_events WHERE aggregate_id = $1 AND topic = 'pr.ready'`, workID); n != 0 {
		t.Errorf("pr.ready events = %d, want 0 (no PR exists)", n)
	}
	var question string
	if err := pool.QueryRow(ctx, `SELECT question FROM user_interactions WHERE work_item_id = $1 AND trigger = $2 AND state = 'pending'`, workID, string(interactions.TriggerApprovalPRGate)).Scan(&question); err != nil {
		t.Fatalf("no pending pr-gate prompt: %v", err)
	}
	if !strings.Contains(question, "github-service") || !strings.Contains(question, "no GitHub credential") {
		t.Errorf("prompt question = %q, want it to say github-service opened no PR and why", question)
	}
	if n := count(t, pool, `SELECT count(*) FROM work_items WHERE id = $1 AND state = 'blocked'`, workID); n != 0 {
		t.Error("the parent must not be blocked: the gate is the way forward")
	}
}
