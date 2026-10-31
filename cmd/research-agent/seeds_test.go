package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestSeedsSection_DecodesCoordinatorPayload(t *testing.T) {
	raw := `{"task_id":"t","work_id":"w","research_type":"code_trace","starting_context":"PRD",
		"seed_files":["public/index.html","landing/index.html"],
		"seed_lines":{"public/index.html":[840,848],"landing/index.html":[855]},
		"budget_hint_tokens":120000}`
	var task researchTaskMessage
	if err := json.Unmarshal([]byte(raw), &task); err != nil {
		t.Fatalf("decode: %v", err)
	}
	got := seedsSection(task)
	for _, want := range []string{
		"Start here (named in the intake):",
		"- public/index.html (lines 840, 848)",
		"- landing/index.html (line 855)",
		"Read these first with codex_read_file; do not search for them.",
		"Budget for this task: about 120000 tokens (about 12 calls).",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("seedsSection lacks %q:\n%s", want, got)
		}
	}
	if strings.Index(got, "public/index.html") > strings.Index(got, "landing/index.html") {
		t.Error("seed order not preserved")
	}
	if strings.Contains(got, "not verified") {
		t.Error("verified seeds described as unverified")
	}
}

func TestSeedsSection_Variants(t *testing.T) {
	zero := int64(0)
	small := int64(4000)
	cases := []struct {
		name    string
		task    researchTaskMessage
		want    []string
		notWant []string
	}{
		{"legacy task: nothing", researchTaskMessage{}, nil, []string{"Start here", "Budget"}},
		{"seed without lines", researchTaskMessage{seedPayload: seedPayload{SeedFiles: []string{"main.go"}}}, []string{"- main.go\n"}, []string{"(line", "Budget"}},
		{"unverified seeds", researchTaskMessage{seedPayload: seedPayload{SeedFiles: []string{"a/b.go"}, SeedsUnverified: true}}, []string{"not verified", "say so"}, nil},
		{"budget only", researchTaskMessage{seedPayload: seedPayload{BudgetHintTokens: &small}}, []string{"Budget for this task: about 4000 tokens (about 1 call)."}, []string{"Start here"}},
		{"budget exhausted", researchTaskMessage{seedPayload: seedPayload{BudgetHintTokens: &zero}}, []string{"Budget for this task: about 0 tokens (about 0 calls)"}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := seedsSection(tc.task)
			if len(tc.want) == 0 && got != "" {
				t.Fatalf("legacy task got a section: %q", got)
			}
			for _, w := range tc.want {
				if !strings.Contains(got, w) {
					t.Errorf("lacks %q:\n%s", w, got)
				}
			}
			for _, w := range tc.notWant {
				if strings.Contains(got, w) {
					t.Errorf("has %q:\n%s", w, got)
				}
			}
		})
	}
}

// The user message carries the section: the one-line insertion in main.go
// is present (CRLF-normalised source scan).
func TestSeedsSection_InUserMessage(t *testing.T) {
	src := strings.ReplaceAll(stripComments(agentSources(t)["main.go"]), "\r\n", "\n")
	if !strings.Contains(src, "userMessage += seedsSection(task)") {
		t.Fatal("main.go does not append seedsSection(task) to the user message")
	}
	if !strings.Contains(src, "\tseedPayload\n") {
		t.Fatal("researchTaskMessage does not embed seedPayload")
	}
}
