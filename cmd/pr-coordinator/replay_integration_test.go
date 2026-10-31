package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/bus"
	"github.com/autonomousengine/autonomous-engine/internal/evidence"
	"github.com/autonomousengine/autonomous-engine/internal/idempotency"
	"github.com/autonomousengine/autonomous-engine/internal/interactions"
	"github.com/autonomousengine/autonomous-engine/internal/products"
	"github.com/autonomousengine/autonomous-engine/internal/verifier/stage"
	"github.com/autonomousengine/autonomous-engine/internal/workgraph"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// M4-06: a replay item (shadow evaluation) never opens a PR — neither
// through github-service (auto gate) nor as a pending gated change (manual
// gate). A verified branch is sent to review-agent as a PR-less pr.ready
// (replay=true), which status-orchestrator turns into a cancellation after
// the review; with nothing reviewable, replay.complete terminates it. The
// event is recorded in the ledger so a redelivery does nothing.
// Env-gated (TEST_POSTGRES_DSN).
func TestIntegration_ReplayItemNeverOpensAPR(t *testing.T) {
	pool := integrationPool(t)
	for _, tc := range []struct {
		name      string
		gate      string
		evidence  bool
		wantTopic string
	}{
		{"verified branch is reviewed without a PR (auto gate)", "auto", true, bus.TopicPRReady},
		{"verified branch is reviewed without a PR (manual gate)", "manual", true, bus.TopicPRReady},
		{"nothing reviewable terminates the replay (manual gate)", "manual", false, bus.TopicReplayComplete},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			workID, taskID := seedGatedParent(t, pool, "implementation")
			if _, err := pool.Exec(ctx, `UPDATE work_items SET replay = true, pr_gate_mode = $2 WHERE id = $1`, workID, tc.gate); err != nil {
				t.Fatalf("mark replay: %v", err)
			}
			var githubCalls int32
			github := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				atomic.AddInt32(&githubCalls, 1)
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"pr_url":"https://example.invalid/pr/1","pr_number":1}`))
			}))
			defer github.Close()

			h := idempotency.NewLedger(pool).Handler(idempotencyConsumer, buildHandler(workgraph.New(pool),
				interactions.NewPublisher(nil), newTracker(), github.URL,
				products.NewStore(pool), evidence.NewStore(pool), evidence.CompatWindowLegacy))
			msg := completionMessage(t, pool, workID, taskID)
			if tc.evidence {
				msg.Value = verifiedCompletion(t, pool, workID, taskID)
			}
			for i := 0; i < 2; i++ {
				if err := h(ctx, msg); err != nil {
					t.Fatalf("delivery %d: %v", i+1, err)
				}
			}

			if n := atomic.LoadInt32(&githubCalls); n != 0 {
				t.Errorf("github-service calls = %d, want 0", n)
			}
			if n := count(t, pool, `SELECT count(*) FROM pending_pr_changes WHERE work_item_id = $1`, workID); n != 0 {
				t.Errorf("pending PR rows = %d, want 0", n)
			}
			if n := count(t, pool, `SELECT count(*) FROM outbox_events WHERE aggregate_id = $1 AND topic = 'pr.gated'`, workID); n != 0 {
				t.Errorf("pr.gated events = %d, want 0", n)
			}
			if n := count(t, pool, `SELECT count(*) FROM outbox_events WHERE aggregate_id = $1 AND topic = $2`, workID, tc.wantTopic); n != 1 {
				t.Errorf("%s events = %d, want 1", tc.wantTopic, n)
			}
			if tc.wantTopic == bus.TopicPRReady {
				var payload []byte
				if err := pool.QueryRow(ctx, `SELECT payload FROM outbox_events WHERE aggregate_id = $1 AND topic = 'pr.ready'`, workID).Scan(&payload); err != nil {
					t.Fatal(err)
				}
				var m prReadyMessage
				if err := json.Unmarshal(payload, &m); err != nil {
					t.Fatal(err)
				}
				if !m.Replay || m.PRNumber != 0 || m.PRURL != "" || m.HeadSHA == "" || m.Branch == "" {
					t.Errorf("replay pr.ready = %+v", m)
				}
			}
			if n := count(t, pool, `SELECT count(*) FROM work_items WHERE id = $1 AND state = 'implementation'`, workID); n != 1 {
				t.Error("pr-coordinator must not move or block a replay parent")
			}
			if n := ledgerRow(t, pool, msg); n != 1 {
				t.Errorf("ledger rows = %d, want 1", n)
			}
		})
	}
}

// verifiedCompletion is an implementation.complete carrying valid verified
// evidence for the fixture subtask and its repository.
func verifiedCompletion(t *testing.T, pool *pgxpool.Pool, workID, taskID uuid.UUID) []byte {
	t.Helper()
	var repoID string
	if err := pool.QueryRow(context.Background(), `SELECT repository_id::text FROM work_items WHERE id = $1`, taskID).Scan(&repoID); err != nil {
		t.Fatal(err)
	}
	ev := evidenceJSON(t, true, func(ev *evidence.ImplementationEvidence) {
		ev.WorkID, ev.TaskID, ev.RepositoryID = workID.String(), taskID.String(), &repoID
	})
	b, err := json.Marshal(map[string]any{"task_id": taskID.String(), "work_id": workID.String(), "success": true, "evidence": ev})
	if err != nil {
		t.Fatal(err)
	}
	// R14: as the verification stage publishes it after approving the
	// evidence's head.
	var decoded evidence.ImplementationEvidence
	if err := json.Unmarshal(ev, &decoded); err != nil {
		t.Fatal(err)
	}
	return verificationOf(t, b, stage.DecisionApprove, decoded.HeadSHA)
}
