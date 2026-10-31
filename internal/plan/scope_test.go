package plan

import (
	"reflect"
	"testing"
)

func TestScopeFilesUnionsStepFiles(t *testing.T) {
	spec := &TaskSpec{Files: []FileRef{
		{Path: "src/server/index.ts", Evidence: EvidenceRef{Kind: EvidenceRepo, Ref: RefStat}},
		{Path: "src/config/env.ts", Evidence: EvidenceRef{Kind: EvidenceCodex, Ref: "call_3"}},
		{Path: "public/index.html", Evidence: EvidenceRef{Kind: EvidenceRepo, Ref: RefStat}},
		{Path: "docs/new-page.md", Evidence: EvidenceRef{Kind: EvidenceNew}},
		{Path: "src/ghost.ts", Evidence: EvidenceRef{Kind: EvidenceHypothesis, Note: "grounding: not found in the repository clone"}},
		{Path: "src/unchecked.ts", Evidence: EvidenceRef{Kind: EvidenceHypothesis, Note: "repository has no clone path; not verified"}},
	}}
	got := ScopeFiles([]string{"./src/server/index.ts", "README.md"}, spec)
	want := []string{"src/server/index.ts", "README.md", "src/config/env.ts", "public/index.html", "docs/new-page.md", "src/unchecked.ts"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ScopeFiles = %v, want %v", got, want)
	}
}

func TestScopeFilesNilSpec(t *testing.T) {
	if got := ScopeFiles([]string{"a.go", "a.go", " "}, nil); !reflect.DeepEqual(got, []string{"a.go"}) {
		t.Fatalf("ScopeFiles(nil spec) = %v", got)
	}
	if got := ScopeFiles(nil, nil); len(got) != 0 {
		t.Fatalf("ScopeFiles(nil, nil) = %v", got)
	}
}

func TestStepFilesExcludesGroundingMisses(t *testing.T) {
	spec := &TaskSpec{Files: []FileRef{
		{Path: "a.ts", Evidence: EvidenceRef{Kind: EvidenceRepo}},
		{Path: "b.ts", Evidence: EvidenceRef{Kind: EvidenceHypothesis, Note: "grounding: path is outside the repository"}},
	}}
	if got := StepFiles(spec); !reflect.DeepEqual(got, []string{"a.ts"}) {
		t.Fatalf("StepFiles = %v", got)
	}
}
