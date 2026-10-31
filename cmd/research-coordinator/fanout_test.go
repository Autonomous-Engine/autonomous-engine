package main

import (
	"reflect"
	"strings"
	"testing"
)

func TestSelectResearchTypes(t *testing.T) {
	noEnv := func(string) string { return "" }
	cases := []struct {
		name     string
		workType string
		seeds    []string
		criteria int
		env      map[string]string
		want     []string
	}{
		{"bug, seeds in one directory", "bug", []string{"cmd/api/a.go", "cmd/api/b.go"}, 2, nil, []string{"code_trace"}},
		{"bug, one root file", "bug", []string{"main.go"}, 0, nil, []string{"code_trace"}},
		{"bug, seeds span two top-level directories", "bug", []string{"cmd/api/a.go", "internal/db/b.go"}, 1, nil, []string{"code_trace", "impact_assessment"}},
		{"bug, root file and a directory", "bug", []string{"main.go", "internal/db/b.go"}, 1, nil, []string{"code_trace", "impact_assessment"}},
		{"bug, the same file mirrored in two trees", "bug", []string{"public/index.html", "landing/index.html"}, 3, nil, []string{"code_trace"}},
		{"bug, seeds, more than 3 criteria", "bug", []string{"cmd/api/a.go"}, 4, nil, []string{"code_trace", "impact_assessment"}},
		{"bug, seeds, exactly 3 criteria", "bug", []string{"cmd/api/a.go"}, 3, nil, []string{"code_trace"}},
		{"bug without seeds", "bug", nil, 0, nil, []string{"code_trace", "bug_archaeology", "impact_assessment"}},
		{"bug without seeds, many criteria", "bug", nil, 9, nil, []string{"code_trace", "bug_archaeology", "impact_assessment"}},
		{"feature with seeds", "feature", []string{"a/b.go"}, 0, nil, []string{"impact_assessment", "pattern_match", "code_trace"}},
		{"feature without seeds", "feature", nil, 5, nil, []string{"impact_assessment", "pattern_match", "code_trace"}},
		{"task (other types as today)", "task", []string{"a/b.go"}, 0, nil, []string{"impact_assessment", "pattern_match", "code_trace", "history_analysis", "bug_archaeology"}},
		{"empty work type", "", nil, 0, nil, []string{"impact_assessment", "pattern_match", "code_trace", "history_analysis", "bug_archaeology"}},
		{"override for bug", "bug", []string{"a/b.go"}, 0, map[string]string{"RESEARCH_TASK_TYPES_BUG": "code_trace, history_analysis"}, []string{"code_trace", "history_analysis"}},
		{"override drops unknown and duplicate types", "feature", nil, 0, map[string]string{"RESEARCH_TASK_TYPES_FEATURE": "pattern_match,bogus,,pattern_match, code_trace"}, []string{"pattern_match", "code_trace"}},
		{"override with nothing valid is ignored", "bug", nil, 0, map[string]string{"RESEARCH_TASK_TYPES_BUG": "nope, ,"}, []string{"code_trace", "bug_archaeology", "impact_assessment"}},
		{"override for another type does not apply", "bug", nil, 0, map[string]string{"RESEARCH_TASK_TYPES_FEATURE": "pattern_match"}, []string{"code_trace", "bug_archaeology", "impact_assessment"}},
		{"override key normalises the work type", "sub-task", nil, 0, map[string]string{"RESEARCH_TASK_TYPES_SUB_TASK": "code_trace"}, []string{"code_trace"}},
		{"bug, same file name in different subtrees spans", "bug", []string{"cmd/a/main.go", "internal/b/main.go"}, 0, nil, []string{"code_trace", "impact_assessment"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			getenv := noEnv
			if tc.env != nil {
				getenv = func(k string) string { return tc.env[k] }
			}
			got := selectResearchTypes(tc.workType, tc.seeds, true, tc.criteria, getenv)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("selectResearchTypes(%q, %v, %d) = %v, want %v", tc.workType, tc.seeds, tc.criteria, got, tc.want)
			}
		})
	}
}

// Unverified seeds do not narrow a bug: it keeps the no-seed set. A feature
// is unaffected.
func TestSelectResearchTypes_UnverifiedSeeds(t *testing.T) {
	noEnv := func(string) string { return "" }
	if got, want := selectResearchTypes("bug", []string{"a/b.go"}, false, 0, noEnv), []string{"code_trace", "bug_archaeology", "impact_assessment"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("bug with unverified seeds = %v, want %v", got, want)
	}
	if got, want := selectResearchTypes("feature", []string{"a/b.go"}, false, 0, noEnv), []string{"impact_assessment", "pattern_match", "code_trace"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("feature with unverified seeds = %v, want %v", got, want)
	}
}

func TestSeedsSpanTopLevelDirs(t *testing.T) {
	cases := []struct {
		seeds []string
		want  bool
	}{
		{[]string{"public/index.html", "landing/index.html"}, false}, // mirrored edit
		{[]string{"cmd/a/main.go", "internal/b/main.go"}, true},      // same name, different paths
		{[]string{"cmd/a/x.go", "cmd/b/y.go"}, false},                // one top-level dir
		{[]string{"main.go", "internal/main.go"}, false},             // root file mirrored as internal/main.go
		{[]string{"main.go", "internal/db/b.go"}, true},
		{[]string{"a/x/y.ts", "b/x/y.ts", "c/x/y.ts"}, false},
		{[]string{"a/x/y.ts", "b/x/y.ts", "c/x/z.ts"}, true},
	}
	for _, tc := range cases {
		if got := seedsSpanTopLevelDirs(tc.seeds); got != tc.want {
			t.Errorf("seedsSpanTopLevelDirs(%v) = %v, want %v", tc.seeds, got, tc.want)
		}
	}
}

func TestCountAcceptanceCriteria(t *testing.T) {
	cases := []struct {
		name string
		prd  string
		want int
	}{
		{"none", "# PRD\n\nJust prose.\n", 0},
		{"markdown heading, dashes", "## Acceptance Criteria\n- a\n- b\n\n## Notes\n- not counted\n", 2},
		{"numbered and checkboxes", "### Acceptance criteria\n1. a\n2) b\n- [ ] c\n* d\n", 4},
		{"bold heading with colon", "**Acceptance Criteria:**\n- a\n- b\n- c\n- d\n**Risks:**\n- r\n", 4},
		{"plain label", "Acceptance criteria:\n- a\n\nOther text\n- b\n", 2},
		{"narrow intake PRD", narrowPRD, 3},
		{"CRLF", strings.ReplaceAll("## Acceptance Criteria\n- a\n- b\n## Next\n- c\n", "\n", "\r\n"), 2},
		{"nested items are not separate criteria", "## Acceptance Criteria\n- a\n  - detail\n- b\n", 2},
		// R7: the live-run PRD shapes (attempts 5-7).
		{"attempt 7: bullets under lettered sub-headings", attempt7PRD, 8},
		{"attempt 6: numbered, bold labels", attempt6PRD, 9},
		{"attempt 5: numbered groups with nested bullets", attempt5PRD, 7},
		{"AC id lines without a list marker", "## Acceptance Criteria\nAC-1: a\nAC-2: b\n**AC-3**: c\n\n## Notes\nAC-4: not counted\n", 3},
		{"an indented continuation starting with an AC id is not a criterion", "## Acceptance Criteria\n1. Login\n   AC-1 covers the happy path\n2. Logout\n", 2},
		{"AC id bullets count once", "## Acceptance Criteria\n- AC-1: a\n- **AC-2**: b\n", 2},
		{"a sibling heading ends the section", "## Acceptance Criteria\n- a\n## Open Questions\n- q\n### Review Findings\n1. r\n", 1},
		{"a sub-heading under a level-3 section ends at the next level-3", "### Acceptance criteria\n#### Group\n- a\n### Risks\n- r\n", 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := countAcceptanceCriteria(tc.prd); got != tc.want {
				t.Fatalf("countAcceptanceCriteria = %d, want %d", got, tc.want)
			}
		})
	}
}
