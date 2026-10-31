package plan

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/evidence"
)

func sampleBrief() BriefCriteria {
	return BriefCriteria{
		SchemaVersion: SchemaVersion,
		Criteria: []Criterion{
			{
				ID:     "AC-1",
				Text:   "The contact form rejects an empty email address",
				Source: Source{Kind: SourceUser, Quote: "the form must not accept an empty email"},
				Files: []FileRef{
					{Path: "src/components/Contact.tsx", Evidence: EvidenceRef{Kind: EvidenceRepo, Ref: "call_3", Confidence: 0.9, Basis: BasisClone}},
					{Path: "src/lib/validate.ts", Evidence: EvidenceRef{Kind: EvidenceNew, Confidence: 0.6}},
				},
				Symbols:        []SymbolRef{{Name: "ContactForm", File: "src/components/Contact.tsx", Line: 12, Evidence: EvidenceRef{Kind: EvidenceLSP, Ref: "call_5", Confidence: 0.8}}},
				Contracts:      []ContractRef{{Type: "rest_openapi", Name: "POST /api/contact", Evidence: EvidenceRef{Kind: EvidenceCodex, Ref: "call_6", Confidence: 0.7}}},
				Risk:           RiskMedium,
				RequiredChecks: []evidence.CheckKind{evidence.CheckKindTest, evidence.CheckKindBuild},
			},
			{
				ID:     "AC-2",
				Text:   "Existing submissions keep working",
				Source: Source{Kind: SourceDerived, Reason: "the handler is shared with the newsletter form"},
				Files:  []FileRef{{Path: "src/api/contact.ts", Evidence: EvidenceRef{Kind: EvidenceHypothesis, Note: "not found in the clone"}}},
				Risk:   RiskLow,
			},
		},
	}
}

func TestBriefCriteriaRoundTrip(t *testing.T) {
	b := sampleBrief()
	b.FileEvidence = BuildFileEvidence(b.Criteria)
	data, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParseBriefCriteria(data)
	if err != nil {
		t.Fatalf("parse: %v\n%s", err, data)
	}
	if !reflect.DeepEqual(*got, b) {
		t.Fatalf("round trip changed the value:\n got %+v\nwant %+v", *got, b)
	}
	for _, key := range []string{`"schema_version":1`, `"required_checks":["test","build"]`, `"file_evidence"`, `"basis":"clone"`} {
		if !strings.Contains(string(data), key) {
			t.Errorf("wire form lacks %s: %s", key, data)
		}
	}
}

func TestTaskSpecRoundTrip(t *testing.T) {
	ts := TaskSpec{
		SchemaVersion: SchemaVersion,
		CriterionIDs:  []string{"AC-1", "AC-2"},
		Deliverable:   "Contact form validates email before submit",
		Verification: []VerificationStep{
			{CheckKind: evidence.CheckKindTest, Command: "npm test -- contact"},
			{CheckKind: evidence.CheckKindOther, Description: "submit with an empty email shows the error"},
		},
		Files: []FileRef{{Path: "src/components/Contact.tsx", Evidence: EvidenceRef{Kind: EvidenceRepo, Ref: RefStat, Confidence: 1, Basis: BasisClone}}},
	}
	data, err := json.Marshal(ts)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParseTaskSpec(data)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !reflect.DeepEqual(*got, ts) {
		t.Fatalf("round trip: got %+v want %+v", *got, ts)
	}
}

func TestBriefCriteriaValidateRejects(t *testing.T) {
	cases := map[string]func(b *BriefCriteria){
		"schema version":       func(b *BriefCriteria) { b.SchemaVersion = 2 },
		"duplicate id":         func(b *BriefCriteria) { b.Criteria[1].ID = "AC-1" },
		"bad id":               func(b *BriefCriteria) { b.Criteria[0].ID = "C1" },
		"zero id":              func(b *BriefCriteria) { b.Criteria[0].ID = "AC-0" },
		"empty text":           func(b *BriefCriteria) { b.Criteria[0].Text = " " },
		"user without quote":   func(b *BriefCriteria) { b.Criteria[0].Source.Quote = "" },
		"derived no reason":    func(b *BriefCriteria) { b.Criteria[1].Source.Reason = "" },
		"unknown source":       func(b *BriefCriteria) { b.Criteria[0].Source.Kind = "llm" },
		"unknown risk":         func(b *BriefCriteria) { b.Criteria[0].Risk = "severe" },
		"missing risk":         func(b *BriefCriteria) { b.Criteria[0].Risk = "" },
		"unknown check":        func(b *BriefCriteria) { b.Criteria[0].RequiredChecks = []evidence.CheckKind{"fuzz"} },
		"unknown evidence":     func(b *BriefCriteria) { b.Criteria[0].Files[0].Evidence.Kind = "grep" },
		"confidence above one": func(b *BriefCriteria) { b.Criteria[0].Files[0].Evidence.Confidence = 1.5 },
		"confidence negative":  func(b *BriefCriteria) { b.Criteria[0].Files[0].Evidence.Confidence = -0.1 },
		"repo without ref":     func(b *BriefCriteria) { b.Criteria[0].Files[0].Evidence.Ref = "" },
		"user evidence on file": func(b *BriefCriteria) {
			b.Criteria[0].Files[0].Evidence = EvidenceRef{Kind: EvidenceUser, Ref: "quote", Confidence: 1}
		},
		"empty path":       func(b *BriefCriteria) { b.Criteria[0].Files[0].Path = "" },
		"unknown basis":    func(b *BriefCriteria) { b.Criteria[0].Files[0].Evidence.Basis = "remote" },
		"new symbol":       func(b *BriefCriteria) { b.Criteria[0].Symbols[0].Evidence = EvidenceRef{Kind: EvidenceNew} },
		"lsp without ref":  func(b *BriefCriteria) { b.Criteria[0].Symbols[0].Evidence.Ref = "" },
		"contract no name": func(b *BriefCriteria) { b.Criteria[0].Contracts[0].Name = "" },
		"file evidence map": func(b *BriefCriteria) {
			b.FileEvidence = map[string]EvidenceRef{"a.go": {Kind: "maybe"}}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			b := sampleBrief()
			mutate(&b)
			if err := b.Validate(); err == nil {
				t.Fatalf("Validate accepted %s", name)
			}
		})
	}
	if err := sampleBrief().Validate(); err != nil {
		t.Fatalf("sample brief should validate: %v", err)
	}
}

func TestTaskSpecValidate(t *testing.T) {
	ok := TaskSpec{SchemaVersion: 1, Deliverable: "x"}
	if err := ok.Validate(); err != nil {
		t.Fatalf("a task without criterion ids is structurally valid (it is a hypothesis): %v", err)
	}
	bad := []TaskSpec{
		{SchemaVersion: 0},
		{SchemaVersion: 1, CriterionIDs: []string{"AC-1", "AC-1"}},
		{SchemaVersion: 1, CriterionIDs: []string{"ac-1"}},
		{SchemaVersion: 1, Verification: []VerificationStep{{CheckKind: "smoke", Command: "x"}}},
		{SchemaVersion: 1, Verification: []VerificationStep{{CheckKind: evidence.CheckKindTest}}},
		{SchemaVersion: 1, Files: []FileRef{{Path: "a", Evidence: EvidenceRef{Kind: "guess"}}}},
	}
	for i, ts := range bad {
		if err := ts.Validate(); err == nil {
			t.Errorf("case %d: Validate accepted %+v", i, ts)
		}
	}
}

func TestParseRejectsUnknownKindsOnTheWire(t *testing.T) {
	raw := `{"schema_version":1,"criteria":[{"id":"AC-1","text":"t","source":{"kind":"user","quote":"q"},"risk":"low","files":[{"path":"a.go","evidence":{"kind":"vibes","confidence":0.5}}]}]}`
	if _, err := ParseBriefCriteria([]byte(raw)); err == nil || !strings.Contains(err.Error(), "vibes") {
		t.Fatalf("want an unknown-kind error naming the kind, got %v", err)
	}
	if _, err := ParseBriefCriteria([]byte(`{`)); err == nil {
		t.Fatal("want a decode error")
	}
}

func TestKnownCheckKindFollowsEvidence(t *testing.T) {
	for _, k := range []evidence.CheckKind{evidence.CheckKindTest, evidence.CheckKindDiagnostic, evidence.CheckKindBuild, evidence.CheckKindLint, evidence.CheckKindOther} {
		if !KnownCheckKind(k) {
			t.Errorf("%s should be known", k)
		}
	}
	if KnownCheckKind("") || KnownCheckKind("vibes") {
		t.Error("unknown kinds must be rejected")
	}
}

func TestNormalizeText(t *testing.T) {
	a := NormalizeText("  The form REJECTS an empty e-mail. ")
	b := NormalizeText("the form rejects an empty e mail")
	if a != b || a != "the form rejects an empty e mail" {
		t.Fatalf("got %q and %q", a, b)
	}
}

func TestBuildFileEvidenceKeepsStrongest(t *testing.T) {
	cs := []Criterion{
		{Files: []FileRef{{Path: "a.go", Evidence: EvidenceRef{Kind: EvidenceHypothesis}}}},
		{Files: []FileRef{{Path: "a.go", Evidence: EvidenceRef{Kind: EvidenceRepo, Ref: "call_1", Confidence: 0.5}}}},
		{Files: []FileRef{{Path: "a.go", Evidence: EvidenceRef{Kind: EvidenceCodex, Ref: "call_2", Confidence: 0.9}}}},
		{Files: []FileRef{{Path: "b.go", Evidence: EvidenceRef{Kind: EvidenceNew}}}},
	}
	m := BuildFileEvidence(cs)
	if m["a.go"].Ref != "call_2" || m["b.go"].Kind != EvidenceNew {
		t.Fatalf("got %+v", m)
	}
	if BuildFileEvidence(nil) != nil {
		t.Fatal("no files → nil map")
	}
}

func TestEvidenceLabel(t *testing.T) {
	got := EvidenceLabel(EvidenceRef{Kind: EvidenceRepo, Ref: "call_7", Basis: BasisClone})
	if got != "repo call_7 (clone)" {
		t.Fatalf("got %q", got)
	}
	got = EvidenceLabel(EvidenceRef{Kind: EvidenceHypothesis, Note: "not found in the clone"})
	if got != "hypothesis — not found in the clone" {
		t.Fatalf("got %q", got)
	}
}
