package main

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
)

// TestResearchResultMessage_RoundTrip_LimitHit verifies that Status and
// ErrorDetail survive a JSON encode/decode cycle. This is the Kafka wire
// contract: the coordinator decodes what the agent encodes; if the field
// names or JSON tags drift, the coordinator sees empty strings and
// silently treats failed tasks as successes (the exact bug we are fixing).
func TestResearchResultMessage_RoundTrip_LimitHit(t *testing.T) {
	src := researchResultMessage{
		TaskID:      "task-1",
		WorkID:      "work-ee978d47",
		Status:      ResearchStatusLimitHit,
		FindingType: "code_trace",
		Evidence:    "Research tool loop failed: tool loop reached safety limit: 15 iterations",
		Confidence:  0.0,
		ErrorDetail: "tool loop reached safety limit: 15 iterations",
	}

	data, err := json.Marshal(src)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	// Assert the wire tag is literally "status" — not "Status" or similar.
	// The coordinator's decoder keys off this exact tag, and a Go rename
	// that forgets to update the tag would regress silently.
	if !jsonContainsKey(data, `"status":`) {
		t.Fatalf("expected JSON to contain \"status\" key, got %s", string(data))
	}
	if !jsonContainsKey(data, `"error_detail":`) {
		t.Fatalf("expected JSON to contain \"error_detail\" key, got %s", string(data))
	}

	var dst researchResultMessage
	if err := json.Unmarshal(data, &dst); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if dst.Status != ResearchStatusLimitHit {
		t.Errorf("Status round-trip failed: got %q, want %q", dst.Status, ResearchStatusLimitHit)
	}
	if dst.ErrorDetail != src.ErrorDetail {
		t.Errorf("ErrorDetail round-trip failed: got %q, want %q", dst.ErrorDetail, src.ErrorDetail)
	}
}

// TestResearchResultMessage_RoundTrip_OK is the happy-path round-trip —
// the status "ok" must survive and the error_detail must omitempty-drop
// when empty (keeping wire payloads compact).
func TestResearchResultMessage_RoundTrip_OK(t *testing.T) {
	src := researchResultMessage{
		TaskID:      "task-1",
		WorkID:      "work-1",
		Status:      ResearchStatusOK,
		FindingType: "pattern_match",
		Evidence:    "Found 3 matching patterns",
		Confidence:  0.9,
	}
	data, err := json.Marshal(src)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// omitempty keeps error_detail out of the happy-path payload.
	if jsonContainsKey(data, `"error_detail":`) {
		t.Errorf("error_detail should be omitted when empty; got %s", string(data))
	}
	var dst researchResultMessage
	if err := json.Unmarshal(data, &dst); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if dst.Status != ResearchStatusOK {
		t.Errorf("Status round-trip failed: got %q, want ok", dst.Status)
	}
}

// TestResearchResultMessage_RoundTrip_StructuredFields confirms the new
// Affected*/Relevant* fields survive a JSON encode/decode — the contract
// between research-agent and research-coordinator for structured output.
// Without this, a JSON-tag typo would silently drop structured fields and
// downstream agents would fall back to re-extracting from prose (exactly
// the bug we're fixing).
func TestResearchResultMessage_RoundTrip_StructuredFields(t *testing.T) {
	src := researchResultMessage{
		TaskID:               "task-s",
		WorkID:               "work-s",
		Status:               ResearchStatusOK,
		FindingType:          "code_trace",
		Evidence:             "found it",
		Confidence:           0.9,
		AffectedFiles:        []string{"cmd/research-agent/main.go", "internal/llm/tools/executor.go"},
		AffectedModules:      []string{"research-agent", "llm.tools"},
		AffectedZones:        []string{"research"},
		CrossRepoTouchpoints: []string{"autonomous-engine"},
		RelevantPatterns:     []string{"tool-loop", "structured-output"},
		RelevantAntiPatterns: []string{"prose-reextraction"},
	}

	data, err := json.Marshal(src)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// Every new field must appear with its literal snake_case tag. This is
	// the wire contract the coordinator's decoder keys off of.
	for _, key := range []string{
		`"affected_files":`,
		`"affected_modules":`,
		`"affected_zones":`,
		`"cross_repo_touchpoints":`,
		`"relevant_patterns":`,
		`"relevant_anti_patterns":`,
	} {
		if !jsonContainsKey(data, key) {
			t.Errorf("expected JSON to contain %s key, got %s", key, string(data))
		}
	}

	var dst researchResultMessage
	if err := json.Unmarshal(data, &dst); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got, want := len(dst.AffectedFiles), 2; got != want {
		t.Errorf("AffectedFiles len: got %d, want %d", got, want)
	}
	if got, want := len(dst.AffectedModules), 2; got != want {
		t.Errorf("AffectedModules len: got %d, want %d", got, want)
	}
	if got, want := len(dst.AffectedZones), 1; got != want {
		t.Errorf("AffectedZones len: got %d, want %d", got, want)
	}
	if got, want := len(dst.CrossRepoTouchpoints), 1; got != want {
		t.Errorf("CrossRepoTouchpoints len: got %d, want %d", got, want)
	}
	if got, want := len(dst.RelevantPatterns), 2; got != want {
		t.Errorf("RelevantPatterns len: got %d, want %d", got, want)
	}
	if got, want := len(dst.RelevantAntiPatterns), 1; got != want {
		t.Errorf("RelevantAntiPatterns len: got %d, want %d", got, want)
	}
}

// TestResearchResultMessage_OmitEmpty_Structured verifies that the new
// structured fields are omitted from the wire payload when empty. This
// keeps Kafka messages compact for research tasks that produced prose
// evidence but no capturable tool-call signal (e.g. a pure git_log
// task) — and matches the omitempty contract declared on the struct.
func TestResearchResultMessage_OmitEmpty_Structured(t *testing.T) {
	src := researchResultMessage{
		TaskID:      "task-e",
		WorkID:      "work-e",
		Status:      ResearchStatusOK,
		FindingType: "history_analysis",
		Evidence:    "no touched files",
		Confidence:  0.9,
	}
	data, err := json.Marshal(src)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, key := range []string{
		`"affected_files":`,
		`"affected_modules":`,
		`"affected_zones":`,
		`"cross_repo_touchpoints":`,
		`"relevant_patterns":`,
		`"relevant_anti_patterns":`,
	} {
		if jsonContainsKey(data, key) {
			t.Errorf("expected omitempty to drop %s; payload was %s", key, string(data))
		}
	}
}

// TestDeriveStructuredFields_NoDB confirms that given a synthetic tool-call
// history, deriveStructuredFields extracts the right files/modules/patterns
// WITHOUT needing a DB (pool=nil). DB-backed lookups (zones, touchpoints)
// return empty in that case, which is the expected degraded behavior for
// Agent A's codex tools landing without a product context — and the same
// path this test takes.
func TestDeriveStructuredFields_NoDB(t *testing.T) {
	history := []toolCallRecord{
		{tool: "read_file", arguments: `{"path":"cmd/research-agent/main.go"}`, result: "..."},
		{tool: "read_file", arguments: `{"path":"./internal/codex/codex.go"}`, result: "..."},
		{tool: "search_code", result: "cmd/research-agent/main.go:42:var x = 1\ninternal/codex/codex.go:100:type Codex struct"},
		{tool: "codex_find_module", result: `{"modules":[{"name":"research-agent","purpose":"..."},{"name":"codex","purpose":"..."}],"count":2}`},
		{tool: "codex_patterns_for", result: `{"query":"testing","patterns":[{"name":"table-driven-tests"}],"anti_patterns":[{"name":"shared-mutable-state"}]}`},
		{tool: "codex_similar_code", result: `{"results":[{"file":"internal/llm/tools/executor.go","function":"Execute"}],"count":1}`},
		{tool: "codex_dependents_of", result: `{"module":"research-agent","dependents":["research-coordinator"]}`},
	}

	// Pool is nil — zones and cross_repo_touchpoints lookups return nil.
	// productID is uuid.Nil — the same degraded path Agent C can rely on.
	sf := deriveStructuredFields(context.Background(), nil, uuid.Nil, history)

	// Files: two read_files (deduped by path) + two grep hits = 3 unique
	// after dedupe (main.go appears in both read_file and grep, so it's 1).
	wantFiles := []string{
		"cmd/research-agent/main.go",
		"internal/codex/codex.go",
		"internal/llm/tools/executor.go", // from codex_similar_code
	}
	if !stringSlicesEqual(sf.AffectedFiles, wantFiles) {
		t.Errorf("AffectedFiles: got %v, want %v", sf.AffectedFiles, wantFiles)
	}

	// Modules come from codex_find_module (research-agent, codex) plus
	// codex_dependents_of (research-agent, research-coordinator). Union
	// deduped is 3 entries.
	wantModules := []string{"codex", "research-agent", "research-coordinator"}
	if !stringSlicesEqual(sf.AffectedModules, wantModules) {
		t.Errorf("AffectedModules: got %v, want %v", sf.AffectedModules, wantModules)
	}

	wantPatterns := []string{"table-driven-tests"}
	if !stringSlicesEqual(sf.RelevantPatterns, wantPatterns) {
		t.Errorf("RelevantPatterns: got %v, want %v", sf.RelevantPatterns, wantPatterns)
	}

	wantAnti := []string{"shared-mutable-state"}
	if !stringSlicesEqual(sf.RelevantAntiPatterns, wantAnti) {
		t.Errorf("RelevantAntiPatterns: got %v, want %v", sf.RelevantAntiPatterns, wantAnti)
	}

	// With pool=nil, zones and touchpoints must be nil.
	if len(sf.AffectedZones) != 0 {
		t.Errorf("AffectedZones must be empty when pool=nil; got %v", sf.AffectedZones)
	}
	if len(sf.CrossRepoTouchpoints) != 0 {
		t.Errorf("CrossRepoTouchpoints must be empty when pool=nil; got %v", sf.CrossRepoTouchpoints)
	}
}

// TestDeriveStructuredFields_CodexToolsAbsent verifies graceful degradation
// when Agent A's codex tools haven't shipped: the history contains only
// classic read_file / search_code records and we still get a populated
// AffectedFiles slice with everything else empty.
func TestDeriveStructuredFields_CodexToolsAbsent(t *testing.T) {
	history := []toolCallRecord{
		{tool: "read_file", arguments: `{"path":"cmd/gateway/main.go"}`},
		{tool: "search_code", result: "cmd/gateway/main.go:12:package main"},
	}
	sf := deriveStructuredFields(context.Background(), nil, uuid.Nil, history)
	if got := len(sf.AffectedFiles); got != 1 {
		t.Errorf("AffectedFiles: got %d, want 1 (%v)", got, sf.AffectedFiles)
	}
	if len(sf.AffectedModules) != 0 || len(sf.RelevantPatterns) != 0 || len(sf.RelevantAntiPatterns) != 0 {
		t.Errorf("codex-derived fields must be empty when codex tools are absent; got modules=%v patterns=%v anti=%v",
			sf.AffectedModules, sf.RelevantPatterns, sf.RelevantAntiPatterns)
	}
}

// stringSlicesEqual is a small helper — testify isn't imported in this
// package, and slices.Equal requires go1.21+ which is fine but let's keep
// the test loop-local for clarity.
func stringSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
// legacy-compat contract: old messages (published by research-agent
// before this change) have no "status" field. They must decode as
// Status=="" (zero value), which the coordinator treats as non-ok. This
// is the honest-failure guarantee — we deliberately do NOT default
// missing-status to "ok", because the whole reason we added the field
// is that old messages sometimes carried empty-evidence failures.
func TestResearchResultMessage_LegacyMessage_DecodesAsFailure(t *testing.T) {
	legacy := []byte(`{"task_id":"t1","work_id":"w1","finding_type":"code_trace","evidence":"","confidence":0.1}`)
	var dst researchResultMessage
	if err := json.Unmarshal(legacy, &dst); err != nil {
		t.Fatalf("unmarshal legacy: %v", err)
	}
	if dst.Status != "" {
		t.Fatalf("legacy message without status field must decode with empty Status; got %q", dst.Status)
	}
	// The coordinator's rule is "Status != ok blocks" — empty string is
	// not "ok", so legacy messages will block. Make that contract explicit.
	if dst.Status == ResearchStatusOK {
		t.Fatal("zero-value Status must NOT equal ResearchStatusOK — otherwise legacy messages would be treated as successes")
	}
}

// jsonContainsKey is a small helper that checks for a JSON key substring
// in marshalled output. Not as rigorous as re-parsing, but sufficient for
// confirming the wire tags are literally what we declared.
func jsonContainsKey(data []byte, key string) bool {
	return indexByteStr(string(data), key) >= 0
}

func indexByteStr(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
