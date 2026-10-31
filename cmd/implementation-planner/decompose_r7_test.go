package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/bus"
	"github.com/autonomousengine/autonomous-engine/internal/config"
	"github.com/autonomousengine/autonomous-engine/internal/llm"
	"github.com/autonomousengine/autonomous-engine/internal/models"
	"github.com/autonomousengine/autonomous-engine/internal/plan"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// R7: a live run (attempt 7) blocked in planning because the
// model redeclared brief criterion AC-9 under task 4's derived_criteria.
// These tests drive decomposeIntoTasks through a real llm.Service against a
// local fake OpenAI-compatible server (no live model) that answers every
// call with a canned decomposition.

// fakeLLM serves canned chat completions, one per call in order (the last
// one repeats), and records every request body.
type fakeLLM struct {
	mu        sync.Mutex
	responses []string
	calls     int
	bodies    []string
}

func (f *fakeLLM) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var req struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	_ = json.Unmarshal(body, &req)
	f.mu.Lock()
	f.bodies = append(f.bodies, string(body))
	// A strategy's self-review pass replays the answer as an assistant
	// message: echo it unchanged, so one canned response is one Execute.
	// Anything that is not a decomposition prompt (a router health
	// check) gets a fixed answer and consumes no canned response.
	content, decompose := "", false
	for _, m := range req.Messages {
		if m.Role == "assistant" {
			content = m.Content
		}
		if strings.Contains(m.Content, "derived_criteria") {
			decompose = true
		}
	}
	if !decompose {
		content = "ok"
	}
	if content == "" {
		i := f.calls
		if i >= len(f.responses) {
			i = len(f.responses) - 1
		}
		f.calls++
		content = f.responses[i]
	}
	f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"id":    "chatcmpl-r7",
		"model": "fake",
		"choices": []map[string]any{{
			"message":       map[string]any{"role": "assistant", "content": content},
			"finish_reason": "stop",
		}},
		"usage": map[string]any{"prompt_tokens": 1, "completion_tokens": 1},
	})
}

func (f *fakeLLM) requests() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.bodies...)
}

// fakeLLMService is an llm.Service whose provider is the fake server.
func fakeLLMService(t *testing.T, responses ...string) (*llm.Service, *fakeLLM) {
	t.Helper()
	f := &fakeLLM{responses: responses}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	svc, err := llm.NewService(&config.ModelConfig{Provider: "openai", Endpoint: srv.URL, APIKey: "test", Name: "fake"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return svc, f
}

func attempt7Fixture(t *testing.T) (structuredBrief, string) {
	t.Helper()
	dir := filepath.Join("testdata", "attempt7")
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
	if err := brief.Criteria.Validate(); err != nil {
		t.Fatalf("fixture brief criteria invalid: %v", err)
	}
	dec, err := os.ReadFile(filepath.Join(dir, "decomposition.json"))
	if err != nil {
		t.Fatal(err)
	}
	return brief, string(dec)
}

// The attempt-7 decomposition (task 4 redeclares AC-9, same text) plans: the
// redeclaration becomes a citation, the plan validates, nothing blocks.
func TestAttempt7_RedeclaredBriefCriterionPlansWithoutBlocking(t *testing.T) {
	brief, dec := attempt7Fixture(t)
	svc, fake := fakeLLMService(t, dec)

	out := planWithRepair(uuid.Nil,
		func(repair string) ([]decomposedTask, error) {
			return decomposeIntoTasks(context.Background(), svc, uuid.Nil, brief, nil, "", repair, nil)
		},
		capturedBuilder(brief.Criteria),
	)
	if out.decomposeErr != nil {
		t.Fatalf("decomposition blocked: %v", out.decomposeErr)
	}
	if out.rejected() {
		t.Fatalf("plan rejected: %+v", out.violations)
	}
	raw := out.build.raw
	if len(raw) != 4 {
		t.Fatalf("tasks = %d, want 4", len(raw))
	}
	task4 := raw[3]
	if !containsString(task4.CriterionIDs, "AC-9") {
		t.Errorf("task 4 criterion_ids = %v, want AC-9 cited", task4.CriterionIDs)
	}
	if len(task4.DerivedCriteria) != 0 {
		t.Errorf("task 4 derived_criteria = %+v, want the duplicate dropped", task4.DerivedCriteria)
	}
	// One LLM round: no re-prompt was needed.
	if fake.calls != 1 {
		t.Errorf("decomposition calls = %d, want 1", fake.calls)
	}
	for _, body := range fake.requests() {
		if strings.Contains(body, "previous response was rejected") {
			t.Errorf("a re-prompt was sent although normalisation fixes the output")
		}
	}
}

func containsString(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

// ─── D1: a derived criterion that reuses a brief id ─────────────────────────

func noticeTask(derived ...derivedCriterion) decomposedTask {
	return decomposedTask{Title: "Display notice", CriterionIDs: []string{"AC-1"}, DerivedCriteria: derived}
}

// Same (or near-same) text as the brief criterion: a citation, nothing else.
func TestNormaliseDerivedCriteria_SameTextBecomesCitation(t *testing.T) {
	brief := testBriefCriteria()
	tasks := []decomposedTask{noticeTask(derivedCriterion{
		ID: "AC-2", Text: "Existing submissions still work.", Reason: "r", Risk: plan.RiskLow,
	})}
	notes := normaliseDerivedCriteria(tasks, brief, nil)
	if got := strings.Join(tasks[0].CriterionIDs, ","); got != "AC-1,AC-2" {
		t.Errorf("criterion_ids = %s, want AC-1,AC-2", got)
	}
	if len(tasks[0].DerivedCriteria) != 0 {
		t.Errorf("derived = %+v, want the duplicate dropped", tasks[0].DerivedCriteria)
	}
	if len(notes) != 1 || !strings.Contains(notes[0], "AC-2") || !strings.Contains(notes[0], "cited") {
		t.Errorf("notes = %q, want one line naming the citation", notes)
	}
	if err := validateDecomposedTasks(tasks, brief, nil); err != nil {
		t.Errorf("normalised tasks rejected: %v", err)
	}
}

// An id already cited is not cited twice.
func TestNormaliseDerivedCriteria_CitationIsDeduplicated(t *testing.T) {
	brief := testBriefCriteria()
	tasks := []decomposedTask{noticeTask(derivedCriterion{ID: "AC-1", Text: brief.Criteria[0].Text, Reason: "r", Risk: plan.RiskLow})}
	normaliseDerivedCriteria(tasks, brief, nil)
	if got := strings.Join(tasks[0].CriterionIDs, ","); got != "AC-1" {
		t.Errorf("criterion_ids = %s, want AC-1 once", got)
	}
}

// Materially different text (Jaccard < 0.6): the brief criterion is cited
// AND the derived one is kept under a fresh id past every id in use, so its
// intent is not lost.
func TestNormaliseDerivedCriteria_DifferentTextIsRenumbered(t *testing.T) {
	brief := testBriefCriteria() // AC-1, AC-2
	other := decomposedTask{Title: "Other", CriterionIDs: []string{"AC-1"}, DerivedCriteria: []derivedCriterion{
		{ID: "AC-3", Text: "The form keeps its layout on mobile", Reason: "r", Risk: plan.RiskLow},
	}}
	tasks := []decomposedTask{
		noticeTask(derivedCriterion{ID: "AC-2", Text: "A banner warns when the API key is missing", Reason: "users must see it", Risk: plan.RiskMedium}),
		other,
	}
	notes := normaliseDerivedCriteria(tasks, brief, nil)
	got := tasks[0]
	if len(got.DerivedCriteria) != 1 || got.DerivedCriteria[0].ID != "AC-4" {
		t.Fatalf("derived = %+v, want one criterion renumbered AC-4", got.DerivedCriteria)
	}
	d := got.DerivedCriteria[0]
	if d.Text != "A banner warns when the API key is missing" || d.Reason != "users must see it" || d.Risk != plan.RiskMedium {
		t.Errorf("renumbered criterion lost its content: %+v", d)
	}
	if ids := strings.Join(got.CriterionIDs, ","); ids != "AC-1,AC-2,AC-4" {
		t.Errorf("criterion_ids = %s, want AC-1,AC-2,AC-4 (the brief id cited, the renumbered one owned)", ids)
	}
	if len(notes) != 1 || !strings.Contains(notes[0], "AC-4") {
		t.Errorf("notes = %q, want one line naming the new id", notes)
	}
	if err := validateDecomposedTasks(tasks, brief, nil); err != nil {
		t.Errorf("normalised tasks rejected: %v", err)
	}
}

// ─── D2: the re-prompt names the exact fix ──────────────────────────────────

// A brief id redeclared (normalisation skipped, so the strict pass sees it):
// the nudge quotes the offending task and says what to change.
func TestRetryNudge_BriefIDRedeclaredNamesTheFix(t *testing.T) {
	brief, dec := attempt7Fixture(t)
	tasks := mustTasks(t, []byte(dec))
	vErr := validateDecomposedTasks(tasks, brief.Criteria, nil)
	if vErr == nil {
		t.Fatal("the unnormalised attempt-7 output must be rejected by the strict pass")
	}
	got := decompositionRetryNudge(vErr, tasks)
	for _, want := range []string{
		`task 4 ("Display Notice for Missing API Key")`,
		"AC-9 is a brief criterion: list it under criterion_ids, do not redeclare it",
		`"title": "Display Notice for Missing API Key"`,
		"Respond with ONLY the corrected JSON array",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("nudge lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, `"title": "Report API Key Status in Config"`) {
		t.Errorf("nudge quotes tasks other than the offending one:\n%s", got)
	}
}

func TestRetryNudge_DuplicateDerivedIDNamesTheFreeID(t *testing.T) {
	brief := testBriefCriteria() // AC-1, AC-2
	tasks := []decomposedTask{
		noticeTask(derivedCriterion{ID: "AC-3", Text: "t one", Reason: "r", Risk: plan.RiskLow}),
		noticeTask(derivedCriterion{ID: "AC-3", Text: "t two", Reason: "r", Risk: plan.RiskLow}),
	}
	tasks[1].Title = "Second"
	got := decompositionRetryNudge(validateDecomposedTasks(tasks, brief, nil), tasks)
	for _, want := range []string{
		`task 2 ("Second")`,
		"AC-3 is already declared by task 1: give this derived criterion the next free id, AC-4",
		`"title": "Second"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("nudge lacks %q:\n%s", want, got)
		}
	}
}

func TestRetryNudge_EvidenceKindNamesTheAllowedValues(t *testing.T) {
	tasks := []decomposedTask{{Title: "t", CriterionIDs: []string{"AC-1"}, FileClaims: []fileClaim{{Path: "a.go", EvidenceRef: "grep:call_1"}}}}
	got := decompositionRetryNudge(validateDecomposedTasks(tasks, testBriefCriteria(), nil), tasks)
	if !strings.Contains(got, `set the evidence_ref of a.go to <codex|lsp|repo>:<ref> copied from the File Evidence table, "new" or "hypothesis"`) {
		t.Errorf("nudge does not name the fix:\n%s", got)
	}
}

// End to end: the re-prompt the model receives carries the fix.
func TestDecompose_RePromptCarriesTheFix(t *testing.T) {
	brief, dec := attempt7Fixture(t)
	bad := strings.Replace(dec, `"criterion_ids": ["AC-5", "AC-6"],`,
		`"criterion_ids": ["AC-5", "AC-6"], "derived_criteria": [{"id": "AC-10", "text": "x", "reason": "r", "risk": "low"}],`, 1)
	bad = strings.Replace(bad, `"id": "AC-9", "text": "Both`, `"id": "AC-10", "text": "Both`, 1)
	if bad == dec {
		t.Fatal("fixture rewrite failed")
	}
	svc, fake := fakeLLMService(t, bad, dec)
	if _, err := decomposeIntoTasks(context.Background(), svc, uuid.Nil, brief, nil, "", "", nil); err != nil {
		t.Fatalf("the corrected second answer must be accepted: %v", err)
	}
	var reprompt string
	for _, body := range fake.requests() {
		var req struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.Unmarshal([]byte(body), &req)
		for _, m := range req.Messages {
			if strings.Contains(m.Content, "previous response was rejected") {
				reprompt = m.Content
			}
		}
	}
	for _, want := range []string{
		`task 4 ("Display Notice for Missing API Key"): derived criterion id AC-10 is used twice`,
		"AC-10 is already declared by task 3: give this derived criterion the next free id, AC-11",
	} {
		if !strings.Contains(reprompt, want) {
			t.Errorf("re-prompt lacks %q:\n%s", want, reprompt)
		}
	}
}

// R7 review: the re-prompt quotes the task as the model sent it, before
// normalisation (here task 4 also redeclared AC-9, which normalisation
// removed from the working copy).
func TestDecompose_RePromptQuotesTheTaskAsSent(t *testing.T) {
	brief, dec := attempt7Fixture(t)
	bad := strings.Replace(dec, `"criterion_ids": ["AC-5", "AC-6"],`,
		`"criterion_ids": ["AC-5", "AC-6"], "derived_criteria": [{"id": "AC-10", "text": "x", "reason": "r", "risk": "low"}],`, 1)
	bad = strings.Replace(bad, `"reason": "the user asked for the notice on both pages", "risk": "medium"}`,
		`"reason": "the user asked for the notice on both pages", "risk": "medium"}, {"id": "AC-10", "text": "y", "reason": "r", "risk": "low"}`, 1)
	svc, fake := fakeLLMService(t, bad, dec)
	if _, err := decomposeIntoTasks(context.Background(), svc, uuid.Nil, brief, nil, "", "", nil); err != nil {
		t.Fatalf("the corrected second answer must be accepted: %v", err)
	}
	reprompt := ""
	for _, body := range fake.requests() {
		var req struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.Unmarshal([]byte(body), &req)
		for _, m := range req.Messages {
			if strings.Contains(m.Content, "previous response was rejected") {
				reprompt = m.Content
			}
		}
	}
	i := strings.Index(reprompt, "Task 4 as you sent it:")
	if i < 0 {
		t.Fatalf("re-prompt lacks the quote label:\n%s", reprompt)
	}
	if quote := reprompt[i:]; !strings.Contains(quote, `"id": "AC-9"`) || !strings.Contains(quote, `"id": "AC-10"`) {
		t.Errorf("the quote is not the task as sent (AC-9 redeclaration missing):\n%s", quote)
	}
}

// R7 review (Major): the brief's highest id was a derived criterion folded
// into a user one (AC-3 → AC-1). A renumbered derived criterion must not
// take AC-3 — foldPlanCriteria would rewrite its citation to AC-1 and leave
// it orphaned — so it becomes AC-4 and stays cited after folding.
func TestNormaliseDerivedCriteria_FreshIDSkipsFoldedBriefIDs(t *testing.T) {
	raw, _, _ := repairFixture(t) // AC-1 user, AC-2 user, AC-3 derived ≈ AC-1
	folded, briefFolds := foldBrief(uuid.Nil, raw)
	if len(briefFolds) != 1 || briefFolds[0].From != "AC-3" || briefFolds[0].Into != "AC-1" {
		t.Fatalf("fixture fold = %+v, want AC-3 into AC-1", briefFolds)
	}
	tasks := []decomposedTask{{
		Title: "Show the currency", CriterionIDs: []string{"AC-1"},
		DerivedCriteria: []derivedCriterion{{ID: "AC-2", Text: "The receipt shows the currency symbol next to every amount", Reason: "r", Risk: plan.RiskLow}},
	}}
	normaliseDerivedCriteria(tasks, folded, briefFolds)
	if len(tasks[0].DerivedCriteria) != 1 || tasks[0].DerivedCriteria[0].ID != "AC-4" {
		t.Fatalf("derived = %+v, want the renumbered criterion AC-4", tasks[0].DerivedCriteria)
	}
	if err := validateDecomposedTasks(tasks, folded, briefFolds); err != nil {
		t.Fatalf("normalised tasks rejected: %v", err)
	}
	numberSteps(tasks)
	criteria, _ := foldPlanCriteria(uuid.Nil, folded, briefFolds, tasks)
	if got := strings.Join(tasks[0].CriterionIDs, ","); got != "AC-1,AC-2,AC-4" {
		t.Errorf("criterion_ids after folding = %s, want AC-1,AC-2,AC-4", got)
	}
	var found bool
	for _, c := range criteria.Criteria {
		if c.ID == "AC-4" && c.Source.Kind == plan.SourceDerived {
			found = true
		}
	}
	if !found {
		t.Errorf("AC-4 is not a criterion of the folded plan: %+v", criteria.Criteria)
	}

	// The prompt numbers new derived criteria past the folded id as well.
	ctx := map[string]string{}
	criteriaPromptContext(ctx, folded, briefFolds)
	if !strings.Contains(ctx["acceptance_criteria"], "numbering from AC-4") {
		t.Errorf("prompt: %s", ctx["acceptance_criteria"])
	}
}

// A redeclared folded id is cited as the criterion it was folded into.
func TestNormaliseDerivedCriteria_FoldedIDCitesItsTarget(t *testing.T) {
	raw, _, _ := repairFixture(t)
	folded, briefFolds := foldBrief(uuid.Nil, raw)
	tasks := []decomposedTask{{
		Title: "Fix order", CriterionIDs: []string{"AC-2"},
		DerivedCriteria: []derivedCriterion{{ID: "AC-3", Text: "The calculator applies the fixed discount before the percentage discount", Reason: "r", Risk: plan.RiskLow}},
	}}
	normaliseDerivedCriteria(tasks, folded, briefFolds)
	if got := strings.Join(tasks[0].CriterionIDs, ","); got != "AC-2,AC-1" || len(tasks[0].DerivedCriteria) != 0 {
		t.Fatalf("criterion_ids = %s derived = %+v, want AC-2,AC-1 and none", got, tasks[0].DerivedCriteria)
	}
}

// ─── D3: an accurate block reason and an escalation prompt ──────────────────

// A decomposition the model gets wrong twice is a model-output failure: the
// reason says so, and says Retry re-runs planning, not "once the provider is
// healthy".
func TestDecompose_InvalidTwiceIsAModelOutputFailure(t *testing.T) {
	brief, dec := attempt7Fixture(t)
	bad := strings.Replace(dec, `"id": "AC-9", "text": "Both`, `"id": "D-9", "text": "Both`, 1)
	svc, fake := fakeLLMService(t, bad)
	_, err := decomposeIntoTasks(context.Background(), svc, uuid.Nil, brief, nil, "", "", nil)
	if err == nil {
		t.Fatal("an output invalid twice must fail")
	}
	if fake.calls != 2 {
		t.Errorf("decomposition calls = %d, want 2 (one correction)", fake.calls)
	}
	reason := decompositionBlockReason(err)
	for _, want := range []string{
		"Task decomposition failed: the model's task decomposition was invalid after one correction: ",
		`task 4 ("Display Notice for Missing API Key")`,
		"Retry re-runs planning.",
	} {
		if !strings.Contains(reason, want) {
			t.Errorf("reason lacks %q:\n%s", want, reason)
		}
	}
	if strings.Contains(reason, "provider is healthy") {
		t.Errorf("a model-output failure blames the provider:\n%s", reason)
	}
}

func TestDecompose_UnparseableTwiceIsAModelOutputFailure(t *testing.T) {
	brief, _ := attempt7Fixture(t)
	svc, _ := fakeLLMService(t, "I cannot produce a plan today.")
	_, err := decomposeIntoTasks(context.Background(), svc, uuid.Nil, brief, nil, "", "", nil)
	if err == nil {
		t.Fatal("unparseable output twice must fail")
	}
	if reason := decompositionBlockReason(err); !strings.Contains(reason, "the model's task decomposition was invalid after one correction") {
		t.Errorf("reason = %s", reason)
	}
}

func TestDecompose_ProviderFailureKeepsTheProviderReason(t *testing.T) {
	brief, _ := attempt7Fixture(t)
	_, err := decomposeIntoTasks(context.Background(), nil, uuid.Nil, brief, nil, "", "", nil)
	if err == nil {
		t.Fatal("no LLM must fail")
	}
	reason := decompositionBlockReason(err)
	if !strings.Contains(reason, "once the LLM provider is healthy") || strings.Contains(reason, "model's task decomposition was invalid") {
		t.Errorf("provider failure reason = %s", reason)
	}
}

// An empty brief is neither the model's fault nor the provider's.
func TestDecompose_EmptyBriefHasItsOwnReason(t *testing.T) {
	svc, fake := fakeLLMService(t, "[]")
	_, err := decomposeIntoTasks(context.Background(), svc, uuid.Nil, structuredBrief{}, nil, "", "", nil)
	if err == nil {
		t.Fatal("an empty brief must fail")
	}
	if fake.calls != 0 {
		t.Errorf("decomposition calls = %d, want 0", fake.calls)
	}
	reason := decompositionBlockReason(err)
	if !strings.Contains(reason, "Task decomposition failed: the technical brief has no prose to decompose") ||
		!strings.Contains(reason, "Retry re-runs planning against the current technical brief") {
		t.Errorf("reason = %s", reason)
	}
	if strings.Contains(reason, "provider is healthy") || strings.Contains(reason, "model's task decomposition") {
		t.Errorf("an empty brief blames the provider or the model: %s", reason)
	}
}

// decompTx is a pgx.Tx that records statements; QueryRow answers the
// transition's row lock with state.
type decompTx struct {
	pgx.Tx
	state     models.WorkItemState
	sqls      []string
	args      [][]any
	committed bool
}

func (f *decompTx) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	f.sqls, f.args = append(f.sqls, sql), append(f.args, args)
	return pgconn.CommandTag{}, nil
}

func (f *decompTx) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	f.sqls, f.args = append(f.sqls, sql), append(f.args, args)
	return decompRow{f}
}

func (f *decompTx) Commit(context.Context) error   { f.committed = true; return nil }
func (f *decompTx) Rollback(context.Context) error { return nil }

type decompRow struct{ f *decompTx }

func (r decompRow) Scan(dest ...any) error {
	*(dest[0].(*models.WorkItemState)) = r.f.state
	return nil
}

type decompDB struct{ tx *decompTx }

func (d decompDB) Begin(context.Context) (pgx.Tx, error) { return d.tx, nil }

// The decomposition block goes through the outbox like the plan-validation
// block: the item is blocked and work.blocked is enqueued in one commit, and
// status-orchestrator's work.blocked consumer (blockFromWorkBlocked) raises
// the escalation prompt row for an already-blocked item with none pending.
func TestBlockForDecompositionFailure_EnqueuesWorkBlocked(t *testing.T) {
	tx := &decompTx{state: models.WorkItemStatePlanning}
	workID := uuid.New()
	reason := decompositionBlockReason(&decompositionOutputError{err: errors.New("task 4: bad")})
	if err := blockForDecompositionFailure(context.Background(), decompDB{tx}, workID, reason); err != nil {
		t.Fatalf("blockForDecompositionFailure: %v", err)
	}
	if !tx.committed {
		t.Fatal("not committed")
	}
	var blocked, enqueued bool
	for i, sql := range tx.sqls {
		if strings.Contains(sql, "UPDATE work_items SET state") && tx.args[i][0] == models.WorkItemStateBlocked {
			blocked = true
		}
		if strings.Contains(sql, "INSERT INTO outbox_events") && tx.args[i][3] == bus.TopicWorkBlocked {
			var body map[string]any
			if err := json.Unmarshal(tx.args[i][5].([]byte), &body); err != nil {
				t.Fatal(err)
			}
			if body["work_id"] != workID.String() || body["reason"] != reason {
				t.Errorf("work.blocked payload = %v", body)
			}
			enqueued = true
		}
	}
	if !blocked || !enqueued {
		t.Fatalf("blocked=%t work.blocked=%t; statements %q", blocked, enqueued, tx.sqls)
	}
}

// The handler blocks a decomposition failure through that path (R9: via
// escalateBlock, which every planning block uses).
func TestHandler_DecompositionFailureBlocksThroughTheOutbox(t *testing.T) {
	code := stripComments(plannerSources(t)["main.go"])
	for _, want := range []string{
		"decompositionBlockReason(outcome.decomposeErr)",
		"escalateBlock(msgCtx, idempotency.Wrap(msgCtx, pool), workID, decompositionBlockKind(outcome.decomposeErr), reason, directBlock)",
	} {
		if !strings.Contains(code, want) {
			t.Errorf("main.go lacks %q", want)
		}
	}
	if !strings.Contains(stripComments(plannerSources(t)["blocks.go"]), "blockForDecompositionFailure(ctx, db, workID, reason)") {
		t.Error("escalateBlock does not block through blockForDecompositionFailure")
	}
}

// Two derived criteria with the same new id are still an error.
func TestNormaliseDerivedCriteria_DuplicateDerivedIDStillRejected(t *testing.T) {
	brief := testBriefCriteria()
	tasks := []decomposedTask{
		noticeTask(derivedCriterion{ID: "AC-5", Text: "t one", Reason: "r", Risk: plan.RiskLow}),
		noticeTask(derivedCriterion{ID: "AC-5", Text: "t two", Reason: "r", Risk: plan.RiskLow}),
	}
	normaliseDerivedCriteria(tasks, brief, nil)
	err := validateDecomposedTasks(tasks, brief, nil)
	if err == nil || !strings.Contains(err.Error(), "derived criterion id AC-5 is used twice") {
		t.Fatalf("err = %v, want the duplicate derived id rejected", err)
	}
}
