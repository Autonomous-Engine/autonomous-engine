package tools

import (
	"encoding/json"
	"sort"
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/llm/adapter"
)

func toolNames(ts []adapter.Tool) []string {
	out := make([]string, 0, len(ts))
	for _, t := range ts {
		out = append(out, t.Name)
	}
	sort.Strings(out)
	return out
}

var researchBase = []string{
	"codex_dependencies_of", "codex_dependents_of", "codex_find_module", "codex_grep",
	"codex_list_tree", "codex_patterns_for", "codex_read_file", "codex_similar_code",
}

var researchLSP = []string{
	"codex_diagnostics", "codex_document_symbols", "codex_find_references",
	"codex_goto_definition", "codex_type_info", "codex_workspace_symbols",
}

func TestResearchToolsForScopesByTypeAndReadiness(t *testing.T) {
	plus := func(extra ...string) []string {
		out := append(append([]string(nil), researchBase...), extra...)
		sort.Strings(out)
		return out
	}
	cases := []struct {
		typ  string
		lsp  bool
		want []string
	}{
		{"code_trace", false, plus()},
		{"pattern_match", false, plus()},
		{"impact_assessment", false, plus()},
		{"", false, plus()},
		{"history_analysis", false, plus("git_log")},
		{"bug_archaeology", false, plus("git_log", "git_diff")},
		{"code_trace", true, plus(researchLSP...)},
		{"bug_archaeology", true, plus(append([]string{"git_log", "git_diff"}, researchLSP...)...)},
	}
	for _, c := range cases {
		got := toolNames(ResearchToolsFor(c.typ, ResearchToolOptions{LSPReady: c.lsp}))
		if strings.Join(got, ",") != strings.Join(c.want, ",") {
			t.Errorf("%s lsp=%v: got %v, want %v", c.typ, c.lsp, got, c.want)
		}
	}
}

func TestResearchToolsForNeverOffersDeadOrDuplicateTools(t *testing.T) {
	never := []string{"request_user_input", "past_decisions_query", "recall_context",
		"read_file", "search_code", "list_files", "codex_verify_imports", "codex_contract_edges"}
	for _, typ := range []string{"code_trace", "history_analysis", "pattern_match", "impact_assessment", "bug_archaeology", "other"} {
		for _, ready := range []bool{false, true} {
			for _, tl := range ResearchToolsFor(typ, ResearchToolOptions{LSPReady: ready}) {
				for _, n := range never {
					if tl.Name == n {
						t.Errorf("%s lsp=%v offers %s", typ, ready, n)
					}
				}
			}
		}
	}
}

// The base research set is sent on every research call; it must stay small.
func TestResearchBaseToolSchemasFitTheBudget(t *testing.T) {
	b, err := json.Marshal(ResearchToolsFor("code_trace", ResearchToolOptions{}))
	if err != nil {
		t.Fatal(err)
	}
	if len(b) > 8000 {
		t.Fatalf("base research tool schemas are %d chars of JSON, want <= 8000", len(b))
	}
	t.Logf("base research tool schemas: %d chars (~%d tokens)", len(b), len(b)/4)
}

// ResearchTools stays the union for any other caller.
func TestResearchToolsRemainsTheUnion(t *testing.T) {
	union := map[string]bool{}
	for _, tl := range ResearchTools() {
		union[tl.Name] = true
	}
	for _, typ := range []string{"code_trace", "history_analysis", "bug_archaeology"} {
		for _, tl := range ResearchToolsFor(typ, ResearchToolOptions{LSPReady: true}) {
			if !union[tl.Name] {
				t.Errorf("ResearchTools() lacks %s offered by ResearchToolsFor(%s)", tl.Name, typ)
			}
		}
	}
}
