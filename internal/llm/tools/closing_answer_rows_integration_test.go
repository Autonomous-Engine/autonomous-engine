package tools

import (
	"context"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/llm/adapter"
	"github.com/google/uuid"
)

// The closing answer (CloseAtLimit, the research cap, a stuck pass) is the
// one call of a loop that leaves no inference_logs row: live, the
// verifier's verdict call after the cap was neither recorded nor metered,
// and its answer — the one the parser rejected — could not be read back.
// Every call of a loop is recorded with its tokens and its answer.
func TestIntegration_TheClosingAnswerIsRecorded(t *testing.T) {
	pool := toolsIntegrationPool(t)
	ctx := context.Background()
	productID, workID := uuid.New(), uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO products (id, name) VALUES ($1, $2)`, productID, "close-"+productID.String()[:8]); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO work_items (id, type, state, title, product_id) VALUES ($1, 'feature', 'verification', 'closing row', $2)`, workID, productID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM inference_logs WHERE work_item_id = $1`, workID)
		_, _ = pool.Exec(ctx, `DELETE FROM work_items WHERE id = $1`, workID)
		_, _ = pool.Exec(ctx, `DELETE FROM products WHERE id = $1`, productID)
	})
	t.Setenv("WORK_ITEM_TOKEN_BUDGET", "500000")
	ex := NewExecutor(t.TempDir())
	req := adapter.CompletionRequest{Messages: []adapter.ChatMessage{{Role: adapter.RoleUser, Content: "go"}}, Tools: SafeImplementationTools()}
	resp, err := RunToolLoopWithOptions(ctx, &closingListingAdapter{}, req, ex, 1, pool, &workID, LoopOptions{CloseAtLimit: true})
	if err != nil || resp == nil || resp.Content != "the closing answer" {
		t.Fatalf("resp=%+v err=%v, want the closing answer", resp, err)
	}
	var rows int
	var last string
	var work *int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM inference_logs WHERE work_item_id = $1 AND intent_type = 'tool_loop'`, workID).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT response_content, work_tokens FROM inference_logs WHERE work_item_id = $1 AND intent_type = 'tool_loop' ORDER BY created_at DESC LIMIT 1`, workID).Scan(&last, &work); err != nil {
		t.Fatal(err)
	}
	if rows != 2 || last != "the closing answer" || work == nil || *work <= 0 {
		t.Fatalf("rows=%d last=%q work=%v, want the tool turn and the closing call both recorded with the answer and its work", rows, last, work)
	}
}

// closingListingAdapter lists files while tools are offered and answers
// when they are not.
type closingListingAdapter struct{ listingAdapter }

func (a *closingListingAdapter) Complete(ctx context.Context, req adapter.CompletionRequest) (*adapter.CompletionResponse, error) {
	if len(req.Tools) == 0 {
		return &adapter.CompletionResponse{Content: "the closing answer", FinishReason: "stop", InputTokens: 150, OutputTokens: 20}, nil
	}
	return a.listingAdapter.Complete(ctx, req)
}
