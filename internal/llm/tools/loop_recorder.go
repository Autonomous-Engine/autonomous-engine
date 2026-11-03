package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/llm"
	"github.com/autonomousengine/autonomous-engine/internal/llm/adapter"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// loopRecorder records every LLM call of a tool loop — the tool turns and
// the closing answer alike — in inference_logs with its tokens, its work
// (work_tokens.go) and its answer. The closing answer used to bypass the
// loop's inline recording: the verdict call after the verifier's cap was
// neither metered nor readable afterwards.
type loopRecorder struct {
	db         *pgxpool.Pool
	workItemID *uuid.UUID
	sent       *sentTracker
}

// complete makes one call of the loop through call and records it. It
// returns the response and the call's work tokens: the input it had not
// sent before plus its output, never more than the input the call reported.
func (r *loopRecorder) complete(ctx context.Context, llmAdapter adapter.ModelAdapter, req adapter.CompletionRequest, call func() (*adapter.CompletionResponse, error)) (*adapter.CompletionResponse, int64, error) {
	if r == nil {
		resp, err := call()
		return resp, workOf(resp, 0), err
	}
	logID := r.start(ctx, llmAdapter)
	newInput := r.sent.newInputTokens(req)
	callStart := time.Now()
	resp, err := call()
	latency := time.Since(callStart)
	work := workOf(resp, newInput)
	r.finish(ctx, logID, req, resp, err, latency, work)
	return resp, work, err
}

// workOf is a call's work: output plus the input it added (capped by the
// input the call reported).
func workOf(resp *adapter.CompletionResponse, newInput int64) int64 {
	if resp == nil {
		return 0
	}
	if resp.InputTokens > 0 && newInput > int64(resp.InputTokens) {
		newInput = int64(resp.InputTokens)
	}
	return newInput + int64(resp.OutputTokens)
}

// start inserts the inference_logs row BEFORE the call so the dashboard can
// show it as "running" while it is in flight; latency_ms=0 is its "still
// running" marker. Returns "" when nothing is recorded (no db, no item).
func (r *loopRecorder) start(ctx context.Context, llmAdapter adapter.ModelAdapter) string {
	if r.db == nil || r.workItemID == nil {
		return ""
	}
	// agent_id is a UUID column; a literal string there made the insert
	// fail silently. Left NULL — intent_type="tool_loop" identifies the
	// rows. M4-05: the caller's inference tags come from ctx.
	tags := llm.InferenceTagsFrom(ctx)
	provider, modelID := llm.ModelIdentity(llmAdapter.Name(), tags)
	var logID string
	if err := r.db.QueryRow(ctx,
		`INSERT INTO inference_logs (model_provider, model_id, intent_type, input_tokens, output_tokens, latency_ms, success, error_message, work_item_id, agent_id, created_at,
		                             role, agent_type, config_snapshot_id)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, NULL, NOW(), $10, $11, $12)
		 RETURNING id::text`,
		provider, modelID, "tool_loop", 0, 0,
		int64(0), false, "", r.workItemID,
		llm.NullIfEmpty(tags.Role), llm.NullIfEmpty(tags.AgentType), llm.NullIfEmpty(tags.ConfigSnapshotID),
	).Scan(&logID); err != nil {
		fmt.Printf("tool-executor: insert in-progress inference_logs row failed: %v\n", err)
		return ""
	}
	if logID != "" {
		_, _ = r.db.Exec(ctx, "SELECT pg_notify('inference_started', $1)", logID)
	}
	return logID
}

// finish updates the row with the call's outcome, conversation and work.
func (r *loopRecorder) finish(ctx context.Context, logID string, req adapter.CompletionRequest, resp *adapter.CompletionResponse, err error, latency time.Duration, work int64) {
	if logID == "" {
		return
	}
	errMsg := ""
	if err != nil {
		errMsg = err.Error()
	}
	var inputTokens, outputTokens int
	responseText := ""
	if resp != nil {
		inputTokens = resp.InputTokens
		outputTokens = resp.OutputTokens
		responseText = resp.Content
	}
	messagesJSON := []byte("[]")
	if b, mErr := json.Marshal(req.Messages); mErr == nil {
		messagesJSON = b
	}
	_, _ = r.db.Exec(ctx,
		`UPDATE inference_logs
		 SET input_tokens = $1, output_tokens = $2, latency_ms = $3,
		     success = $4, error_message = $5,
		     prompt_messages = $6::jsonb, response_content = $7,
		     work_tokens = $9
		 WHERE id = $8::uuid`,
		inputTokens, outputTokens, latency.Milliseconds(),
		err == nil, errMsg, string(messagesJSON), responseText, logID,
		work,
	)
	_, _ = r.db.Exec(ctx, "SELECT pg_notify('inference_completed', $1)", logID)
}
