package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/evidence"
	"github.com/autonomousengine/autonomous-engine/internal/plan"
	"github.com/autonomousengine/autonomous-engine/internal/verifier"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// R8: a live run (attempt 8) blocked in planning on
// orphan_criterion AC-6 ("The \"no sign-up required\" claim in `landing/index.html`
// is corrected."), which the model left out of both the decomposition and
// the repair round although tasks 3 and 4 both change landing/index.html.
// The fixture is the live brief's criteria and the last decompose_tasks
// response, served twice (the repair round repeats the omission).

func attempt8Fixture(t *testing.T) (structuredBrief, string) {
	t.Helper()
	dir := filepath.Join("testdata", "attempt8")
	briefData, err := os.ReadFile(filepath.Join(dir, "brief.json"))
	if err != nil {
		t.Fatal(err)
	}
	var brief structuredBrief
	if err := json.Unmarshal(briefData, &brief); err != nil {
		t.Fatal(err)
	}
	if brief.Criteria == nil || len(brief.Criteria.Criteria) != 9 {
		t.Fatalf("fixture brief must carry AC-1..AC-9: %+v", brief.Criteria)
	}
	dec, err := os.ReadFile(filepath.Join(dir, "decomposition.json"))
	if err != nil {
		t.Fatal(err)
	}
	return brief, string(dec)
}

// attempt8Plan runs the attempt-8 decomposition through planWithRepair with
// the fake LLM answering both rounds with the same plan.
func attempt8Plan(t *testing.T) (planOutcome, *fakeLLM) {
	t.Helper()
	brief, dec := attempt8Fixture(t)
	svc, fake := fakeLLMService(t, dec, dec)
	out := planWithRepair(uuid.Nil,
		func(repair string) ([]decomposedTask, error) {
			return decomposeIntoTasks(context.Background(), svc, uuid.Nil, brief, nil, "", repair, nil)
		},
		capturedBuilder(brief.Criteria),
	)
	if out.decomposeErr != nil {
		t.Fatalf("decomposition failed: %v", out.decomposeErr)
	}
	return out, fake
}

// O1 acceptance: the attempt-8 plan is not blocked; AC-6 is re-homed to
// task 3 (both 3 and 4 change landing/index.html with two files each: the
// tie goes to the lower index).
func TestAttempt8_OrphanReHomedByFileOverlap(t *testing.T) {
	out, fake := attempt8Plan(t)
	if out.rejected() {
		t.Fatalf("plan rejected: %s", plan.SummarizeViolations(plan.Errors(out.violations), 10))
	}
	// R9 P1: the fixture's only problem is the orphan, which the planner
	// re-homes before any re-prompt, so no repair round is spent on it.
	if fake.calls != 1 {
		t.Errorf("decomposition calls = %d, want 1 (no repair round for a re-homeable orphan)", fake.calls)
	}
	raw := out.build.raw
	if len(raw) != 7 {
		t.Fatalf("tasks = %d, want 7", len(raw))
	}
	if !containsString(raw[2].CriterionIDs, "AC-6") {
		t.Errorf("task 3 criterion_ids = %v, want AC-6 re-homed to it", raw[2].CriterionIDs)
	}
	if containsString(raw[3].CriterionIDs, "AC-6") {
		t.Errorf("task 4 criterion_ids = %v: AC-6 must be re-homed to one task only", raw[3].CriterionIDs)
	}
	for _, v := range out.violations {
		if v.Code == plan.CodeOrphanCriterion {
			t.Errorf("orphan violation left: %+v", v)
		}
	}
	if strings.Contains(strings.Join(fake.requests(), "\n"), "AC-6") == false {
		t.Errorf("the repair round never named AC-6")
	}
}

// O1: the re-homing is recorded on the plan (rehomed_criteria in every
// subtask's message), and AC-6 is an ordinary cited criterion of the subtask
// that owns task 3: the verifier's requirements, loaded from the stored
// plan_tasks message, carry it unmarked (judged on evidence, no exemption).
func TestAttempt8_RehomingIsRecordedAndVerified(t *testing.T) {
	out, _ := attempt8Plan(t)
	if out.rejected() {
		t.Fatalf("plan rejected: %+v", out.violations)
	}
	b := out.build
	want := []rehomedCriterion{{CriterionID: "AC-6", TaskIndex: 3, TaskTitle: "Update HTML Pages to Fetch API Key", Files: []string{"landing/index.html"}}}
	if got := mustJSON(t, b.rehomed); string(got) != string(mustJSON(t, want)) {
		t.Fatalf("rehomed = %s, want %s", got, mustJSON(t, want))
	}
	attachNonCodeCriteria(b)
	if len(b.planned) != 1 {
		t.Fatalf("subtasks = %d, want 1 (one repository)", len(b.planned))
	}
	msg := b.planned[0].msg
	if len(msg.NonCodeCriteria) != 0 {
		t.Errorf("non_code_criteria = %+v, want none: AC-6 was re-homed, not recorded as a gap", msg.NonCodeCriteria)
	}
	if !strings.Contains(msg.Description, "## Re-homed acceptance criteria") || !strings.Contains(msg.Description, "because that step changes landing/index.html") {
		t.Errorf("description lacks the re-homing note:\n%s", msg.Description)
	}
	raw := mustJSON(t, msg)
	if !strings.Contains(string(raw), `"rehomed_criteria":[{"criterion_id":"AC-6","task_index":3,`) {
		t.Errorf("plan_tasks.message lacks rehomed_criteria: %s", raw)
	}

	req, err := verifier.LoadRequirements(context.Background(), planTasksQuerier{messages: [][]byte{raw}}, uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	ac6, ok := findCriterion(req.Criteria, "AC-6")
	if !ok || ac6.NonCode || ac6.Unowned {
		t.Fatalf("verifier requirements AC-6 = %+v (found %t), want the criterion, unmarked (judged on evidence)", ac6, ok)
	}
	if len(req.TaskSpecs) != 1 || !containsString(req.TaskSpecs[0].Spec.CriterionIDs, "AC-6") {
		t.Fatalf("verifier task specs = %+v, want AC-6 cited by the subtask", req.TaskSpecs)
	}
}

func findCriterion(cs []plan.Criterion, id string) (plan.Criterion, bool) {
	for _, c := range cs {
		if c.ID == id {
			return c, true
		}
	}
	return plan.Criterion{}, false
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// planTasksQuerier is a verifier.Querier serving plan_tasks messages and no
// technical brief.
type planTasksQuerier struct{ messages [][]byte }

func (q planTasksQuerier) Query(_ context.Context, sql string, _ ...any) (pgx.Rows, error) {
	if !strings.Contains(sql, "FROM plan_tasks") {
		return nil, errors.New("unexpected query: " + sql)
	}
	return &messageRows{messages: q.messages, i: -1}, nil
}

func (q planTasksQuerier) QueryRow(context.Context, string, ...any) pgx.Row { return noRow{} }

type noRow struct{}

func (noRow) Scan(...any) error { return pgx.ErrNoRows }

type messageRows struct {
	messages [][]byte
	i        int
}

func (r *messageRows) Close()                                       {}
func (r *messageRows) Err() error                                   { return nil }
func (r *messageRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *messageRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *messageRows) Next() bool                                   { r.i++; return r.i < len(r.messages) }
func (r *messageRows) Values() ([]any, error)                       { return nil, nil }
func (r *messageRows) RawValues() [][]byte                          { return nil }
func (r *messageRows) Conn() *pgx.Conn                              { return nil }
func (r *messageRows) Scan(dest ...any) error {
	*dest[0].(*string) = fmt.Sprintf("00000000-0000-0000-0000-%012d", r.i+1)
	*dest[1].(*[]byte) = r.messages[r.i]
	return nil
}

// ─── synthetic plans ─────────────────────────────────────────────────────────

// orphanBrief is a brief with the given criterion texts (AC-1, AC-2, …).
func orphanBrief(texts ...string) *plan.BriefCriteria {
	bc := &plan.BriefCriteria{SchemaVersion: plan.SchemaVersion}
	for i, text := range texts {
		bc.Criteria = append(bc.Criteria, plan.Criterion{
			ID: fmt.Sprintf("AC-%d", i+1), Text: text,
			Source: plan.Source{Kind: plan.SourceUser, Quote: text}, Risk: plan.RiskLow,
		})
	}
	return bc
}

// orphanStep is a step citing ids and changing files, with a test check.
func orphanStep(title string, ids []string, files ...string) decomposedTask {
	t := decomposedTask{Title: title, Description: title, CriterionIDs: ids, Deliverable: title + " done",
		Verification: []plan.VerificationStep{{CheckKind: evidence.CheckKindTest, Command: "npm test"}}}
	for _, f := range files {
		t.FileClaims = append(t.FileClaims, fileClaim{Path: f, EvidenceRef: "hypothesis"})
	}
	return t
}

func copySteps(steps []decomposedTask) []decomposedTask {
	out := make([]decomposedTask, len(steps))
	for i, s := range steps {
		s.CriterionIDs = append([]string(nil), s.CriterionIDs...)
		out[i] = s
	}
	return out
}

// planTwice runs planWithRepair with the model returning steps both times,
// and returns the repair prompt it was sent ("" when none).
func planTwice(t *testing.T, brief *plan.BriefCriteria, steps []decomposedTask) (planOutcome, string) {
	t.Helper()
	var repair string
	out := planWithRepair(uuid.Nil, func(r string) ([]decomposedTask, error) {
		if r != "" {
			repair = r
		}
		return copySteps(steps), nil
	}, capturedBuilder(brief))
	if out.decomposeErr != nil {
		t.Fatal(out.decomposeErr)
	}
	return out, repair
}

// O1: the candidate with the fewest files wins over a lower index.
func TestRehomeChoosesTheStepWithFewestFiles(t *testing.T) {
	brief := orphanBrief("Wide change", "Narrow change", "The banner in `web/app.js` says hello")
	out, _ := planTwice(t, brief, []decomposedTask{
		orphanStep("Wide", []string{"AC-1"}, "web/app.js", "web/index.html", "web/style.css"),
		orphanStep("Narrow", []string{"AC-2"}, "web/app.js"),
	})
	if out.rejected() {
		t.Fatalf("rejected: %+v", out.violations)
	}
	if !containsString(out.build.raw[1].CriterionIDs, "AC-3") || containsString(out.build.raw[0].CriterionIDs, "AC-3") {
		t.Fatalf("AC-3 went to %v / %v, want task 2 (one file) only", out.build.raw[0].CriterionIDs, out.build.raw[1].CriterionIDs)
	}
}

// O1: a re-homed criterion is held to its required checks on its new step:
// the plan is still rejected when that step lacks one.
func TestRehomedCriterionKeepsItsRequiredChecks(t *testing.T) {
	brief := orphanBrief("A", "B", "`src/a.ts` builds cleanly")
	brief.Criteria[2].RequiredChecks = []evidence.CheckKind{evidence.CheckKindBuild}
	out, _ := planTwice(t, brief, []decomposedTask{
		orphanStep("A", []string{"AC-1"}, "src/a.ts"),
		orphanStep("B", []string{"AC-2"}, "src/b.ts"),
	})
	if !out.rejected() {
		t.Fatal("plan accepted although the re-homed criterion's build check is missing")
	}
	errs := plan.Errors(out.violations)
	if len(errs) != 1 || errs[0].Code != plan.CodeVerifierMissing || errs[0].CriterionID != "AC-3" {
		t.Fatalf("errors = %+v, want verifier_missing for AC-3 on its new step", errs)
	}
	if len(out.build.rehomed) != 1 || out.build.rehomed[0].TaskIndex != 1 {
		t.Fatalf("rehomed = %+v", out.build.rehomed)
	}
}

func TestCriterionPaths(t *testing.T) {
	for _, tc := range []struct {
		text string
		want []string
	}{
		{"The \"no sign-up required\" claim in `landing/index.html` is corrected.", []string{"landing/index.html"}},
		{"Both `public/index.html` and `landing/index.html` fetch the `/api/client-config` endpoint and append `?api_key=<key>`.", []string{"public/index.html", "landing/index.html"}},
		{"Documentation (`README.md`, `.env.example`, `docker-compose.yml`, `docs/configuration.md`) is updated.", []string{"README.md", ".env.example", "docker-compose.yml", "docs/configuration.md"}},
		{"Update src/server/index.ts, then ./src/config/env.ts.", []string{"src/server/index.ts", "src/config/env.ts"}},
		{"See https://example.com/docs/setup.md and /etc/nginx/nginx.conf.", nil},
		{"`src/a.go:12-20` is fixed and docs\\setup.md mentions it", []string{"src/a.go", "docs/setup.md"}},
		{"The notice is hidden when the API key is configured (e.g. in process.env).", nil},
	} {
		got := criterionPaths(tc.text)
		if strings.Join(got, "|") != strings.Join(tc.want, "|") {
			t.Errorf("criterionPaths(%q) = %q, want %q", tc.text, got, tc.want)
		}
	}
}
