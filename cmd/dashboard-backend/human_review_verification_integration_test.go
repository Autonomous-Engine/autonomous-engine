package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/bus"
	"github.com/autonomousengine/autonomous-engine/internal/workgraph"
	"github.com/google/uuid"
)

// Live (ciblame item 0f565594), the verifier escalated one criterion to a
// person with everything else met; the person approved the human-review
// prompt, and the dashboard sent the parent to QA — the pre-R14 step —
// where the parent (no repository of its own) blocked: "Cannot run QA: no
// repository available". Nobody told pr-coordinator, which had refused the
// PR on the escalation. A person's approval of an escalated verification
// IS the verification decision: an approve result is recorded for the
// escalated subtask and its verification.complete is re-enqueued as
// approve, so pr-coordinator opens the PR (or the manual gate's pending
// change); the parent returns to human_review, not QA.
func TestIntegration_HumanReviewApprovalAfterEscalationBecomesTheApproveVerification(t *testing.T) {
	pool := refusalPool(t)
	ctx := context.Background()
	product, parent, sub := uuid.New(), uuid.New(), uuid.New()
	head := strings.Repeat("a", 40)
	escalated := map[string]any{
		"work_id": parent.String(), "task_id": sub.String(), "head_sha": head, "branch": "agent/x",
		"decision": "escalate", "reason": "recorded gaps need human review (criteria AC-3); everything else met",
		"result_id": uuid.New().String(), "defects": map[string]int{"critical": 0, "major": 0, "minor": 1},
		"implementation": map[string]any{"work_id": parent.String(), "task_id": sub.String(), "success": true, "branch": "agent/x", "head_sha": head},
		"event_id": uuid.New().String(), "timestamp": "2026-11-03T19:03:13Z",
	}
	payload, _ := json.Marshal(escalated)
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO products (id, name) VALUES ($1, $2)`, []any{product, "hr-" + product.String()[:8]}},
		{`INSERT INTO work_items (id, type, state, previous_state, title, product_id) VALUES ($1, 'feature', 'awaiting_user', 'human_review', 'parent', $2)`, []any{parent, product}},
		{`INSERT INTO work_items (id, type, state, title, product_id, parent_id) VALUES ($1, 'subtask', 'passed', 'sub', $2, $3)`, []any{sub, product, parent}},
		{`INSERT INTO verification_results (id, work_item_id, task_id, head_sha, decision, reason, verifier_version) VALUES ($1, $2, $3, $4, 'escalate', 'recorded gaps need human review', '7')`, []any{uuid.New(), parent, sub, head}},
		{`INSERT INTO outbox_events (id, aggregate_type, aggregate_id, topic, key, payload, headers, created_at, published_at) VALUES ($1, 'work_item', $2, $3, $4, $5::jsonb, '{}'::jsonb, NOW() - interval '1 minute', NOW() - interval '50 seconds')`, []any{uuid.New(), parent, bus.TopicVerificationComplete, parent.String(), string(payload)}},
	} {
		if _, err := pool.Exec(ctx, q.sql, q.args...); err != nil {
			t.Fatalf("%s: %v", q.sql, err)
		}
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM outbox_events WHERE aggregate_id = $1`, parent)
		_, _ = pool.Exec(ctx, `DELETE FROM verification_results WHERE work_item_id = $1`, parent)
		_, _ = pool.Exec(ctx, `DELETE FROM work_items WHERE product_id = $1`, product)
		_, _ = pool.Exec(ctx, `DELETE FROM products WHERE id = $1`, product)
	})

	h := NewHandlers(workgraph.New(pool), nil, nil, nil, nil, nil)
	if err := h.HandleHumanReviewPromptAnswer(ctx, parent, "approve", "", "operator"); err != nil {
		t.Fatalf("approve: %v", err)
	}

	var decision, resultHead, version string
	if err := pool.QueryRow(ctx, `SELECT decision, head_sha, verifier_version FROM verification_results WHERE task_id = $1 ORDER BY created_at DESC LIMIT 1`, sub).Scan(&decision, &resultHead, &version); err != nil {
		t.Fatal(err)
	}
	if decision != "approve" || resultHead != head || !strings.HasPrefix(version, "human") {
		t.Fatalf("latest result = %s %s %s, want the person's approve for the escalated head", decision, resultHead, version)
	}
	var raw []byte
	if err := pool.QueryRow(ctx, `SELECT payload FROM outbox_events WHERE aggregate_id = $1 AND topic = $2 AND published_at IS NULL ORDER BY created_at DESC LIMIT 1`, parent, bus.TopicVerificationComplete).Scan(&raw); err != nil {
		t.Fatalf("no verification.complete re-enqueued: %v", err)
	}
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	if m["decision"] != "approve" || m["task_id"] != sub.String() || m["head_sha"] != head || m["implementation"] == nil || !strings.Contains(m["reason"].(string), "operator") {
		t.Fatalf("re-enqueued payload = %s, want approve for the task with its implementation payload and the person named", raw)
	}
	var state string
	var qaReady int
	_ = pool.QueryRow(ctx, `SELECT state FROM work_items WHERE id = $1`, parent).Scan(&state)
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM outbox_events WHERE aggregate_id = $1 AND topic = $2`, parent, bus.TopicQAReady).Scan(&qaReady)
	if state != "human_review" || qaReady != 0 {
		t.Fatalf("parent state = %s, qa.ready events = %d; want human_review and no QA", state, qaReady)
	}
}
