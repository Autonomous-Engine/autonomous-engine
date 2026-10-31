package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/evidence"
	"github.com/autonomousengine/autonomous-engine/internal/plan"
)

func testBriefCriteria() *plan.BriefCriteria {
	return &plan.BriefCriteria{
		SchemaVersion: 1,
		Criteria: []plan.Criterion{
			{
				ID: "AC-1", Text: "Empty email is rejected with a visible error",
				Source:         plan.Source{Kind: plan.SourceUser, Quote: "must not accept an empty email"},
				Risk:           plan.RiskMedium,
				RequiredChecks: []evidence.CheckKind{evidence.CheckKindTest},
				Files:          []plan.FileRef{{Path: "src/Contact.tsx", Evidence: plan.EvidenceRef{Kind: plan.EvidenceLSP, Ref: "call_9", Confidence: 0.8}}},
			},
			{ID: "AC-2", Text: "Existing submissions still work", Source: plan.Source{Kind: plan.SourceDerived, Reason: "shared handler"}, Risk: plan.RiskLow},
		},
		FileEvidence: map[string]plan.EvidenceRef{
			"src/Contact.tsx": {Kind: plan.EvidenceLSP, Ref: "call_9", Confidence: 0.8},
			"src/ghost.ts":    {Kind: plan.EvidenceCodex, Ref: "call_4", Confidence: 0.6},
		},
	}
}

func TestParseTaskArrayReadsTheTaskContract(t *testing.T) {
	raw := `[{"title":"Validate email","description":"d",
	  "criterion_ids":["AC-1"],"deliverable":"empty email shows an error",
	  "verification":[{"check_kind":"test","command":"npm test -- contact"}],
	  "files":[{"path":"src/Contact.tsx","evidence_ref":"lsp:call_9"},"src/legacy-string.ts",{"path":"src/new.ts","evidence_ref":"new"}],
	  "depends_on":[],"derived_criteria":[{"id":"AC-3","text":"t","reason":"r","risk":"low"}]}]`
	tasks, err := parseTaskArray(raw)
	if err != nil {
		t.Fatal(err)
	}
	got := tasks[0]
	if strings.Join(got.Files, ",") != "src/Contact.tsx,src/legacy-string.ts,src/new.ts" {
		t.Errorf("Files are the claimed paths: %v", got.Files)
	}
	if got.FileClaims[0].EvidenceRef != "lsp:call_9" || got.FileClaims[1].EvidenceRef != "" {
		t.Errorf("claims: %+v", got.FileClaims)
	}
	if got.Deliverable == "" || len(got.Verification) != 1 || got.CriterionIDs[0] != "AC-1" || got.DerivedCriteria[0].ID != "AC-3" {
		t.Errorf("contract fields: %+v", got)
	}
	if err := validateDecomposedTasks(tasks, testBriefCriteria(), nil); err != nil {
		t.Errorf("valid output rejected: %v", err)
	}
}

func TestParseTaskArrayLegacyStringFiles(t *testing.T) {
	tasks, err := parseTaskArray(`[{"title":"a","description":"b","files":["x.go","y.go"],"depends_on":[]}]`)
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks[0].Files) != 2 {
		t.Fatalf("legacy string files still parse: %+v", tasks[0])
	}
	if err := validateDecomposedTasks(tasks, nil, nil); err != nil {
		t.Fatalf("a legacy task (no criteria, plain paths) is structurally valid: %v", err)
	}
}

func TestValidateDecomposedTasksRejectsUnknownKinds(t *testing.T) {
	base := func() decomposedTask {
		return decomposedTask{Title: "t", CriterionIDs: []string{"AC-1"}, FileClaims: []fileClaim{{Path: "a.go", EvidenceRef: "repo:call_1"}}}
	}
	cases := map[string]func(*decomposedTask){
		"unknown evidence kind": func(d *decomposedTask) { d.FileClaims[0].EvidenceRef = "grep:call_1" },
		"supported kind no ref": func(d *decomposedTask) { d.FileClaims[0].EvidenceRef = "codex:" },
		"bare kind":             func(d *decomposedTask) { d.FileClaims[0].EvidenceRef = "repo" },
		"unknown check":         func(d *decomposedTask) { d.Verification = []plan.VerificationStep{{CheckKind: "smoke", Command: "x"}} },
		"empty verification":    func(d *decomposedTask) { d.Verification = []plan.VerificationStep{{CheckKind: evidence.CheckKindTest}} },
		"bad criterion id":      func(d *decomposedTask) { d.CriterionIDs = []string{"criterion 1"} },
		"duplicate criterion":   func(d *decomposedTask) { d.CriterionIDs = []string{"AC-1", "AC-1"} },
		"empty path":            func(d *decomposedTask) { d.FileClaims[0].Path = " " },
		"derived no reason": func(d *decomposedTask) {
			d.DerivedCriteria = []derivedCriterion{{ID: "AC-3", Text: "t", Risk: plan.RiskLow}}
		},
		"derived bad risk": func(d *decomposedTask) {
			d.DerivedCriteria = []derivedCriterion{{ID: "AC-3", Text: "t", Reason: "r", Risk: "huge"}}
		},
		"derived reuses brief id": func(d *decomposedTask) {
			d.DerivedCriteria = []derivedCriterion{{ID: "AC-2", Text: "t", Reason: "r", Risk: plan.RiskLow}}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			d := base()
			mutate(&d)
			if err := validateDecomposedTasks([]decomposedTask{d}, testBriefCriteria(), nil); err == nil {
				t.Fatal("accepted")
			}
		})
	}
	// The same derived id on two tasks is rejected too.
	d1, d2 := base(), base()
	d1.DerivedCriteria = []derivedCriterion{{ID: "AC-3", Text: "t", Reason: "r", Risk: plan.RiskLow}}
	d2.DerivedCriteria = d1.DerivedCriteria
	if err := validateDecomposedTasks([]decomposedTask{d1, d2}, testBriefCriteria(), nil); err == nil {
		t.Fatal("duplicate derived id across tasks accepted")
	}
}

func TestGroundTaskFilesProducesEvidence(t *testing.T) {
	root := t.TempDir()
	for _, p := range []string{"src/Contact.tsx", "src/plain.ts"} {
		full := filepath.Join(root, filepath.FromSlash(p))
		_ = os.MkdirAll(filepath.Dir(full), 0o755)
		if err := os.WriteFile(full, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	claims := []fileClaim{
		{Path: "src/Contact.tsx", EvidenceRef: "lsp:call_9"},
		{Path: "src/plain.ts"},                              // exists, no claim → repo by stat
		{Path: "src/ghost.ts", EvidenceRef: "codex:call_4"}, // research cited it, but it is not on disk
		{Path: "src/missing.ts"},                            // unevidenced and missing → hypothesis
		{Path: "src/new.ts", EvidenceRef: "new"},
	}
	got := groundTaskFiles(root, claims, testBriefCriteria().FileEvidence)
	if len(got) != len(claims) {
		t.Fatalf("no path may be dropped: %+v", got)
	}
	want := []struct {
		status fileGrounding
		kind   plan.EvidenceKind
		ref    string
	}{
		{fileExists, plan.EvidenceLSP, "call_9"},
		{fileExists, plan.EvidenceRepo, plan.RefStat},
		{fileMissing, plan.EvidenceHypothesis, ""},
		{fileMissing, plan.EvidenceHypothesis, ""},
		{fileNew, plan.EvidenceNew, ""},
	}
	for i, w := range want {
		g := got[i]
		if g.status != w.status || g.ref.Evidence.Kind != w.kind || g.ref.Evidence.Ref != w.ref {
			t.Errorf("%s: got status=%d %+v, want %+v", g.ref.Path, g.status, g.ref.Evidence, w)
		}
		if err := g.ref.Validate(); err != nil {
			t.Errorf("%s: invalid evidence: %v", g.ref.Path, err)
		}
	}
	if got[1].ref.Evidence.Basis != plan.BasisClone {
		t.Errorf("stat evidence records its basis: %+v", got[1].ref.Evidence)
	}
	if !strings.Contains(got[2].ref.Evidence.Note, "research cited codex call_4") || !strings.Contains(got[3].ref.Evidence.Note, "grounding: not found") {
		t.Errorf("hypotheses carry the grounding note: %+v / %+v", got[2].ref.Evidence, got[3].ref.Evidence)
	}

	unverified := groundTaskFiles("", []fileClaim{{Path: "a.go"}, {Path: "src/Contact.tsx"}, {Path: "n.go", EvidenceRef: "new"}}, testBriefCriteria().FileEvidence)
	if unverified[0].status != fileUnverified || unverified[0].ref.Evidence.Kind != plan.EvidenceHypothesis {
		t.Errorf("no clone path: unevidenced path is an unverified hypothesis: %+v", unverified[0])
	}
	if unverified[1].ref.Evidence.Kind != plan.EvidenceLSP || unverified[2].ref.Evidence.Kind != plan.EvidenceNew {
		t.Errorf("no clone path keeps research evidence and new: %+v", unverified)
	}
}

func TestGroundTaskFilesContainment(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	outsideFile := filepath.Join(filepath.Dir(root), "secret.txt")
	claims := []fileClaim{
		{Path: "../secret.txt"},
		{Path: "src/../../escape.go"},
		{Path: outsideFile},
		{Path: "/etc/passwd"},
		{Path: filepath.ToSlash(filepath.Join(root, "main.go"))}, // absolute under the root
	}
	got := groundTaskFiles(root, claims, nil)
	for _, g := range got[:4] {
		if g.ref.Evidence.Kind != plan.EvidenceHypothesis || g.ref.Evidence.Note != "grounding: path is outside the repository" || g.status != fileMissing {
			t.Errorf("%s: an escaping path is an outside-the-repository hypothesis: %+v status=%d", g.ref.Path, g.ref.Evidence, g.status)
		}
	}
	if g := got[4]; g.status != fileExists || g.ref.Path != "main.go" || g.ref.Evidence.Kind != plan.EvidenceRepo {
		t.Errorf("an absolute path under the clone is made relative and stat'ed: %+v status=%d", g.ref, g.status)
	}
	noClone := groundTaskFiles("", []fileClaim{{Path: "../x.go"}, {Path: "ok.go"}}, nil)
	if noClone[0].ref.Evidence.Note != "grounding: path is outside the repository" || noClone[1].status != fileUnverified {
		t.Errorf("containment applies without a clone path too: %+v", noClone)
	}
}

func TestNewFileInMissingDirectoryMustBeJustified(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	got := groundTaskFiles(root, []fileClaim{
		{Path: "src/new.go", EvidenceRef: "new"},
		{Path: "internal/brandnew/x.go", EvidenceRef: "new"},
		{Path: "top.go", EvidenceRef: "new"},
	}, nil)
	if got[0].ref.Evidence.Note != "" || got[2].ref.Evidence.Note != "" {
		t.Errorf("new files in existing directories carry no note: %+v / %+v", got[0].ref.Evidence, got[2].ref.Evidence)
	}
	if !strings.Contains(got[1].ref.Evidence.Note, "directory internal/brandnew does not exist") {
		t.Errorf("new file in a missing directory: %+v", got[1].ref.Evidence)
	}
	text := renderSubtaskSpec(buildTaskSpec(nil, got), nil)
	if !strings.Contains(text, "Files to be created must be justified: internal/brandnew/x.go") || strings.Contains(text, "justified: src/new.go") {
		t.Errorf("rendering:\n%s", text)
	}
}

func TestTaskWithoutCriteriaIsRenderedAsHypothesis(t *testing.T) {
	step := decomposedTask{Title: "Tweak styles", Description: "d", Files: []string{"src/missing.ts"}}
	stepText := renderStepSpec(step)
	if !strings.Contains(stepText, "HYPOTHESIS") {
		t.Errorf("a step citing no criterion is marked a hypothesis: %q", stepText)
	}
	root := t.TempDir()
	grounded := groundTaskFiles(root, groupFileClaims([]decomposedTask{step}), nil)
	spec := buildTaskSpec([]decomposedTask{step}, grounded)
	if err := spec.Validate(); err != nil {
		t.Fatal(err)
	}
	if len(spec.CriterionIDs) != 0 || len(spec.Files) != 1 || spec.Files[0].Evidence.Kind != plan.EvidenceHypothesis {
		t.Fatalf("the unevidenced path is kept as a hypothesis, not dropped: %+v", spec)
	}
	text := renderSubtaskSpec(spec, nil)
	for _, want := range []string{"## Acceptance criteria", "HYPOTHESIS", "## File evidence", "src/missing.ts — hypothesis — grounding: not found"} {
		if !strings.Contains(text, want) {
			t.Errorf("rendering lacks %q:\n%s", want, text)
		}
	}
}

func TestBuildTaskSpecMergesSteps(t *testing.T) {
	test := plan.VerificationStep{CheckKind: evidence.CheckKindTest, Command: "npm test"}
	steps := []decomposedTask{
		{Title: "A", CriterionIDs: []string{"AC-1"}, Deliverable: "form validates", Verification: []plan.VerificationStep{test}},
		{Title: "B", CriterionIDs: []string{"AC-2", "AC-1", "AC-7"}, Deliverable: "handler unchanged", Verification: []plan.VerificationStep{test, {CheckKind: evidence.CheckKindOther, Description: "submit the form"}}},
	}
	spec := buildTaskSpec(steps, nil)
	if strings.Join(spec.CriterionIDs, ",") != "AC-1,AC-2,AC-7" {
		t.Errorf("ids unioned in order: %v", spec.CriterionIDs)
	}
	if len(spec.Verification) != 2 {
		t.Errorf("verification deduplicated: %+v", spec.Verification)
	}
	if spec.Deliverable != "Step 1 (A): form validates\nStep 2 (B): handler unchanged" {
		t.Errorf("deliverable: %q", spec.Deliverable)
	}
	derived := planDerivedCriteria([]decomposedTask{{DerivedCriteria: []derivedCriterion{{ID: "AC-7", Text: "logs", Reason: "ops need it", Risk: plan.RiskLow}}}})
	crit := relevantCriteria(spec, testBriefCriteria(), derived)
	if len(crit) != 3 || crit[2].Source.Kind != plan.SourceDerived || crit[2].Source.Reason != "ops need it" {
		t.Fatalf("relevant criteria: %+v", crit)
	}
	text := renderSubtaskSpec(spec, crit[:2])
	for _, want := range []string{
		"- AC-1 [risk: medium; checks: test]: Empty email is rejected",
		`Source: user — "must not accept an empty email"`,
		"- AC-7: cited, but not in the brief",
		"## Deliverable", "## Verification", "- [test] `npm test`", "- [other] submit the form",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("rendering lacks %q:\n%s", want, text)
		}
	}
}

func TestCriteriaPromptContext(t *testing.T) {
	ctx := map[string]string{}
	criteriaPromptContext(ctx, testBriefCriteria(), nil)
	ac := ctx["acceptance_criteria"]
	for _, want := range []string{"- AC-1 [risk: medium; checks: test]", "- AC-2 [risk: low]", "MUST list in criterion_ids", "numbering from AC-3", "Never add one silently"} {
		if !strings.Contains(ac, want) {
			t.Errorf("acceptance_criteria lacks %q:\n%s", want, ac)
		}
	}
	fe := ctx["file_evidence"]
	if !strings.Contains(fe, `- src/Contact.tsx — evidence_ref "lsp:call_9"`) || !strings.Contains(fe, `"hypothesis"`) {
		t.Errorf("file_evidence:\n%s", fe)
	}

	legacy := map[string]string{}
	criteriaPromptContext(legacy, nil, nil)
	if !strings.Contains(legacy["acceptance_criteria"], "no acceptance criteria") || !strings.Contains(legacy["acceptance_criteria"], "AC-1") {
		t.Errorf("legacy brief: %q", legacy["acceptance_criteria"])
	}
	if _, ok := legacy["file_evidence"]; ok {
		t.Error("no evidence table without criteria")
	}

	for _, want := range []string{`"criterion_ids"`, `"deliverable"`, `"verification"`, `"check_kind"`, `"evidence_ref":"repo:call_1"`, `"evidence_ref":"new"`, `"evidence_ref":"hypothesis"`, `"derived_criteria"`, "test|build|lint|diagnostic|other",
		// R6 M3: files_to_change grew with files read only for context.
		"list only paths this task creates or modifies, not files read for context"} {
		if !strings.Contains(decomposeOutputFormat, want) {
			t.Errorf("output format lacks %s", want)
		}
	}
}

func TestImplementationTaskMessageCarriesTaskSpec(t *testing.T) {
	spec := plan.TaskSpec{SchemaVersion: 1, CriterionIDs: []string{"AC-1"}, Deliverable: "d", Verification: []plan.VerificationStep{}}
	msg := implementationTaskMessage{TaskID: "t", TaskSpec: &spec, Criteria: testBriefCriteria().Criteria[:1]}
	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	var back struct {
		TaskSpec json.RawMessage  `json:"task_spec"`
		Criteria []plan.Criterion `json:"criteria"`
	}
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatal(err)
	}
	if _, err := plan.ParseTaskSpec(back.TaskSpec); err != nil {
		t.Fatalf("task_spec on the wire: %v (%s)", err, data)
	}
	if len(back.Criteria) != 1 || back.Criteria[0].ID != "AC-1" {
		t.Fatalf("criteria on the wire: %s", data)
	}
	legacy, _ := json.Marshal(implementationTaskMessage{TaskID: "t"})
	if strings.Contains(string(legacy), "task_spec") || strings.Contains(string(legacy), `"criteria"`) {
		t.Errorf("fields are additive: %s", legacy)
	}
}

func TestStructuredBriefReadsCriteria(t *testing.T) {
	data, _ := json.Marshal(map[string]any{"brief": "b", "criteria": testBriefCriteria()})
	var b structuredBrief
	if err := json.Unmarshal(data, &b); err != nil {
		t.Fatal(err)
	}
	got, err := briefCriteriaOf(b)
	if err != nil || got == nil || len(got.Criteria) != 2 {
		t.Fatalf("got %+v, %v", got, err)
	}
	b.Criteria.Criteria[0].Risk = "extreme"
	if _, err := briefCriteriaOf(b); err == nil {
		t.Fatal("invalid criteria must be reported")
	}
	if got, err := briefCriteriaOf(structuredBrief{}); got != nil || err != nil {
		t.Fatal("a legacy brief has no criteria and no error")
	}
}
