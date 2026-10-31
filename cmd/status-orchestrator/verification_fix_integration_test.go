package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/bus"
	"github.com/autonomousengine/autonomous-engine/internal/verifier"
	"github.com/autonomousengine/autonomous-engine/internal/verifier/stage"
	"github.com/autonomousengine/autonomous-engine/internal/workgraph"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// R16 against a real PostgreSQL: the verifier's request_changes dispatches
// a fix subtask carrying the verdict's findings (G3), and the fix rounds
// are bounded by the shared circuit breaker (G4).

// recordVerdict commits what the verification stage commits for sub with
// the verdict v (its gate decides the decision) and returns the
// verification.complete to deliver.
func recordVerdict(t *testing.T, pool *pgxpool.Pool, parent, sub uuid.UUID, v verifier.Verdict) *bus.Message {
	t.Helper()
	ctx := context.Background()
	head := strings.Repeat("e", 40)
	g := verifier.Gate(v)
	decision := stage.DecisionFor(g, true)
	o := &stage.Outcome{
		Message:  stage.Message{WorkID: parent.String(), TaskID: sub.String(), HeadSHA: head, Decision: decision, Reason: g.Reason},
		Result:   stage.Result{WorkID: parent, TaskID: sub, HeadSHA: head, Decision: decision, Reason: g.Reason, VerifierVersion: verifier.Version},
		Artifact: stage.NewArtifact(parent, "review-agent", stage.NewArtifactContent(v, g, string(decision), "agent/x", head, head, sub.String())),
	}
	if err := stage.Commit(ctx, pool, workgraph.New(pool), "review-agent", o); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c := context.Background()
		_, _ = pool.Exec(c, `DELETE FROM verification_results WHERE work_item_id = $1`, parent)
		_, _ = pool.Exec(c, `DELETE FROM work_item_artifacts WHERE work_item_id = $1`, parent)
	})
	msg := eventMessage(t, bus.TopicVerificationComplete, o.Message)
	cleanupLedgerRow(t, pool, msg)
	return msg
}

// seedPlanTask records sub's plan task the way the planner does: its
// criteria and its research line seeds (R10 W5).
func seedPlanTask(t *testing.T, pool *pgxpool.Pool, parent, sub uuid.UUID, message string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `INSERT INTO plan_tasks (id, parent_work_id, title, status, message) VALUES ($1, $2, 'R16 subtask', 'completed', $3)`,
		sub, parent, message); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM plan_tasks WHERE parent_work_id = $1`, parent)
	})
}

// fixDispatches are the implementation.tasks fix messages enqueued for the
// parent, oldest first.
func fixDispatches(t *testing.T, pool *pgxpool.Pool, parent uuid.UUID) []map[string]any {
	t.Helper()
	rows, err := pool.Query(context.Background(), `SELECT payload FROM outbox_events WHERE aggregate_id = $1 AND topic = 'implementation.tasks' ORDER BY created_at, id`, parent)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatal(err)
		}
		out = append(out, m)
	}
	return out
}

const r16PlanTask = `{
	"task_spec": {"schema_version":1,"criterion_ids":["AC-8"],"deliverable":"d","verification":[{"check_kind":"test","command":"npm test"}]},
	"criteria": [{"id":"AC-8","text":"Map tiles are requested with the Mapforge API key from MAPFORGE_API_KEY","source":{"kind":"derived","reason":"r"},"risk":"low"}],
	"non_code_criteria": [{"id":"AC-11","text":"State in the PR description that the owner must set MAPFORGE_API_KEY.","source":{"kind":"derived","reason":"r"},"risk":"low","non_code":true,"non_code_reason":"satisfied by the PR text"}],
	"line_seeds": [
		{"path":"index.html","line_start":836,"line_end":853,"snippet":"L.tileLayer('https://tiles.mapforgecdn.example/{z}/{x}/{y}.png')","verified":true},
		{"path":"src/unrelated.js","line_start":3,"line_end":3,"snippet":"x","verified":true}
	]}`

const r16GapLine = "criterion AC-11 is non-code: satisfied by the PR text; verify manually"

// r16Verdict is the live-run shape: AC-8 not met at index.html:840 with a
// major defect there, and the AC-11 PR-text gap. marker tells rounds apart.
func r16Verdict(marker string) verifier.Verdict {
	return verifier.Verdict{
		SchemaVersion: verifier.SchemaVersion,
		PerCriterion: []verifier.CriterionResult{
			{ID: "AC-8", Status: verifier.StatusNotMet, Evidence: []verifier.Ref{"index.html:840"}, Note: marker + " the tile layer still requests the keyless endpoint"},
			{ID: "AC-11", Status: verifier.StatusNotVerifiable, Evidence: []verifier.Ref{}, Note: r16GapLine},
		},
		Defects: []verifier.Defect{{Severity: verifier.SeverityMajor, File: "index.html", Line: 840, Category: "logic", Description: marker + " the URL template ignores MAPFORGE_API_KEY"}},
		Overall: verifier.OverallRequestChanges,
		Gaps:    []string{r16GapLine},
	}
}

// latestFix is the newest fix subtask of parent.
func latestFix(t *testing.T, pool *pgxpool.Pool, parent uuid.UUID) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := pool.QueryRow(context.Background(), `SELECT id FROM work_items WHERE parent_id = $1 AND title = $2 ORDER BY created_at DESC LIMIT 1`, parent, fixSubtaskTitle).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// implemented delivers the fix subtask's implementation.complete.
func implemented(t *testing.T, pool *pgxpool.Pool, h bus.Handler, parent, fix uuid.UUID) {
	t.Helper()
	done := eventMessage(t, bus.TopicImplementationComplete, map[string]any{"task_id": fix.String(), "work_id": parent.String(), "success": true})
	cleanupLedgerRow(t, pool, done)
	replay(t, h, done, 1)
}

// G4: the verifier keeps failing. Each fix round is re-verified (the parent
// goes implementation → verification again, never to review); after the
// default 3 fix rounds the breaker escalates to human_review with every
// round's defects in the prompt, and no fourth fix is dispatched.
func TestIntegration_Verification_BreakerEscalatesWithTheDefectHistory(t *testing.T) {
	t.Setenv(MaxChangeCyclesEnv, "")
	pool := integrationPool(t)
	parent, sub := seedVerifyingParent(t, pool)
	seedPlanTask(t, pool, parent, sub, r16PlanTask)
	h := guardedOrchestrator(t, pool)

	// Round 0: the subtask's verdict → fix round 1.
	replay(t, h, recordVerdict(t, pool, parent, sub, r16Verdict("ROUND-1")), 2)
	if s := itemState(t, pool, parent); s != "implementation" {
		t.Fatalf("after the first verdict parent = %s, want implementation", s)
	}
	fix1 := latestFix(t, pool, parent)
	implemented(t, pool, h, parent, fix1)
	if s := itemState(t, pool, parent); s != "verification" {
		t.Fatalf("after fix 1 parent = %s, want verification (re-verified, not the gate)", s)
	}

	// Fix 1 fails verification → fix round 2.
	replay(t, h, recordVerdict(t, pool, parent, fix1, r16Verdict("ROUND-2")), 2)
	if s := itemState(t, pool, parent); s != "implementation" {
		t.Fatalf("after fix 1's verdict parent = %s, want implementation (fix round 2)", s)
	}
	fix2 := latestFix(t, pool, parent)
	if fix2 == fix1 {
		t.Fatal("no second fix subtask")
	}
	implemented(t, pool, h, parent, fix2)
	if s := itemState(t, pool, parent); s != "verification" {
		t.Fatalf("after fix 2 parent = %s, want verification", s)
	}

	// Fix 2 fails verification → fix round 3.
	replay(t, h, recordVerdict(t, pool, parent, fix2, r16Verdict("ROUND-3")), 2)
	if s := itemState(t, pool, parent); s != "implementation" {
		t.Fatalf("after fix 2's verdict parent = %s, want implementation (fix round 3)", s)
	}
	fix3 := latestFix(t, pool, parent)
	if fix3 == fix2 {
		t.Fatal("no third fix subtask")
	}
	implemented(t, pool, h, parent, fix3)
	if s := itemState(t, pool, parent); s != "verification" {
		t.Fatalf("after fix 3 parent = %s, want verification", s)
	}

	// Fix 3 fails too → the breaker trips.
	replay(t, h, recordVerdict(t, pool, parent, fix3, r16Verdict("ROUND-4")), 2)
	if s := itemState(t, pool, parent); s != "human_review" {
		t.Fatalf("after fix 3's verdict parent = %s, want human_review (breaker)", s)
	}
	if n := len(fixDispatches(t, pool, parent)); n != 3 {
		t.Errorf("fix dispatches = %d, want 3 (DefaultMaxChangeCycles)", n)
	}
	if n := transitionsTo(t, pool, parent, "verification", "implementation"); n != 3 {
		t.Errorf("verification→implementation = %d, want 3", n)
	}
	if n := transitionsTo(t, pool, parent, "implementation", "verification"); n != 4 {
		t.Errorf("implementation→verification = %d, want 4 (every round re-verified)", n)
	}
	if n := count(t, pool, `SELECT count(*) FROM work_item_transitions WHERE work_item_id = $1 AND to_state = 'review'`, parent); n != 0 {
		t.Errorf("transitions to review = %d, want 0", n)
	}
	if r := latestReason(t, pool, parent, "human_review"); !strings.Contains(r, "fix-round limit reached after verification (3 cycles)") {
		t.Errorf("human_review reason = %q", r)
	}
	var question string
	if err := pool.QueryRow(context.Background(), `SELECT question FROM user_interactions WHERE work_item_id = $1 AND trigger = 'approval:human_review'`, parent).Scan(&question); err != nil {
		t.Fatalf("human-review prompt: %v", err)
	}
	for _, want := range []string{"Fix-round limit reached: 3 automatic fix round(s) (limit 3", "Round 1 (fix subtask " + fix1.String()[:8], "Round 2 (fix subtask " + fix2.String()[:8], "Round 3 (fix subtask " + fix3.String()[:8], "Latest request (not dispatched)"} {
		if !strings.Contains(question, want) {
			t.Errorf("prompt lacks %q:\n%s", want, question)
		}
	}
	idx := []int{strings.Index(question, "ROUND-1"), strings.Index(question, "ROUND-2"), strings.Index(question, "ROUND-3"), strings.Index(question, "ROUND-4")}
	for i, at := range idx {
		if at < 0 || (i > 0 && at <= idx[i-1]) {
			t.Errorf("prompt does not carry every round's defects oldest first (ROUND-1..4 at %v):\n%s", idx, question)
			break
		}
	}
}

// G1 + G3: not_met + a recorded gap is a fix round (no human yet), and the
// fix subtask's message carries the criterion id and text, the defect, the
// file:line references and the original subtask's line seed for that file.
func TestIntegration_Verification_FixSubtaskCarriesTheVerdictsFindings(t *testing.T) {
	pool := integrationPool(t)
	parent, sub := seedVerifyingParent(t, pool)
	seedPlanTask(t, pool, parent, sub, r16PlanTask)
	replay(t, guardedOrchestrator(t, pool), recordVerdict(t, pool, parent, sub, r16Verdict("ROUND-1")), 3)

	if s := itemState(t, pool, parent); s != "implementation" {
		t.Fatalf("parent = %s, want implementation (a fix round, not human review)", s)
	}
	fixes := fixDispatches(t, pool, parent)
	if len(fixes) != 1 {
		t.Fatalf("fix dispatches = %d, want 1", len(fixes))
	}
	fb, _ := fixes[0]["review_feedback"].(string)
	desc, _ := fixes[0]["description"].(string)
	if fb != desc {
		t.Errorf("description and review_feedback differ")
	}
	for _, want := range []string{
		"INDEPENDENT VERIFIER REQUESTED CHANGES",
		`AC-8 not met (subtask ` + sub.String()[:8] + `): "Map tiles are requested with the Mapforge API key from MAPFORGE_API_KEY"`,
		"ROUND-1 the tile layer still requests the keyless endpoint",
		"At: index.html:840",
		"[major] index.html:840 — logic: ROUND-1 the URL template ignores MAPFORGE_API_KEY",
		r16GapLine,
		`Where to edit: the task's "## Where to edit"`,
	} {
		if !strings.Contains(fb, want) {
			t.Errorf("fix feedback lacks %q:\n%s", want, fb)
		}
	}
	// The seeds render once, from line_seeds: no copy in the feedback.
	for _, lack := range []string{"src/unrelated.js", "index.html:836", "L.tileLayer"} {
		if strings.Contains(fb, lack) {
			t.Errorf("fix feedback carries %q:\n%s", lack, fb)
		}
	}
	if len(fb) > verifier.FixBriefMaxChars {
		t.Errorf("fix feedback is %d bytes, over %d", len(fb), verifier.FixBriefMaxChars)
	}
	seeds, _ := fixes[0]["line_seeds"].([]any)
	if len(seeds) != 1 {
		t.Fatalf("fix message line_seeds = %v, want the index.html seed only", fixes[0]["line_seeds"])
	}
	if s0, _ := seeds[0].(map[string]any); s0["path"] != "index.html" || s0["line_start"] != float64(836) || s0["verified"] != true {
		t.Errorf("line seed = %v", seeds[0])
	}
	if fixes[0]["is_review_fix"] != true {
		t.Errorf("fix message is_review_fix = %v", fixes[0]["is_review_fix"])
	}
	if n := count(t, pool, `SELECT count(*) FROM user_interactions WHERE work_item_id = $1 AND trigger = 'approval:human_review'`, parent); n != 0 {
		t.Errorf("human-review prompts = %d, want 0 before the fix round", n)
	}
}
