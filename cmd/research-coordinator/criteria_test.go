package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/evidence"
	"github.com/autonomousengine/autonomous-engine/internal/plan"
)

func repoEv(ref string, conf float64) plan.EvidenceRef {
	return plan.EvidenceRef{Kind: plan.EvidenceRepo, Ref: ref, Confidence: conf, Basis: plan.BasisClone}
}

func TestMergeBriefCriteria(t *testing.T) {
	r1 := researchResultMessage{Status: ResearchStatusOK, Criteria: &plan.BriefCriteria{SchemaVersion: 1, Criteria: []plan.Criterion{
		{
			ID: "AC-1", Text: "The form rejects an empty email.",
			Source: plan.Source{Kind: plan.SourceDerived, Reason: "validation convention"},
			Files:  []plan.FileRef{{Path: "src/Contact.tsx", Evidence: plan.EvidenceRef{Kind: plan.EvidenceHypothesis}}},
			Risk:   plan.RiskLow, RequiredChecks: []evidence.CheckKind{evidence.CheckKindTest},
		},
		{ID: "AC-2", Text: "Submissions are stored", Source: plan.Source{Kind: plan.SourceUser, Quote: "store them"}, Risk: plan.RiskMedium},
	}}}
	r2 := researchResultMessage{Status: ResearchStatusOK, Criteria: &plan.BriefCriteria{SchemaVersion: 1, Criteria: []plan.Criterion{
		{
			ID: "AC-1", Text: "the form REJECTS an empty email",
			Source: plan.Source{Kind: plan.SourceUser, Quote: "must not accept an empty email"},
			Files: []plan.FileRef{
				{Path: "src/Contact.tsx", Evidence: repoEv("call_1", 0.9)},
				{Path: "src/validate.ts", Evidence: plan.EvidenceRef{Kind: plan.EvidenceNew, Confidence: 0.5}},
			},
			Symbols: []plan.SymbolRef{{Name: "ContactForm", Evidence: plan.EvidenceRef{Kind: plan.EvidenceLSP, Ref: "call_2", Confidence: 0.8}}},
			Risk:    plan.RiskHigh, RequiredChecks: []evidence.CheckKind{evidence.CheckKindBuild, evidence.CheckKindTest},
		},
		{ID: "AC-2", Text: "Mobile layout works", Source: plan.Source{Kind: plan.SourceDerived, Reason: "r"}, Risk: plan.RiskLow},
	}}}
	failed := researchResultMessage{Status: ResearchStatusError, Criteria: &plan.BriefCriteria{SchemaVersion: 1, Criteria: []plan.Criterion{
		{ID: "AC-1", Text: "From a failed task", Source: plan.Source{Kind: plan.SourceDerived, Reason: "r"}, Risk: plan.RiskLow},
	}}}
	legacy := researchResultMessage{Status: ResearchStatusOK}

	got := mergeBriefCriteria([]researchResultMessage{r1, legacy, failed, r2})
	if got == nil {
		t.Fatal("want merged criteria")
	}
	if err := got.Validate(); err != nil {
		t.Fatal(err)
	}
	if len(got.Criteria) != 3 {
		t.Fatalf("want 3 criteria (dedupe by normalised text, failed task excluded), got %+v", got.Criteria)
	}
	for i, c := range got.Criteria {
		if c.ID != []string{"AC-1", "AC-2", "AC-3"}[i] {
			t.Errorf("ids renumbered in first-seen order, got %s at %d", c.ID, i)
		}
	}
	ac1 := got.Criteria[0]
	if ac1.Text != "The form rejects an empty email." {
		t.Errorf("first-seen text kept: %q", ac1.Text)
	}
	if ac1.Source.Kind != plan.SourceUser || ac1.Source.Quote == "" {
		t.Errorf("user source with quote wins: %+v", ac1.Source)
	}
	if ac1.Risk != plan.RiskHigh {
		t.Errorf("highest risk wins: %s", ac1.Risk)
	}
	if plan.CheckList(ac1.RequiredChecks) != "test, build" {
		t.Errorf("checks unioned: %v", ac1.RequiredChecks)
	}
	if len(ac1.Files) != 2 || ac1.Files[0].Evidence.Kind != plan.EvidenceRepo || ac1.Files[0].Evidence.Ref != "call_1" {
		t.Errorf("richest file evidence kept: %+v", ac1.Files)
	}
	if len(ac1.Symbols) != 1 {
		t.Errorf("symbols unioned: %+v", ac1.Symbols)
	}
	if got.FileEvidence["src/Contact.tsx"].Kind != plan.EvidenceRepo || got.FileEvidence["src/validate.ts"].Kind != plan.EvidenceNew {
		t.Errorf("file evidence map: %+v", got.FileEvidence)
	}
	// The inputs are not mutated by the merge.
	if r1.Criteria.Criteria[0].Files[0].Evidence.Kind != plan.EvidenceHypothesis {
		t.Error("merge mutated its input")
	}
}

func userCrit(text, quote string, risk plan.Risk) plan.Criterion {
	return plan.Criterion{ID: "AC-1", Text: text, Source: plan.Source{Kind: plan.SourceUser, Quote: quote}, Risk: risk}
}

func okResult(findingType, taskID string, cs ...plan.Criterion) researchResultMessage {
	for i := range cs {
		cs[i].ID = fmt.Sprintf("AC-%d", i+1)
	}
	return researchResultMessage{Status: ResearchStatusOK, FindingType: findingType, TaskID: taskID,
		Criteria: &plan.BriefCriteria{SchemaVersion: 1, Criteria: cs}}
}

func TestMergeBriefCriteriaSameQuote(t *testing.T) {
	quote := "A $100 item with a $10 fixed discount and 20% off must come to $72 before tax"
	// Across results, strongly overlapping texts with the same quote merge.
	got := mergeBriefCriteria([]researchResultMessage{
		okResult("code_trace", "a", userCrit("The calculator applies the fixed discount before the percentage discount", quote, plan.RiskMedium)),
		okResult("impact_assessment", "b", userCrit("The calculator applies the fixed discount before the percentage discount first", quote, plan.RiskHigh)),
	})
	if len(got.Criteria) != 1 || got.Criteria[0].Risk != plan.RiskHigh {
		t.Fatalf("near-identical texts quoting the same words merge (Jaccard >= 0.6): %+v", got.Criteria)
	}
	// Across results, the same quote with weakly overlapping texts stays apart.
	got = mergeBriefCriteria([]researchResultMessage{
		okResult("code_trace", "a", userCrit("The calculator applies the fixed discount first", quote, plan.RiskMedium)),
		okResult("impact_assessment", "b", userCrit("For price 100, fixed 10, 20% the subtotal is 72", quote, plan.RiskHigh)),
	})
	if len(got.Criteria) != 2 {
		t.Fatalf("weak overlap is not merged on the quote alone: %+v", got.Criteria)
	}
}

// One quoted PRD sentence can carry two distinct requirements; a task that
// states both must keep both, and a second task repeating one of them merges
// into that one only.
func TestMergeBriefCriteriaOneQuoteTwoRequirements(t *testing.T) {
	quote := "Orders over $50 ship free and the total shows the discount line"
	a := okResult("code_trace", "a",
		userCrit("Orders over $50 ship free", quote, plan.RiskMedium),
		userCrit("The order total shows the discount line", quote, plan.RiskLow),
	)
	b := okResult("pattern_match", "b",
		userCrit("Orders over $50 ship free of charge", quote, plan.RiskHigh),
	)
	got := mergeBriefCriteria([]researchResultMessage{b, a})
	if len(got.Criteria) != 2 {
		t.Fatalf("both requirements survive: %+v", got.Criteria)
	}
	texts := got.Criteria[0].Text + " | " + got.Criteria[1].Text
	if !strings.Contains(texts, "ship free") || !strings.Contains(texts, "discount line") {
		t.Fatalf("got %s", texts)
	}
	if got.Criteria[0].Risk != plan.RiskHigh {
		t.Fatalf("the repeated requirement merged across results: %+v", got.Criteria[0])
	}
}

func TestMergeBriefCriteriaOrderIsReproducible(t *testing.T) {
	r1 := okResult("pattern_match", "b", userCrit("Second requirement", "q2", plan.RiskLow))
	r2 := okResult("code_trace", "a", userCrit("First requirement", "q1", plan.RiskLow))
	r3 := okResult("code_trace", "c", userCrit("Third requirement", "q3", plan.RiskLow))
	x := mergeBriefCriteria([]researchResultMessage{r1, r2, r3})
	y := mergeBriefCriteria([]researchResultMessage{r3, r1, r2})
	for i, want := range []string{"First requirement", "Third requirement", "Second requirement"} {
		if x.Criteria[i].Text != want || y.Criteria[i].Text != want || x.Criteria[i].ID != y.Criteria[i].ID {
			t.Fatalf("AC numbering follows (FindingType, TaskID): %+v / %+v", x.Criteria, y.Criteria)
		}
	}
}

func TestTokenJaccard(t *testing.T) {
	if j := tokenJaccard("a b c", "a b d"); j != 0.5 {
		t.Fatalf("got %v", j)
	}
	if tokenJaccard("", "") != 1 || tokenJaccard("a", "") != 0 {
		t.Fatal("edge cases")
	}
}

func TestMergeBriefCriteriaLegacy(t *testing.T) {
	if mergeBriefCriteria([]researchResultMessage{{Status: ResearchStatusOK}}) != nil {
		t.Fatal("no criteria on any result → none on the brief")
	}
}

func TestTechnicalBriefCarriesCriteria(t *testing.T) {
	c := &plan.BriefCriteria{SchemaVersion: 1, Criteria: []plan.Criterion{{ID: "AC-1", Text: "t", Source: plan.Source{Kind: plan.SourceDerived, Reason: "r"}, Risk: plan.RiskLow}}}
	art, _ := json.Marshal(technicalBrief{Brief: "b", Narrative: "b", Criteria: c})
	msg, _ := json.Marshal(technicalBriefReadyMessage{WorkID: "w", Criteria: c})
	for _, data := range [][]byte{art, msg} {
		if !strings.Contains(string(data), `"criteria":{"schema_version":1,"criteria":[{"id":"AC-1"`) {
			t.Errorf("criteria missing from %s", data)
		}
	}
	legacy, _ := json.Marshal(technicalBriefReadyMessage{WorkID: "w"})
	if strings.Contains(string(legacy), "criteria") {
		t.Errorf("criteria must be omitted when absent: %s", legacy)
	}
}
