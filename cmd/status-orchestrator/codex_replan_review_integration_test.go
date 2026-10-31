package main

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/bus"
	"github.com/autonomousengine/autonomous-engine/internal/codex"
	"github.com/autonomousengine/autonomous-engine/internal/models"
	"github.com/autonomousengine/autonomous-engine/internal/verifier/stage"
	"github.com/autonomousengine/autonomous-engine/internal/workgraph"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Codex coherence Part 1, review fixes (status-orchestrator side).

// Review fix 2: an implementation.complete from a subtask the codex re-plan
// superseded does not walk the re-planning parent to implementation.
func TestIntegration_CodexReplan_CompletionFromASupersededSubtaskIsIgnored(t *testing.T) {
	pool := integrationPool(t)
	f := newReplanFixture(t, pool)
	r := newTestReplanner(pool, testReplanConfig())
	f.version("main", "module:a")
	parent, subs := f.parent(models.WorkItemStateImplementation, "main", 1, []string{"a"}, models.WorkItemStateImplementation)
	replay(t, codexHandler(pool, r), f.version("main", "module:a"), 1)
	f.due()
	f.sweep(r)
	if s := itemState(t, pool, parent); s != "planning" {
		t.Fatalf("parent = %s, want planning", s)
	}
	msg := eventMessage(t, bus.TopicImplementationComplete, map[string]any{"task_id": subs[0].String(), "work_id": parent.String(), "success": true})
	cleanupLedgerRow(t, pool, msg)
	replay(t, guardedOrchestrator(t, pool), msg, 1)
	if s := itemState(t, pool, parent); s != "planning" {
		t.Fatalf("a superseded subtask's completion moved the parent to %s", s)
	}
	if s := itemState(t, pool, subs[0]); s != "blocked" {
		t.Fatalf("superseded subtask = %s, want blocked", s)
	}
}

// approved records an approving verification result for sub.
func approved(t *testing.T, f *replanFixture, parent, sub uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	if err := workgraph.WithTx(ctx, f.pool, func(tx pgx.Tx) error {
		return stage.InsertResultTx(ctx, tx, &stage.Result{WorkID: parent, TaskID: sub, HeadSHA: strings.Repeat("a", 40), Decision: stage.DecisionApprove, Reason: "approved"})
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = f.pool.Exec(context.Background(), `DELETE FROM verification_results WHERE work_item_id = $1`, parent)
	})
}

// Review fix 4: the re-emitted brief names its replanned event.
func TestIntegration_CodexReplan_BriefCarriesTheReplanEventID(t *testing.T) {
	pool := integrationPool(t)
	f := newReplanFixture(t, pool)
	r := newTestReplanner(pool, testReplanConfig())
	f.version("main", "module:a")
	parent, _ := f.parent(models.WorkItemStateImplementation, "main", 1, []string{"a"})
	replay(t, codexHandler(pool, r), f.version("main", "module:a"), 1)
	f.due()
	f.sweep(r)
	var payload []byte
	if err := pool.QueryRow(context.Background(), `SELECT payload FROM outbox_events WHERE topic = 'technical-brief.ready' AND aggregate_id = $1`, parent).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	_ = json.Unmarshal(payload, &m)
	var id string
	if err := pool.QueryRow(context.Background(), `SELECT id::text FROM work_item_codex_events WHERE work_item_id = $1 AND kind = 'replanned'`, parent).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if m["replan"] != true || m["replan_event_id"] != id {
		t.Fatalf("brief = %v, want replan_event_id %s", m, id)
	}
}

// Review fix 4 (M1): two sweeps of the same change at once (a lease that
// expired mid-sweep) re-plan once: the history is re-read under the
// parent's row lock.
func TestIntegration_CodexReplan_ConcurrentDoubleSweepReplansOnce(t *testing.T) {
	pool := integrationPool(t)
	f := newReplanFixture(t, pool)
	cfg := testReplanConfig()
	cfg.Max = 1
	f.version("main", "module:a")
	parent, _ := f.parent(models.WorkItemStateImplementation, "main", 1, []string{"a"}, models.WorkItemStateImplementation)
	f.version("main", "module:a")
	var wg sync.WaitGroup
	errs := make(chan error, 4)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- newTestReplanner(pool, cfg).sweep(context.Background(), codex.PendingSweep{ProductID: f.product, Branch: "main"})
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("sweep: %v", err)
		}
	}
	if n := f.events(parent, codex.EventReplanned); n != 1 {
		t.Fatalf("re-plans = %d, want 1", n)
	}
	if n := f.events(parent, codex.EventEscalated); n != 0 {
		t.Fatalf("escalations = %d, want 0 (the cap is counted once)", n)
	}
	if n := f.briefs(parent); n != 1 {
		t.Fatalf("briefs = %d, want 1", n)
	}
}

// Review fix 5 (ruling): an automatic re-plan never discards verified
// work. A parent in implementation whose current round already has an
// approved subtask is not re-planned: the change is noted and the operator
// is asked.
func TestIntegration_CodexReplan_VerifiedWorkEscalatesInsteadOfReplanning(t *testing.T) {
	pool := integrationPool(t)
	f := newReplanFixture(t, pool)
	r := newTestReplanner(pool, testReplanConfig())
	f.version("main", "module:a")
	parent, subs := f.parent(models.WorkItemStateImplementation, "main", 1, []string{"a"}, models.WorkItemStatePassed, models.WorkItemStateImplementation)
	approved(t, f, parent, subs[0])
	replay(t, codexHandler(pool, r), f.version("main", "module:a"), 1)
	f.due()
	f.sweep(r)
	if s := itemState(t, pool, parent); s != "implementation" {
		t.Fatalf("parent = %s, want implementation", s)
	}
	if s := itemState(t, pool, subs[1]); s != "implementation" {
		t.Fatalf("running subtask = %s, want implementation", s)
	}
	if n := f.briefs(parent); n != 0 {
		t.Fatalf("briefs = %d, want 0", n)
	}
	if n := f.events(parent, codex.EventNoted); n != 1 {
		t.Errorf("notes = %d, want 1", n)
	}
	var cause, question string
	if err := pool.QueryRow(context.Background(), `SELECT e.cause, u.question FROM work_item_codex_events e JOIN user_interactions u ON u.id = e.prompt_id WHERE e.work_item_id = $1 AND e.kind = 'escalated'`, parent).Scan(&cause, &question); err != nil {
		t.Fatalf("escalation: %v", err)
	}
	if cause != causeVerifiedWork || !strings.Contains(question, "already verified") {
		t.Errorf("escalation cause=%q question=%q", cause, question)
	}
	// The operator may still choose Re-plan for this escalation.
	prompt := promptOf(t, f, parent)
	if _, _, err := parkOnPrompt(context.Background(), pool, parent, prompt); err != nil {
		t.Fatal(err)
	}
	answer(t, f, parent, prompt, "replan")
	if s := itemState(t, pool, parent); s != "planning" {
		t.Fatalf("parent after the operator's Re-plan = %s, want planning", s)
	}
}

func promptOf(t *testing.T, f *replanFixture, parent uuid.UUID) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := f.pool.QueryRow(context.Background(), `SELECT id FROM user_interactions WHERE work_item_id = $1 AND trigger = 'codex:replan' AND state = 'pending' ORDER BY requested_at DESC LIMIT 1`, parent).Scan(&id); err != nil {
		t.Fatalf("pending codex:replan prompt: %v", err)
	}
	return id
}

// Review fix 3: the operator's Re-plan is refused when, while the prompt
// was pending, a PR was opened or a subtask got verified: the change is
// noted and the operator is told (continue / cancel only).
func TestIntegration_CodexReplan_ReplanAnswerRefusedForAnOpenPROrNewlyVerifiedWork(t *testing.T) {
	pool := integrationPool(t)
	for name, arrange := range map[string]func(f *replanFixture, parent, sub uuid.UUID){
		"open PR": func(f *replanFixture, parent, _ uuid.UUID) {
			if _, err := f.pool.Exec(context.Background(), `INSERT INTO outbox_events (aggregate_type, aggregate_id, topic, key, payload) VALUES ('work_item', $1, 'pr.ready', $2, '{"pr_url":"https://example.invalid/pr/2"}')`, parent, parent.String()); err != nil {
				t.Fatal(err)
			}
		},
		"newly verified subtask": func(f *replanFixture, parent, sub uuid.UUID) {
			if _, err := f.pool.Exec(context.Background(), `UPDATE work_items SET state = 'passed' WHERE id = $1`, sub); err != nil {
				t.Fatal(err)
			}
			approved(t, f, parent, sub)
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newReplanFixture(t, pool)
			r := newTestReplanner(pool, testReplanConfig())
			parent, sub, prompt := escalated(t, f, r, models.WorkItemStateImplementation)
			arrange(f, parent, sub)
			answer(t, f, parent, prompt, "replan")
			if s := itemState(t, pool, parent); s != "implementation" {
				t.Fatalf("parent = %s, want implementation (re-plan refused)", s)
			}
			if n := f.briefs(parent); n != 0 {
				t.Fatalf("briefs = %d, want 0", n)
			}
			if n := f.count(`SELECT count(*) FROM work_item_codex_events WHERE work_item_id = $1 AND kind = 'noted' AND cause = 'refused'`, parent); n != 1 {
				t.Errorf("refusal notes = %d, want 1", n)
			}
			var actions []byte
			if err := pool.QueryRow(context.Background(), `SELECT actions FROM user_interactions WHERE work_item_id = $1 AND trigger = 'codex:replan' AND state = 'pending'`, parent).Scan(&actions); err != nil {
				t.Fatalf("refusal prompt: %v", err)
			}
			if strings.Contains(string(actions), `"replan"`) || !strings.Contains(string(actions), `"continue"`) || !strings.Contains(string(actions), `"cancel"`) {
				t.Errorf("refusal prompt actions = %s, want continue and cancel only", actions)
			}
		})
	}
}

// Review fix 3: "Continue as is" re-enqueues the parent's latest
// verification.complete, so pr-coordinator (which held the PR while the
// escalation was pending) opens it.
func TestIntegration_CodexReplan_ContinueReenqueuesTheLatestVerification(t *testing.T) {
	pool := integrationPool(t)
	f := newReplanFixture(t, pool)
	r := newTestReplanner(pool, testReplanConfig())
	parent, sub, prompt := escalated(t, f, r, models.WorkItemStateImplementation)
	if _, err := pool.Exec(context.Background(), `INSERT INTO outbox_events (aggregate_type, aggregate_id, topic, key, payload, created_at) VALUES ('work_item', $1, 'verification.complete', $2, $3, now() - interval '1 minute')`,
		parent, parent.String(), `{"work_id":"`+parent.String()+`","task_id":"`+sub.String()+`","decision":"approve","event_id":"old"}`); err != nil {
		t.Fatal(err)
	}
	answer(t, f, parent, prompt, "continue")
	rows, err := pool.Query(context.Background(), `SELECT payload FROM outbox_events WHERE aggregate_id = $1 AND topic = 'verification.complete' ORDER BY created_at`, parent)
	if err != nil {
		t.Fatal(err)
	}
	var payloads []map[string]any
	for rows.Next() {
		var b []byte
		_ = rows.Scan(&b)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		payloads = append(payloads, m)
	}
	rows.Close()
	if len(payloads) != 2 || payloads[1]["task_id"] != sub.String() || payloads[1]["decision"] != "approve" || payloads[1]["event_id"] == "old" {
		t.Fatalf("verification.complete events = %v, want the latest one re-enqueued with a new event id", payloads)
	}
}

// "Continue as is" on a parent whose verification ran (pr-coordinator held
// its PR) but whose verification.complete outbox row is gone (pruned): the
// held PR cannot be re-decided, so the item is not left silently waiting —
// it is blocked with the Retry / Cancel escalation saying so.
func TestIntegration_CodexReplan_ContinueWithoutAVerificationToReenqueueEscalates(t *testing.T) {
	pool := integrationPool(t)
	f := newReplanFixture(t, pool)
	r := newTestReplanner(pool, testReplanConfig())
	parent, sub, prompt := escalated(t, f, r, models.WorkItemStateImplementation)
	approved(t, f, parent, sub)
	answer(t, f, parent, prompt, "continue")

	if n := f.count(`SELECT count(*) FROM outbox_events WHERE aggregate_id = $1 AND topic = 'verification.complete'`, parent); n != 0 {
		t.Fatalf("verification.complete events = %d, want 0 (nothing to re-enqueue)", n)
	}
	if n := f.events(parent, codex.EventContinued); n != 1 {
		t.Errorf("continued events = %d, want 1", n)
	}
	if s := itemState(t, pool, parent); s != "blocked" {
		t.Fatalf("parent = %s, want blocked (the held PR could not be re-decided)", s)
	}
	var question string
	var actions []byte
	if err := pool.QueryRow(context.Background(), `SELECT question, actions FROM user_interactions WHERE work_item_id = $1 AND trigger = 'blocked' AND state = 'pending'`, parent).Scan(&question, &actions); err != nil {
		t.Fatalf("Retry / Cancel escalation: %v", err)
	}
	if !strings.Contains(question, "could not be re-decided") {
		t.Errorf("escalation question = %q, want it to say the held PR could not be re-decided", question)
	}
	if !strings.Contains(string(actions), `"retry"`) || !strings.Contains(string(actions), `"cancel"`) {
		t.Errorf("escalation actions = %s, want retry and cancel", actions)
	}
}
