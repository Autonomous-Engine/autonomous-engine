package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/llm/adapter"
	"github.com/autonomousengine/autonomous-engine/internal/plan"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// scriptedAdapter returns its responses in order and records every request.
type scriptedAdapter struct {
	responses []string
	requests  []adapter.CompletionRequest
}

func (s *scriptedAdapter) Complete(_ context.Context, req adapter.CompletionRequest) (*adapter.CompletionResponse, error) {
	s.requests = append(s.requests, req)
	if len(s.responses) == 0 {
		return nil, errors.New("no scripted response left")
	}
	r := s.responses[0]
	s.responses = s.responses[1:]
	return &adapter.CompletionResponse{Content: r}, nil
}
func (s *scriptedAdapter) Stream(context.Context, adapter.CompletionRequest) (<-chan adapter.StreamChunk, error) {
	return nil, errors.New("not used")
}
func (s *scriptedAdapter) Capabilities() adapter.ModelCapabilities {
	return adapter.ModelCapabilities{}
}
func (s *scriptedAdapter) HealthCheck(context.Context) error { return nil }
func (s *scriptedAdapter) Name() string                      { return "scripted" }

// repoFixture is a clone with two files.
func repoFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, p := range []string{"src/components/Contact.tsx", "src/api/contact.ts"} {
		full := filepath.Join(root, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func fixtureCalls() []toolCallRecord {
	return []toolCallRecord{
		{callID: "call_read", tool: "read_file", arguments: `{"path":"src/components/Contact.tsx"}`, result: "export function ContactForm() {}"},
		{callID: "call_sym", tool: "codex_workspace_symbols", arguments: `{"query":"ContactForm"}`, result: "ContactForm src/components/Contact.tsx:3"},
		{callID: "call_mod", tool: "codex_find_module", arguments: `{"query":"contact"}`, result: `{"modules":[{"name":"contact"}]}`},
		{callID: "call_bad", tool: "read_file", arguments: `{"path":"nope.ts"}`, result: "no such file", isError: true},
		{callID: "call_search", tool: "search_code", arguments: `{"query":"contact"}`, result: "src/api/contact.ts:1:x"},
		{tool: "list_files", arguments: `{"path":"src"}`, result: "components/ api/"},
	}
}

func TestEvidenceCatalogue(t *testing.T) {
	cat := buildEvidenceCatalogue(fixtureCalls())
	if len(cat.list) != 4 {
		t.Fatalf("want read_file, lsp, codex and list_files entries (errored call and search_code excluded), got %+v", cat.list)
	}
	if cat.byID["c1"].Kind != plan.EvidenceRepo || cat.byID["c2"].Kind != plan.EvidenceLSP || cat.byID["c3"].Kind != plan.EvidenceCodex {
		t.Fatalf("wrong classes: %+v", cat.byID)
	}
	if cat.byID["c6"].Tool != "list_files" {
		t.Fatalf("every call has a positional id, with or without a provider id: %+v", cat.byID)
	}
	if _, problem := cat.resolve("c4"); problem == "" {
		t.Fatal("an errored call is not evidence")
	}
	if _, problem := cat.resolve("call_search"); problem == "" {
		t.Fatal("search_code is not an acceptable evidence source")
	}
	if e, problem := cat.resolve("call_read"); problem != "" || e.ID != "c1" {
		t.Fatalf("a unique raw id resolves to its positional id: %+v %q", e, problem)
	}
}

// Ollama numbers tool calls per turn, so call_0 names a different call in
// every turn. Each call must stay citable by its positional id, and a raw id
// that names several calls must not be resolved to any of them.
func TestEvidenceCatalogueReusedProviderIDs(t *testing.T) {
	calls := []toolCallRecord{
		{callID: "call_0", tool: "codex_find_module", arguments: `{"query":"calc"}`, result: "calculate"},
		{callID: "call_0", tool: "read_file", arguments: `{"path":"calculate.go"}`, result: "package main"},
	}
	cat := buildEvidenceCatalogue(calls)
	if len(cat.list) != 2 || cat.list[0].ID != "c1" || cat.list[1].ID != "c2" {
		t.Fatalf("both calls are catalogued with unique ids: %+v", cat.list)
	}
	if _, problem := cat.resolve("call_0"); !strings.Contains(problem, "ambiguous") {
		t.Fatalf("a reused raw id is rejected as ambiguous, got %q", problem)
	}
	_, user := buildCriteriaPrompt(criteriaInput{}, cat.list)
	if !strings.Contains(user, "id=c1 kind=codex") || !strings.Contains(user, "id=c2 kind=repo") {
		t.Fatalf("the prompt shows the unique ids:\n%s", user)
	}

	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "calculate.go"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	roots := criteriaRootsFor([]string{root})
	good := groundFile(plan.FileRef{Path: "calculate.go", Evidence: plan.EvidenceRef{Kind: plan.EvidenceRepo, Ref: "c2", Confidence: 0.9}}, cat, roots)
	if good.Evidence.Kind != plan.EvidenceRepo || good.Evidence.Ref != "c2" || good.Evidence.Note != "" {
		t.Fatalf("the second call is citable by its own id: %+v", good.Evidence)
	}
	amb := groundFile(plan.FileRef{Path: "calculate.go", Evidence: plan.EvidenceRef{Kind: plan.EvidenceRepo, Ref: "call_0", Confidence: 0.9}}, cat, roots)
	if amb.Evidence.Ref != plan.RefStat || !strings.Contains(amb.Evidence.Note, "ambiguous") {
		t.Fatalf("an ambiguous citation does not count (stat still decides existence): %+v", amb.Evidence)
	}
	sym := groundCitation(plan.EvidenceRef{Kind: plan.EvidenceCodex, Ref: "call_0", Confidence: 0.5}, cat)
	if sym.Kind != plan.EvidenceHypothesis {
		t.Fatalf("an ambiguous symbol citation is a hypothesis: %+v", sym)
	}
}

func TestNewFileInMissingDirectoryIsFlagged(t *testing.T) {
	root := repoFixture(t)
	roots := criteriaRootsFor([]string{root})
	cat := buildEvidenceCatalogue(nil)
	inExisting := groundFile(plan.FileRef{Path: "src/api/new.ts", Evidence: plan.EvidenceRef{Kind: plan.EvidenceNew}}, cat, roots)
	if inExisting.Evidence.Kind != plan.EvidenceNew || inExisting.Evidence.Note != "" {
		t.Fatalf("a new file in an existing directory: %+v", inExisting.Evidence)
	}
	inMissing := groundFile(plan.FileRef{Path: "lib/deep/new.ts", Evidence: plan.EvidenceRef{Kind: plan.EvidenceNew}}, cat, roots)
	if inMissing.Evidence.Kind != plan.EvidenceNew || !strings.Contains(inMissing.Evidence.Note, "directory lib/deep does not exist") {
		t.Fatalf("a new file in a missing directory carries the note: %+v", inMissing.Evidence)
	}
}

func TestCriteriaPromptFormat(t *testing.T) {
	list := buildEvidenceCatalogue(fixtureCalls()).list
	sys, user := buildCriteriaPrompt(criteriaInput{Request: "Add a contact form", PRD: "PRD body", Findings: "found it"}, list)
	for _, want := range []string{
		`"schema_version":1`, `MUST carry "quote"`, `MUST carry "reason"`,
		"EVIDENCE CATALOGUE", "codex|lsp|repo|hypothesis|new", "low|medium|high",
		"test|build|lint|diagnostic|other", `"new"`, `"hypothesis"`,
	} {
		if !strings.Contains(sys, want) {
			t.Errorf("system prompt lacks %q", want)
		}
	}
	for _, want := range []string{"## REQUEST", "Add a contact form", "## PRD", "## RESEARCH FINDINGS", "id=c1 kind=repo tool=read_file", "id=c2 kind=lsp"} {
		if !strings.Contains(user, want) {
			t.Errorf("user prompt lacks %q:\n%s", want, user)
		}
	}
}

const goodCriteria = `{"schema_version":1,"criteria":[
 {"id":"AC-1","text":"Submitting the contact form with an empty email shows an error","source":{"kind":"user","quote":"must not accept an empty email"},
  "files":[
    {"path":"src/components/Contact.tsx","evidence":{"kind":"repo","ref":"call_read","confidence":0.9}},
    {"path":"src/api/contact.ts","evidence":{"kind":"codex","ref":"call_invented","confidence":0.7}},
    {"path":"src/lib/validate.ts","evidence":{"kind":"new","confidence":0.6}},
    {"path":"src/old/Legacy.tsx","evidence":{"kind":"repo","ref":"call_read","confidence":0.5}},
    {"path":"src/components/Contact.tsx","evidence":{"kind":"hypothesis","confidence":0}}
  ],
  "symbols":[{"name":"ContactForm","file":"src/components/Contact.tsx","line":3,"evidence":{"kind":"lsp","ref":"call_sym","confidence":0.8}},
             {"name":"submit","evidence":{"kind":"lsp","ref":"call_mod","confidence":0.8}}],
  "risk":"medium","required_checks":["test"]},
 {"id":"AC-2","text":"The form keeps working on mobile","source":{"kind":"user","quote":"works great on phones too"},"risk":"low"}
]}`

func TestExtractBriefCriteriaGrounds(t *testing.T) {
	root := repoFixture(t)
	a := &scriptedAdapter{responses: []string{"```json\n" + goodCriteria + "\n```"}}
	in := criteriaInput{
		Request: "Contact form\n\nThe form must not accept an empty email address.",
		PRD:     "## Goals\nValidation.",
		Calls:   fixtureCalls(),
		Roots:   criteriaRootsFor([]string{root}),
	}
	bc, notes, err := extractBriefCriteria(context.Background(), a, in)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if len(a.requests) != 1 {
		t.Fatalf("one call expected, got %d", len(a.requests))
	}
	if len(a.requests[0].Tools) != 0 {
		t.Fatal("the criteria pass must not offer tools")
	}
	files := map[string]plan.EvidenceRef{}
	for _, f := range bc.Criteria[0].Files {
		files[f.Path] = f.Evidence
	}
	if len(bc.Criteria[0].Files) != 4 {
		t.Fatalf("duplicate path should be merged, got %+v", bc.Criteria[0].Files)
	}
	// Valid repo citation, exists → kept, basis recorded.
	if e := files["src/components/Contact.tsx"]; e.Kind != plan.EvidenceRepo || e.Ref != "c1" || e.Basis != plan.BasisClone {
		t.Errorf("Contact.tsx: %+v", e)
	}
	// Invented citation, but the path exists → repo by stat, with the reason.
	if e := files["src/api/contact.ts"]; e.Kind != plan.EvidenceRepo || e.Ref != plan.RefStat || !strings.Contains(e.Note, "call_invented") {
		t.Errorf("contact.ts: %+v", e)
	}
	// New and absent → stays new.
	if e := files["src/lib/validate.ts"]; e.Kind != plan.EvidenceNew {
		t.Errorf("validate.ts: %+v", e)
	}
	// Cited but absent from the tree → hypothesis, not dropped.
	if e := files["src/old/Legacy.tsx"]; e.Kind != plan.EvidenceHypothesis || !strings.Contains(e.Note, "not found in the clone") {
		t.Errorf("Legacy.tsx: %+v", e)
	}
	if s := bc.Criteria[0].Symbols; s[0].Evidence.Kind != plan.EvidenceLSP || s[1].Evidence.Kind != plan.EvidenceHypothesis {
		t.Errorf("symbols: %+v", s)
	}
	if bc.Criteria[0].Source.Kind != plan.SourceUser {
		t.Errorf("AC-1 quote is in the request: %+v", bc.Criteria[0].Source)
	}
	if src := bc.Criteria[1].Source; src.Kind != plan.SourceDerived || src.Reason == "" {
		t.Errorf("AC-2 quote is not in the request or PRD, want derived with reason: %+v", src)
	}
	if bc.FileEvidence["src/old/Legacy.tsx"].Kind != plan.EvidenceHypothesis || len(bc.FileEvidence) != 4 {
		t.Errorf("file evidence map: %+v", bc.FileEvidence)
	}
	if len(notes) == 0 {
		t.Error("grounding changes should be reported")
	}
	if err := bc.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestExtractBriefCriteriaReprompsOnceThenFails(t *testing.T) {
	bad := `{"schema_version":1,"criteria":[{"id":"AC-1","text":"t","source":{"kind":"user","quote":"q"},"risk":"extreme"}]}`
	a := &scriptedAdapter{responses: []string{bad, bad}}
	_, _, err := extractBriefCriteria(context.Background(), a, criteriaInput{Request: "q"})
	if err == nil || !strings.Contains(err.Error(), "extreme") || !strings.Contains(err.Error(), "re-prompt") {
		t.Fatalf("want the parse error after one re-prompt, got %v", err)
	}
	if len(a.requests) != 2 {
		t.Fatalf("want exactly 2 calls (one re-prompt), got %d", len(a.requests))
	}
	last := a.requests[1].Messages[len(a.requests[1].Messages)-1].Content
	if !strings.Contains(last, "rejected") || !strings.Contains(last, "extreme") {
		t.Fatalf("re-prompt must carry the rejection: %q", last)
	}
}

func TestExtractBriefCriteriaRecoversOnReprompt(t *testing.T) {
	bad := `{"criteria":[{"id":"AC-1","text":"t","source":{"kind":"user","quote":"q"},"risk":"low","files":[{"path":"a.go","evidence":{"kind":"grep","confidence":1}}]}]}`
	good := `{"criteria":[{"id":"AC-1","text":"t","source":{"kind":"derived","reason":"r"},"risk":"low","files":[{"path":"a.go","evidence":{"kind":"hypothesis","confidence":0}}]}]}`
	var logged int
	a := &scriptedAdapter{responses: []string{bad, good}}
	bc, _, err := extractBriefCriteria(context.Background(), a, criteriaInput{
		LogInference: func(*adapter.CompletionResponse, time.Duration, error) { logged++ },
	})
	if err != nil {
		t.Fatal(err)
	}
	if bc.SchemaVersion != 1 || bc.Criteria[0].Files[0].Evidence.Kind != plan.EvidenceHypothesis {
		t.Fatalf("got %+v", bc)
	}
	if logged != 2 {
		t.Fatalf("each call is recorded, got %d", logged)
	}
}

func TestParseCriteriaOutputStrict(t *testing.T) {
	for name, s := range map[string]string{
		"no json":       "I could not find anything",
		"empty":         `{"schema_version":1,"criteria":[]}`,
		"unknown kind":  `{"criteria":[{"id":"AC-1","text":"t","source":{"kind":"assumed"},"risk":"low"}]}`,
		"unknown check": `{"criteria":[{"id":"AC-1","text":"t","source":{"kind":"derived","reason":"r"},"risk":"low","required_checks":["e2e"]}]}`,
		"schema 2":      `{"schema_version":2,"criteria":[{"id":"AC-1","text":"t","source":{"kind":"derived","reason":"r"},"risk":"low"}]}`,
	} {
		if _, err := parseCriteriaOutput(s); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := parseCriteriaOutput("<think>hmm {not json}</think>" + `{"criteria":[{"id":"AC-1","text":"t","source":{"kind":"derived","reason":"r"},"risk":"low"}]}`); err != nil {
		t.Errorf("thinking tags should be stripped: %v", err)
	}
}

type budgetDB struct {
	used  int64
	execs int
}

type budgetRow struct{ v int64 }

func (r budgetRow) Scan(dest ...any) error {
	*(dest[0].(*int64)) = r.v
	return nil
}

func (d *budgetDB) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	d.execs++
	return pgconn.CommandTag{}, nil
}
func (d *budgetDB) QueryRow(context.Context, string, ...any) pgx.Row { return budgetRow{d.used} }

func TestCriteriaPassSkippedWhenBudgetExhausted(t *testing.T) {
	t.Setenv("WORK_ITEM_TOKEN_BUDGET", "1000")
	a := &scriptedAdapter{responses: []string{goodCriteria}}
	db := &budgetDB{used: 1000}
	got, err := runCriteriaPass(context.Background(), db, a, uuid.New(), nil, researchTaskMessage{TaskID: "t"}, nil, "", nil)
	if err != nil || got != nil {
		t.Fatalf("exhausted budget: want no criteria and no error, got %+v, %v", got, err)
	}
	if len(a.requests) != 0 || db.execs != 0 {
		t.Fatalf("no LLM call and no inference row when skipped: requests=%d execs=%d", len(a.requests), db.execs)
	}
	if exhausted, _, _ := criteriaBudgetExhausted(context.Background(), &budgetDB{used: 999}, uuid.New()); exhausted {
		t.Fatal("999 of 1000 is not exhausted")
	}
}

func TestCleanRepoPath(t *testing.T) {
	roots := []criteriaRoot{{Path: "/repos/app", Basis: plan.BasisClone}}
	cases := map[string]string{
		"./src/a.ts":          "src/a.ts",
		"/repos/app/src/a.ts": "src/a.ts",
		"src\\b.ts":           "src/b.ts",
		"src/../src/c.ts":     "src/c.ts",
		"../etc/passwd":       "",
		"/etc/passwd":         "",
		"":                    "",
	}
	for in, want := range cases {
		got, ok := cleanRepoPath(in, roots)
		if want == "" {
			if ok {
				t.Errorf("%q: want rejected, got %q", in, got)
			}
			continue
		}
		if !ok || got != want {
			t.Errorf("%q: got %q ok=%v want %q", in, got, ok, want)
		}
	}
}
