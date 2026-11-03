package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/models"
	"github.com/autonomousengine/autonomous-engine/internal/workgraph"
	"github.com/google/uuid"
)

// PRD review offered only Approve or Reject, and Reject was terminal: a
// reviewer who saw a fixable mistake had to approve it or kill the work
// (F34). Request changes sends the item back through intake with the
// feedback, so the product agent regenerates the PRD.
func TestIntegration_RevisePRDRegeneratesWithFeedback(t *testing.T) {
	pool := runsIntegrationPool(t)
	ctx := context.Background()
	productID, workID := uuid.New(), uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO products (id, name) VALUES ($1, $2)`, productID, "revise-"+productID.String()[:8]); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO work_items (id, type, state, title, description, product_id) VALUES ($1, 'feature', 'prd_approval', 'Add parser option', 'Custom charset mappings', $2)`, workID, productID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM outbox_events WHERE aggregate_id = $1`, workID)
		_, _ = pool.Exec(ctx, `DELETE FROM work_items WHERE id = $1`, workID)
		_, _ = pool.Exec(ctx, `DELETE FROM products WHERE id = $1`, productID)
	})

	h := NewHandlers(workgraph.New(pool), nil, nil, nil, nil, nil)
	feedback := "Name the option type Option (the existing type), not ParserOption."
	rec := httptest.NewRecorder()
	h.RevisePRD(rec, httptest.NewRequest(http.MethodPost, "/api/work-items/revise-prd?id="+workID.String(), strings.NewReader(`{"reason":"`+feedback+`"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}

	var state string
	if err := pool.QueryRow(ctx, `SELECT state FROM work_items WHERE id = $1`, workID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != string(models.WorkItemStatePRD) {
		t.Fatalf("state = %q, want prd (the regenerate edge)", state)
	}
	var reason string
	if err := pool.QueryRow(ctx, `SELECT reason FROM work_item_transitions WHERE work_item_id = $1 ORDER BY "timestamp" DESC LIMIT 1`, workID).Scan(&reason); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reason, feedback) {
		t.Fatalf("transition reason = %q, want the feedback", reason)
	}
	var guidance string
	if err := pool.QueryRow(ctx, `SELECT payload->'metadata'->>'user_guidance' FROM outbox_events WHERE aggregate_id = $1 AND topic = 'intake' ORDER BY created_at DESC LIMIT 1`, workID).Scan(&guidance); err != nil {
		t.Fatalf("intake event: %v", err)
	}
	if guidance != feedback {
		t.Fatalf("intake user_guidance = %q, want the feedback", guidance)
	}

	// Only a PRD awaiting approval can be sent back.
	rec = httptest.NewRecorder()
	h.RevisePRD(rec, httptest.NewRequest(http.MethodPost, "/api/work-items/revise-prd?id="+workID.String(), strings.NewReader(`{"reason":"again"}`)))
	if rec.Code != http.StatusConflict {
		t.Fatalf("second revise: %d %s, want 409", rec.Code, rec.Body.String())
	}
}

func TestRevisePRDRequiresFeedback(t *testing.T) {
	h := NewHandlers(nil, nil, nil, nil, nil, nil)
	rec := httptest.NewRecorder()
	h.RevisePRD(rec, httptest.NewRequest(http.MethodPost, "/api/work-items/revise-prd?id="+uuid.New().String(), strings.NewReader(`{"reason":"  "}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("%d %s, want 400 for empty feedback", rec.Code, rec.Body.String())
	}
}

// While the approval prompt is pending the orchestrator parks the item in
// awaiting_user, so "Request changes" arrives as the prompt's answer with
// action "revise" (rerun NEW-5). It must regenerate like RevisePRD.
func TestIntegration_PRDPromptAnswerReviseRegenerates(t *testing.T) {
	pool := runsIntegrationPool(t)
	ctx := context.Background()
	productID, workID := uuid.New(), uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO products (id, name) VALUES ($1, $2)`, productID, "revise2-"+productID.String()[:8]); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO work_items (id, type, state, title, description, product_id) VALUES ($1, 'feature', 'awaiting_user', 'Add parser option', 'Custom charset mappings', $2)`, workID, productID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM outbox_events WHERE aggregate_id = $1`, workID)
		_, _ = pool.Exec(ctx, `DELETE FROM work_items WHERE id = $1`, workID)
		_, _ = pool.Exec(ctx, `DELETE FROM products WHERE id = $1`, productID)
	})
	h := NewHandlers(workgraph.New(pool), nil, nil, nil, nil, nil)
	if err := h.HandlePRDPromptAnswer(ctx, workID, "revise", "   ", "u1"); err == nil {
		t.Fatal("empty feedback must be refused")
	}
	feedback := "Record an unsupported target charset on Envelope.Errors instead of failing the parse."
	if err := h.HandlePRDPromptAnswer(ctx, workID, "revise", feedback, "u1"); err != nil {
		t.Fatalf("revise via prompt answer: %v", err)
	}
	var state, guidance string
	if err := pool.QueryRow(ctx, `SELECT state FROM work_items WHERE id = $1`, workID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != string(models.WorkItemStatePRD) {
		t.Fatalf("state = %q, want prd", state)
	}
	if err := pool.QueryRow(ctx, `SELECT payload->'metadata'->>'user_guidance' FROM outbox_events WHERE aggregate_id = $1 AND topic = 'intake' ORDER BY created_at DESC LIMIT 1`, workID).Scan(&guidance); err != nil || guidance != feedback {
		t.Fatalf("intake guidance = %q err=%v, want the feedback", guidance, err)
	}
}
