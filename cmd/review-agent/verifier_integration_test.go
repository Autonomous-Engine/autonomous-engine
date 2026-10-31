package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/agentregistry"
	"github.com/autonomousengine/autonomous-engine/internal/bus"
	"github.com/autonomousengine/autonomous-engine/internal/evidence"
	"github.com/autonomousengine/autonomous-engine/internal/idempotency"
	"github.com/autonomousengine/autonomous-engine/internal/models"
	"github.com/autonomousengine/autonomous-engine/internal/verifier"
	"github.com/autonomousengine/autonomous-engine/internal/workgraph"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// M3-07 against a real PostgreSQL (TEST_POSTGRES_DSN, migrated): the
// verifier's inputs are read from what the pipeline stores, and its verdict
// artifact commits in the same transaction as the outcome it causes.

type verifierFixture struct {
	parent, subtask uuid.UUID
}

func seedVerifierFixture(t *testing.T, pool *pgxpool.Pool, parentState string) verifierFixture {
	t.Helper()
	ctx := context.Background()
	f := verifierFixture{parent: uuid.New(), subtask: uuid.New()}
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	t.Cleanup(func() {
		// Each statement gets exactly the arguments it uses: pgx rejects
		// extra ones, which left these rows behind (and an unpublished
		// review.complete that a later outbox test's dispatcher delivered).
		for _, q := range []struct {
			sql  string
			args []any
		}{
			{`DELETE FROM outbox_events WHERE aggregate_id = $1`, []any{f.parent}},
			{`DELETE FROM agent_assignments WHERE work_item_id = $1 OR work_item_id = $2`, []any{f.parent, f.subtask}},
			{`DELETE FROM plan_tasks WHERE parent_work_id = $1`, []any{f.parent}},
			{`DELETE FROM work_items WHERE id = $1`, []any{f.subtask}},
			{`DELETE FROM work_items WHERE id = $1`, []any{f.parent}},
		} {
			if _, err := pool.Exec(context.Background(), q.sql, q.args...); err != nil {
				t.Logf("cleanup %q: %v", q.sql, err)
			}
		}
	})
	exec(`INSERT INTO work_items (id, type, state, title, description) VALUES ($1, 'feature', $2, 'M3-07 verifier item', 'AUTHOR-DESCRIPTION-MARKER')`, f.parent, parentState)
	exec(`INSERT INTO work_items (id, type, state, title, description, parent_id) VALUES ($1, 'subtask', 'passed', 'M3-07 subtask', 'SUBTASK-PROSE-MARKER', $2)`, f.subtask, f.parent)
	exec(`INSERT INTO work_item_artifacts (id, work_item_id, artifact_type, content, agent_id) VALUES ($1, $2, 'technical_brief', $3, 'research-coordinator')`,
		uuid.New(), f.parent, `{"brief":"BRIEF-PROSE-MARKER","criteria":{"schema_version":1,"criteria":[
			{"id":"AC-1","text":"Signed-out visitors see anonymous","source":{"kind":"user","quote":"visitors see anonymous"},"risk":"high"}]}}`)
	exec(`INSERT INTO work_item_artifacts (id, work_item_id, artifact_type, content, agent_id) VALUES ($1, $2, 'research_finding', $3, 'implementation-agent')`,
		uuid.New(), f.parent, `{"type":"implementation","summary":"IMPL-SUMMARY-MARKER every criterion is satisfied","criterion_ids":"AC-1"}`)
	exec(`INSERT INTO plan_tasks (id, parent_work_id, title, status, message) VALUES ($1, $2, 'M3-07 subtask', 'completed', $3)`,
		f.subtask, f.parent, `{"description":"SUBTASK-PROSE-MARKER","technical_brief":"BRIEF-PROSE-MARKER",
			"task_spec":{"schema_version":1,"criterion_ids":["AC-1","AC-2"],"deliverable":"DisplayName handles nil","verification":[{"check_kind":"test","command":"go test ./..."}]},
			"criteria":[{"id":"AC-2","text":"DisplayName never panics","source":{"kind":"derived","reason":"robustness"},"risk":"medium"}]}`)
	exec(`INSERT INTO implementation_verifications (work_item_id, task_id, schema_version, base_sha, head_sha, branch, outcome, evidence)
		VALUES ($1, $2, 1, 'aaaa', 'bbbb', 'agent/x', 'verified', $3)`, f.parent, f.subtask.String(),
		`{"schema_version":1,"checks":[{"kind":"test","command":"go test ./...","exit_code":0,"status":"passed"}],"outcome":"verified","changed_files":["user.go"],"reason":"RECORD-REASON"}`)
	var runID uuid.UUID
	if err := pool.QueryRow(ctx, `INSERT INTO implementation_runs (work_item_id, task_id, attempt, state) VALUES ($1, $2, 1, 'final_verified') RETURNING id`, f.parent, f.subtask.String()).Scan(&runID); err != nil {
		t.Fatal(err)
	}
	exec(`INSERT INTO implementation_run_events (run_id, seq, kind, tool, command, exit_code) VALUES ($1, 1, 'tool', 'run_command', 'go test ./...', 0)`, runID)
	exec(`INSERT INTO implementation_run_events (run_id, seq, kind, from_state, to_state, note) VALUES ($1, 2, 'transition', 'implement', 'green_observed', 'test command exited 0')`, runID)
	exec(`INSERT INTO implementation_run_events (run_id, seq, kind, note) VALUES ($1, 3, 'note', 'REPAIR-NOTE-MARKER the author thinks this is fine')`, runID)
	return f
}

func TestIntegration_Verifier_LoadsStoredInputsWithoutAuthorProse(t *testing.T) {
	pool := integrationPool(t)
	f := seedVerifierFixture(t, pool, "implementation")
	reqs, ev, err := verifier.Load(context.Background(), pool, f.parent)
	if err != nil {
		t.Fatal(err)
	}
	if len(reqs.Criteria) != 2 || reqs.Criteria[0].ID != "AC-2" || reqs.Criteria[1].ID != "AC-1" || len(reqs.TaskSpecs) != 1 {
		t.Fatalf("requirements = %+v", reqs)
	}
	if len(ev.Records) != 1 || ev.Records[0].Outcome != "verified" || len(ev.Records[0].Checks) != 1 {
		t.Fatalf("records = %+v", ev.Records)
	}
	if len(ev.Runs) != 1 || ev.Runs[0].NoteEvents != 1 || len(ev.Runs[0].Commands) != 1 || len(ev.Runs[0].Transitions) != 1 {
		t.Fatalf("runs = %+v", ev.Runs)
	}
	prompt := verifier.RenderPrompt(verifier.Build(verifier.Sources{WorkID: f.parent, Requirements: reqs, Evidence: ev, RepoTree: "/x"}))
	for _, m := range []string{"AUTHOR-DESCRIPTION-MARKER", "SUBTASK-PROSE-MARKER", "BRIEF-PROSE-MARKER", "IMPL-SUMMARY-MARKER", "REPAIR-NOTE-MARKER"} {
		if strings.Contains(prompt, m) {
			t.Errorf("prompt contains %s", m)
		}
	}
}

// No clone: the guarded handler escalates; the verdict artifact (every
// criterion not_verifiable) and the human_review transition are committed
// together, and nothing is blocked.
func TestIntegration_Verifier_NoCloneEscalatesToHumanReview(t *testing.T) {
	pool := integrationPool(t)
	ctx := context.Background()
	f := seedVerifierFixture(t, pool, "implementation")
	eventID := uuid.New()
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM processed_events WHERE event_id = $1`, eventID)
	})
	b, _ := json.Marshal(map[string]any{"work_id": f.parent.String(), "pr_url": "https://example.invalid/pr/7", "pr_number": 7, "branch": "agent/x",
		"pr_description": "PR-DESCRIPTION-MARKER", "head_sha": "ABCDEF0123456789abcdef0123456789abcdef01"})
	msg := &bus.Message{Topic: bus.TopicPRReady, Key: f.parent.String(), Value: b, Headers: map[string]string{bus.HeaderEventID: eventID.String()}}
	h := idempotency.NewLedger(pool).Handler("review-agent",
		buildHandler(ctx, pool, workgraph.New(pool), agentregistry.New(pool), nil, nopBus{}, true, "http://github-service.invalid", nil, evidence.CompatWindowLegacy))
	if err := h(ctx, msg); err != nil {
		t.Fatal(err)
	}

	var state string
	if err := pool.QueryRow(ctx, `SELECT state FROM work_items WHERE id = $1`, f.parent).Scan(&state); err != nil || state != "human_review" {
		t.Fatalf("state = %q, %v; want human_review", state, err)
	}
	var reason string
	if err := pool.QueryRow(ctx, `SELECT reason FROM work_item_transitions WHERE work_item_id = $1 AND to_state = 'human_review'`, f.parent).Scan(&reason); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reason, "independent verifier escalated") || !strings.Contains(reason, "AC-") {
		t.Errorf("reason = %q", reason)
	}
	var raw []byte
	if err := pool.QueryRow(ctx, `SELECT content FROM work_item_artifacts WHERE work_item_id = $1 AND artifact_type = 'review_comments'`, f.parent).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var c verifierArtifactContent
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatal(err)
	}
	if c.Kind != "verifier" || c.Decision != "escalate" || len(c.Verdict.PerCriterion) != 2 || c.HeadSHA != "abcdef0123456789abcdef0123456789abcdef01" || c.ReviewedSHA != "" {
		t.Fatalf("artifact = %+v", c)
	}
	for _, r := range c.Verdict.PerCriterion {
		if r.Status != verifier.StatusNotVerifiable || !strings.Contains(r.Note, "repository not available") {
			t.Errorf("%s = %s (%s)", r.ID, r.Status, r.Note)
		}
	}
	if strings.Contains(string(raw), "PR-DESCRIPTION-MARKER") {
		t.Error("the verdict carries the PR description")
	}
	var blocked int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM outbox_events WHERE aggregate_id = $1 AND topic = 'work.blocked'`, f.parent).Scan(&blocked)
	if blocked != 0 {
		t.Errorf("work.blocked = %d", blocked)
	}
}

// request_changes: the verdict artifact and review.complete
// (changes_requested, feedback = the structured defects) commit in one
// transaction.
func TestIntegration_Verifier_RequestChangesCommitsArtifactWithEvent(t *testing.T) {
	pool := integrationPool(t)
	ctx := context.Background()
	f := seedVerifierFixture(t, pool, "review")
	wg := workgraph.New(pool)
	asg := &models.AgentAssignment{WorkItemID: f.parent, AgentType: models.AgentTypeReview, AgentInstanceID: agentID, Status: models.AssignmentStatusActive}
	if err := wg.AssignAgent(ctx, asg); err != nil {
		t.Fatal(err)
	}
	v, err := verifier.ParseVerdict(verdictRequestChanges, []string{"AC-1", "AC-2"}, verifier.Grounding{})
	if err != nil {
		t.Fatal(err)
	}
	d := verifier.Gate(v)
	p := planAfterVerifier(v, d, true, true)
	rw := reviewWriter{db: pool, wg: wg}
	if !rw.recordReview(ctx, f.parent, asg.ID, verifierArtifact(f.parent, v, d, p, "agent/x", "", ""), *p.decision) {
		t.Fatal("not recorded")
	}
	var payload []byte
	if err := pool.QueryRow(ctx, `SELECT payload FROM outbox_events WHERE aggregate_id = $1 AND topic = 'review.complete'`, f.parent).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	var m reviewCompleteMessage
	if err := json.Unmarshal(payload, &m); err != nil {
		t.Fatal(err)
	}
	if m.Decision != "changes_requested" || !strings.Contains(m.Feedback, "[critical] user.go:14") {
		t.Fatalf("review.complete = %+v", m)
	}
	var n int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM work_item_artifacts WHERE work_item_id = $1 AND content->>'kind' = 'verifier' AND content->'defects'->>'critical' = '1'`, f.parent).Scan(&n)
	if n != 1 {
		t.Fatalf("verifier artifacts with defect counts = %d", n)
	}
}

// Atomicity: when the transition cannot commit, neither does the verdict.
func TestIntegration_Verifier_ArtifactRollsBackWithTransition(t *testing.T) {
	pool := integrationPool(t)
	ctx := context.Background()
	f := seedVerifierFixture(t, pool, "intake") // intake → human_review is not a valid transition
	wg := workgraph.New(pool)
	asg := &models.AgentAssignment{WorkItemID: f.parent, AgentType: models.AgentTypeReview, AgentInstanceID: agentID, Status: models.AssignmentStatusActive}
	if err := wg.AssignAgent(ctx, asg); err != nil {
		t.Fatal(err)
	}
	v := verifier.Escalation([]string{"AC-1"}, "not verifiable: test", verifier.ModelInfo{})
	d := verifier.Gate(v)
	rw := reviewWriter{db: pool, wg: wg}
	if err := rw.commitReview(ctx, f.parent, asg.ID, verifierArtifact(f.parent, v, d, planAfterVerifier(v, d, true, true), "agent/x", "", ""), reviewDecision{escalate: true, reason: "x"}); err == nil {
		t.Fatal("commit succeeded on an invalid transition")
	}
	var n int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM work_item_artifacts WHERE work_item_id = $1 AND artifact_type = 'review_comments'`, f.parent).Scan(&n)
	if n != 0 {
		t.Fatalf("verdict artifact committed without its transition (%d rows)", n)
	}
}
