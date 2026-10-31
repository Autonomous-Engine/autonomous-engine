package main

import (
	"encoding/json"
	"testing"
)

// TestAggregateStructuredFields_UnionDedupe verifies the coordinator's
// across-task aggregation: 5 research tasks each contribute their own
// structured fields, the coordinator unions them and dedupes so the
// technical brief carries a single coherent view of everything touched.
// This is the contract downstream planners depend on — the whole point
// of Approach A is that file paths survive the 5→1 aggregation without
// re-extraction from prose.
func TestAggregateStructuredFields_UnionDedupe(t *testing.T) {
	results := []researchResultMessage{
		{
			TaskID:               "task-1",
			FindingType:          "code_trace",
			AffectedFiles:        []string{"a.go", "b.go"},
			AffectedModules:      []string{"alpha", "beta"},
			AffectedZones:        []string{"core"},
			CrossRepoTouchpoints: []string{"repo-a"},
			RelevantPatterns:     []string{"retry"},
			RelevantAntiPatterns: []string{"shared-state"},
		},
		{
			TaskID:               "task-2",
			FindingType:          "pattern_match",
			AffectedFiles:        []string{"b.go", "c.go"}, // b.go dupes task-1
			AffectedModules:      []string{"beta", "gamma"},
			AffectedZones:        []string{"core", "io"},
			CrossRepoTouchpoints: []string{"repo-a", "repo-b"},
			RelevantPatterns:     []string{"retry", "circuit-breaker"},
			RelevantAntiPatterns: []string{},
		},
		{
			TaskID:          "task-3",
			FindingType:     "history_analysis",
			AffectedFiles:   nil, // tasks that produced prose-only evidence contribute nothing
			AffectedModules: nil,
		},
		{
			TaskID:               "task-4",
			FindingType:          "impact_assessment",
			AffectedFiles:        []string{"a.go"},
			AffectedModules:      []string{"alpha"},
			RelevantAntiPatterns: []string{"n-plus-one", "shared-state"},
		},
		{
			TaskID:           "task-5",
			FindingType:      "bug_archaeology",
			AffectedFiles:    []string{"d.go"},
			RelevantPatterns: []string{"circuit-breaker", "timeout"},
		},
	}

	agg := aggregateStructuredFields(results)

	// Files: union of {a,b,c,d} in sorted order.
	wantFiles := []string{"a.go", "b.go", "c.go", "d.go"}
	if !equal(agg.AffectedFiles, wantFiles) {
		t.Errorf("AffectedFiles: got %v, want %v", agg.AffectedFiles, wantFiles)
	}

	// Modules: {alpha, beta, gamma}
	wantModules := []string{"alpha", "beta", "gamma"}
	if !equal(agg.AffectedModules, wantModules) {
		t.Errorf("AffectedModules: got %v, want %v", agg.AffectedModules, wantModules)
	}

	// Zones: {core, io}
	wantZones := []string{"core", "io"}
	if !equal(agg.AffectedZones, wantZones) {
		t.Errorf("AffectedZones: got %v, want %v", agg.AffectedZones, wantZones)
	}

	// Cross-repo touchpoints: {repo-a, repo-b}
	wantTouchpoints := []string{"repo-a", "repo-b"}
	if !equal(agg.CrossRepoTouchpoints, wantTouchpoints) {
		t.Errorf("CrossRepoTouchpoints: got %v, want %v", agg.CrossRepoTouchpoints, wantTouchpoints)
	}

	// Patterns: {circuit-breaker, retry, timeout}
	wantPatterns := []string{"circuit-breaker", "retry", "timeout"}
	if !equal(agg.RelevantPatterns, wantPatterns) {
		t.Errorf("RelevantPatterns: got %v, want %v", agg.RelevantPatterns, wantPatterns)
	}

	// Anti-patterns: {n-plus-one, shared-state}
	wantAnti := []string{"n-plus-one", "shared-state"}
	if !equal(agg.RelevantAntiPatterns, wantAnti) {
		t.Errorf("RelevantAntiPatterns: got %v, want %v", agg.RelevantAntiPatterns, wantAnti)
	}
}

// TestAggregateStructuredFields_Empty verifies that zero results produce
// stable empty (non-nil) slices. The planner's JSON parser keys off the
// array shape, so nil-vs-empty matters on the wire.
func TestAggregateStructuredFields_Empty(t *testing.T) {
	agg := aggregateStructuredFields(nil)
	if agg.AffectedFiles == nil {
		t.Error("AffectedFiles must be empty slice, not nil, for stable JSON shape")
	}
	if len(agg.AffectedFiles) != 0 {
		t.Errorf("AffectedFiles len: got %d, want 0", len(agg.AffectedFiles))
	}
}

// TestTechnicalBrief_JSONShape locks in the artifact content shape. The
// planner reads `brief` for backwards compatibility AND the structured
// fields are available under their snake_case keys. A JSON tag drift
// here silently breaks the planner without a test failure downstream.
func TestTechnicalBrief_JSONShape(t *testing.T) {
	b := technicalBrief{
		Brief:                "narrative content",
		Narrative:            "narrative content",
		WorkID:               "work-1",
		AffectedFiles:        []string{"x.go"},
		AffectedModules:      []string{"m"},
		AffectedZones:        []string{"z"},
		CrossRepoTouchpoints: []string{"r"},
		RelevantPatterns:     []string{"p"},
		RelevantAntiPatterns: []string{"ap"},
	}
	data, err := json.Marshal(b)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, key := range []string{
		`"brief":`,
		`"narrative":`,
		`"work_id":`,
		`"affected_files":`,
		`"affected_modules":`,
		`"affected_zones":`,
		`"cross_repo_touchpoints":`,
		`"relevant_patterns":`,
		`"relevant_anti_patterns":`,
	} {
		if !contains(string(data), key) {
			t.Errorf("expected JSON to contain %s; got %s", key, string(data))
		}
	}
}

func equal(a, b []string) bool {
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

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
