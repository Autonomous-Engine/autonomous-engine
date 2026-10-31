package main

// The recorded-decomposition regression corpus (R9 P3). Every case is a
// planning run of an early live run (qwen2.5-coder:32b), rebuilt as a
// synthetic fixture: the technical brief the planner read and the model's
// responses, in the shape exported from the database (work_item_artifacts,
// inference_logs), with the application replaced by a made-up "sample-app"
// (a small web app whose map pages need a tile API key). The
// table test replays each case through planWithPolicy and the real
// decomposeIntoTasks against a local fake of the provider (no live model),
// so every planner change is checked against model output of that shape.
//
// The fake answers by role. Each recorded response answered one kind of
// prompt (source.json "answers"): "decompose" (the plain decomposition
// prompt), "correction" (decomposeIntoTasks' re-prompt after an
// unparseable or strictly rejected answer) or "repair" (the plan-validation
// repair round). A request is answered with the next unused response of its
// role. When the current planner asks for something the run never recorded
// (attempt 7 was blocked before any repair round existed for it), the fake
// answers nothing usable and the case ends "unrecorded:<role>": the planner
// did not block, it asked the model, and what the model would have said is
// not in the corpus. The case then asserts what the planner asked.
//
// Layout: testdata/corpus/<case>/
//
//	brief.json      the technical_brief artifact's content
//	response_N.txt  the Nth decompose_tasks response_content
//	prompt_N.json   the Nth call's prompt_messages (provenance; not replayed)
//	source.json     work item, artifact and inference_logs ids, the model,
//	                and the role of each response
//
// How to add a case: see testdata/corpus/README.md.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/config"
	"github.com/autonomousengine/autonomous-engine/internal/llm"
	"github.com/autonomousengine/autonomous-engine/internal/plan"
	"github.com/google/uuid"
)

const corpusDir = "testdata/corpus"

// Roles of a recorded response (source.json "answers").
const (
	roleDecompose  = "decompose"
	roleCorrection = "correction"
	roleRepair     = "repair"
)

// corpusCase is one recorded run and what the current planner must do with
// it.
type corpusCase struct {
	name string
	// recorded is what happened live.
	recorded string
	want     corpusWant
}

type corpusWant struct {
	// outcome is "planned", "blocked", or "unrecorded:<role>".
	outcome string
	// served is how many recorded responses the fake served.
	served int
	// reason is a substring of the block reason (blocked only).
	reason string
	// rehomed and gaps are the criteria the planner re-homed by file
	// overlap and the ones it recorded as gaps (non-code or unowned).
	rehomed []string
	gaps    []string
	// attached are the checks the planner attached (verifier_missing's
	// normaliser), as "<criterion>:<kind>@<task>".
	attached []string
	// asked are substrings of the last request the planner sent that the
	// recording could not answer (unrecorded only): what it asked for.
	asked []string
	// notAsked must not be in that request.
	notAsked []string
}

// With verifier_missing normalised (R9 ruling: the repository's one
// whole-suite command of the kind, attached to the lowest-index owner),
// every case but attempt 6 plans on the model's first decomposition: the
// recorded repair rounds were spent on missing checks and orphans the
// planner now settles itself. Attempt 6's missing check is a diagnostic
// kind no step runs, so it still takes the recorded repair round.
var corpusCases = []corpusCase{
	{
		name:     "attempt5",
		recorded: "blocked: [orphan_criterion] AC-7 after the repair round (the documentation task was dropped)",
		want: corpusWant{outcome: "planned", served: 1,
			attached: []string{"AC-2:test@4", "AC-5:test@4", "AC-6:test@5", "AC-6:build@5", "AC-6:lint@5"}},
	},
	{
		name:     "attempt6",
		recorded: "planned after one repair round (verifier_missing AC-3)",
		want:     corpusWant{outcome: "planned", served: 2},
	},
	{
		// R7 normalises the AC-9 redeclaration; the documentation task's
		// missing test check gets the plan's one "npm run test".
		name:     "attempt7",
		recorded: "blocked: derived criterion AC-9 reuses a brief criterion id (task 4), after one correction",
		want:     corpusWant{outcome: "planned", served: 1, attached: []string{"AC-7:test@7"}},
	},
	{
		name:     "attempt7b",
		recorded: "blocked: derived criterion AC-9 reuses a brief criterion id (task 3), after one correction",
		want: corpusWant{outcome: "planned", served: 1, rehomed: []string{"AC-6"},
			attached: []string{"AC-5:test@6", "AC-7:test@7"}},
	},
	{
		name:     "attempt8",
		recorded: "blocked: [orphan_criterion] AC-6 after the repair round",
		want: corpusWant{outcome: "planned", served: 1, rehomed: []string{"AC-6"},
			attached: []string{"AC-4:test@4", "AC-5:test@5", "AC-7:build@6", "AC-7:test@6", "AC-9:test@4"}},
	},
	{
		name:     "attempt9",
		recorded: "planned after one repair round (orphans AC-4, AC-6; verifier_missing AC-5, AC-7)",
		want: corpusWant{outcome: "planned", served: 1, rehomed: []string{"AC-4", "AC-6"},
			attached: []string{"AC-5:test@4", "AC-7:test@5"}},
	},
	{
		// R13: task 9 "Add pull request description for API key" declared
		// derived AC-11 and cited nothing (no_criteria, orphan AC-11); the
		// live repair cited AC-11 and dropped its definition
		// (unknown_criterion). A task now cites its own derived criteria
		// (D1). Task 9 names no repository file and produces the PR text,
		// so it is dropped as non-code work and AC-11, which targets the PR
		// description, is a non-code gap that pr-coordinator writes into the
		// PR description (D3). The first decomposition is accepted.
		name:     "attempt13",
		recorded: "blocked: [unknown_criterion] task 9 cites AC-11 after the repair round (round 1: no_criteria task 9, orphan AC-11; the repair dropped AC-11's definition)",
		want: corpusWant{outcome: "planned", served: 1, gaps: []string{"AC-11"},
			attached: []string{"AC-3:build@6", "AC-8:test@3"}},
	},
}

// corpusSource is a case's source.json.
type corpusSource struct {
	WorkItemID               string `json:"work_item_id"`
	TechnicalBriefArtifactID string `json:"technical_brief_artifact_id"`
	Responses                []struct {
		File           string `json:"file"`
		Prompt         string `json:"prompt"`
		Answers        string `json:"answers"`
		InferenceLogID string `json:"inference_log_id"`
		Model          string `json:"model"`
	} `json:"responses"`
}

func loadCorpusSource(t *testing.T, dir string) corpusSource {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "source.json"))
	if err != nil {
		t.Fatal(err)
	}
	var src corpusSource
	if err := json.Unmarshal(data, &src); err != nil {
		t.Fatalf("%s: source.json: %v", dir, err)
	}
	return src
}

func loadCorpusBrief(t *testing.T, dir string) structuredBrief {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "brief.json"))
	if err != nil {
		t.Fatal(err)
	}
	var brief structuredBrief
	if err := json.Unmarshal(data, &brief); err != nil {
		t.Fatal(err)
	}
	return brief
}

// corpusLLM is the role-aware fake provider.
type corpusLLM struct {
	mu        sync.Mutex
	responses map[string][]string // by role, in recorded order
	served    int
	// unrecorded is the role of the first request no recorded response
	// answered, and lastAsked that request's body.
	unrecorded string
	lastAsked  string
}

// requestRole classifies a decomposition request by the prompt the planner
// built: a correction nudge (decomposeIntoTasks' retry_nudge) wins over the
// repair round it may be part of.
func requestRole(body string) string {
	switch {
	case strings.Contains(body, "previous response was rejected") || strings.Contains(body, "could not be parsed as a JSON array"):
		return roleCorrection
	case strings.Contains(body, "rejected by the plan validator"):
		return roleRepair
	}
	return roleDecompose
}

const unrecordedAnswer = "UNRECORDED: the corpus holds no response to this prompt"

func (f *corpusLLM) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var req struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	_ = json.Unmarshal(body, &req)
	content, decompose := "", false
	for _, m := range req.Messages {
		if m.Role == "assistant" {
			content = m.Content // a self-review pass: echo the answer
		}
		if strings.Contains(m.Content, "derived_criteria") {
			decompose = true
		}
	}
	f.mu.Lock()
	switch {
	case !decompose:
		content = "ok"
	case content == "":
		// Once the recording has run out, nothing after it is replayed:
		// a later request answers a conversation that never happened.
		role := requestRole(string(body))
		if q := f.responses[role]; len(q) > 0 && f.unrecorded == "" {
			content, f.responses[role] = q[0], q[1:]
			f.served++
		} else {
			if f.unrecorded == "" {
				f.unrecorded, f.lastAsked = role, string(body)
			}
			content = unrecordedAnswer
		}
	}
	f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"id":    "chatcmpl-corpus",
		"model": "fake",
		"choices": []map[string]any{{
			"message":       map[string]any{"role": "assistant", "content": content},
			"finish_reason": "stop",
		}},
		"usage": map[string]any{"prompt_tokens": 1, "completion_tokens": 1},
	})
}

// corpusResult is a replayed case's outcome in the corpus's terms.
type corpusResult struct {
	outcome  string
	reason   string
	served   int
	rehomed  []string
	attached []string
	gaps     []string
	asked    string
}

// replayCorpusCase runs one case as handleTechnicalBrief does, minus the
// database: the brief's untrusted marks are stripped and invalid criteria
// dropped (loadStructuredBrief, briefCriteriaOf), the brief is folded before
// the prompt (foldBrief), the decomposition goes through the real
// decomposeIntoTasks against the fake, and phase 1 is capturedBuild (every
// repository's policy requires a test check, as the default policy does;
// paths are not stat'ed, as there is no snapshot).
func replayCorpusCase(t *testing.T, name string, rb repairBudget) corpusResult {
	t.Helper()
	dir := filepath.Join(corpusDir, name)
	src := loadCorpusSource(t, dir)
	fake := &corpusLLM{responses: map[string][]string{}}
	for _, r := range src.Responses {
		b, err := os.ReadFile(filepath.Join(dir, r.File))
		if err != nil {
			t.Fatal(err)
		}
		fake.responses[r.Answers] = append(fake.responses[r.Answers], string(b))
	}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	svc, err := llm.NewService(&config.ModelConfig{Provider: "openai", Endpoint: srv.URL, APIKey: "test", Name: "fake"}, nil)
	if err != nil {
		t.Fatal(err)
	}

	structured := loadCorpusBrief(t, dir)
	plan.StripNonCode(structured.Criteria)
	if _, err := briefCriteriaOf(structured); err != nil {
		t.Logf("%s: brief criteria invalid (%v) — planning without them, as the handler does", name, err)
		structured.Criteria = nil
	}
	unfolded := structured.Criteria
	var folds []plan.Fold
	structured.Criteria, folds = foldBrief(uuid.Nil, structured.Criteria)

	out := planWithPolicy(uuid.Nil,
		func(repair string, reserved []string) ([]decomposedTask, error) {
			return decomposeIntoTasks(context.Background(), svc, uuid.Nil, structured, folds, "", repair, reserved)
		},
		capturedBuilder(unfolded),
		rb,
	)
	res := corpusResult{served: fake.served}
	switch {
	case fake.unrecorded != "":
		res.outcome, res.asked = "unrecorded:"+fake.unrecorded, fake.lastAsked
	case out.decomposeErr != nil:
		res.outcome, res.reason = "blocked", decompositionBlockReason(out.decomposeErr)+out.previousNote()
	case out.build == nil || out.build.noRepository:
		res.outcome, res.reason = "blocked", "no repository"
	case out.build.allNonCode:
		res.outcome, res.reason = "blocked", out.build.allNonCodeReason()
	case out.rejected():
		res.outcome, res.reason = "blocked", planValidationReason(plan.Errors(out.violations), out.attempts)+out.blockNote()
	default:
		res.outcome = "planned"
		attachNonCodeCriteria(out.build)
		for _, r := range out.build.rehomed {
			res.rehomed = append(res.rehomed, r.CriterionID)
		}
		for _, a := range out.build.attached {
			res.attached = append(res.attached, a.CriterionID+":"+string(a.CheckKind)+"@"+strconv.Itoa(a.TaskIndex))
		}
		if out.build.criteria != nil {
			for _, c := range out.build.criteria.Criteria {
				if c.NonCode {
					res.gaps = append(res.gaps, c.ID)
				}
			}
		}
	}
	return res
}

// unescapedBody undoes the JSON string escaping of a request body, so the
// repair prompt's text can be matched as the planner wrote it.
func unescapedBody(body string) string {
	return strings.NewReplacer(`\"`, `"`, `\n`, "\n", `<`, "<", `>`, ">", `&`, "&").Replace(body)
}

// The corpus: every case's outcome on the current planner.
func TestRecordedDecompositionCorpus(t *testing.T) {
	entries, err := os.ReadDir(corpusDir)
	if err != nil {
		t.Fatal(err)
	}
	listed := map[string]bool{}
	for _, c := range corpusCases {
		listed[c.name] = true
	}
	for _, e := range entries {
		if e.IsDir() && !listed[e.Name()] {
			t.Errorf("corpus case %s has no expectation in corpusCases", e.Name())
		}
	}
	for _, c := range corpusCases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			got := replayCorpusCase(t, c.name, repairBudget{budget: 500_000, spent: func() (int64, error) { return 0, nil }})
			t.Logf("%s: recorded %s; now %s (served %d, rehomed %v, attached %v, gaps %v) %s", c.name, c.recorded, got.outcome, got.served, got.rehomed, got.attached, got.gaps, got.reason)
			if got.asked != "" && os.Getenv("CORPUS_DUMP") != "" {
				t.Logf("asked:\n%s", unescapedBody(got.asked))
			}
			if got.outcome != c.want.outcome {
				t.Fatalf("outcome = %s (%s), want %s", got.outcome, got.reason, c.want.outcome)
			}
			if got.served != c.want.served {
				t.Errorf("recorded responses served = %d, want %d", got.served, c.want.served)
			}
			if c.want.reason != "" && !strings.Contains(got.reason, c.want.reason) {
				t.Errorf("reason = %q, want it to contain %q", got.reason, c.want.reason)
			}
			if strings.Join(got.rehomed, ",") != strings.Join(c.want.rehomed, ",") {
				t.Errorf("rehomed = %v, want %v", got.rehomed, c.want.rehomed)
			}
			if strings.Join(got.attached, ",") != strings.Join(c.want.attached, ",") {
				t.Errorf("attached = %v, want %v", got.attached, c.want.attached)
			}
			if strings.Join(got.gaps, ",") != strings.Join(c.want.gaps, ",") {
				t.Errorf("gaps = %v, want %v", got.gaps, c.want.gaps)
			}
			asked := unescapedBody(got.asked)
			for _, a := range c.want.asked {
				if !strings.Contains(asked, a) {
					t.Errorf("the unrecorded request lacks %q", a)
				}
			}
			if i := strings.Index(asked, "Your previous plan (task 1"); i >= 0 {
				asked = asked[:i] // the quoted plan may name anything
			}
			for _, a := range c.want.notAsked {
				if strings.Contains(asked, a) {
					t.Errorf("the unrecorded request asks %q", a)
				}
			}
		})
	}
}

// Every case directory is complete: a brief with prose and criteria, and
// every response source.json names, each with a known role.
func TestCorpusCasesAreComplete(t *testing.T) {
	for _, c := range corpusCases {
		dir := filepath.Join(corpusDir, c.name)
		brief := loadCorpusBrief(t, dir)
		if strings.TrimSpace(brief.proseContent()) == "" || brief.Criteria == nil || len(brief.Criteria.Criteria) == 0 {
			t.Errorf("%s: the brief has no prose or no criteria", c.name)
		}
		src := loadCorpusSource(t, dir)
		if src.WorkItemID == "" || src.TechnicalBriefArtifactID == "" || len(src.Responses) == 0 {
			t.Errorf("%s: source.json incomplete: %+v", c.name, src)
		}
		for _, r := range src.Responses {
			switch r.Answers {
			case roleDecompose, roleCorrection, roleRepair:
			default:
				t.Errorf("%s: %s answers %q", c.name, r.File, r.Answers)
			}
			if _, err := os.Stat(filepath.Join(dir, r.File)); err != nil {
				t.Errorf("%s: %v", c.name, err)
			}
			if r.InferenceLogID == "" {
				t.Errorf("%s: %s has no inference_log_id", c.name, r.File)
			}
		}
	}
}
