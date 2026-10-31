package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestStructuredBrief_ProseContentFallback verifies proseContent() prefers
// the new `narrative` key when present and falls back to the legacy
// `brief` key when narrative is empty. This is the contract downstream
// callers rely on when handling artifacts written before Agent B added
// the structured shape.
func TestStructuredBrief_ProseContentFallback(t *testing.T) {
	tests := []struct {
		name string
		in   structuredBrief
		want string
	}{
		{
			name: "narrative preferred over brief",
			in:   structuredBrief{Brief: "legacy prose", Narrative: "new prose"},
			want: "new prose",
		},
		{
			name: "fallback to brief when narrative empty",
			in:   structuredBrief{Brief: "legacy prose", Narrative: ""},
			want: "legacy prose",
		},
		{
			name: "fallback to brief when narrative is whitespace-only",
			in:   structuredBrief{Brief: "legacy prose", Narrative: "   \n\t"},
			want: "legacy prose",
		},
		{
			name: "both empty returns empty string",
			in:   structuredBrief{Brief: "", Narrative: ""},
			want: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.in.proseContent()
			if got != tt.want {
				t.Errorf("proseContent() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestStructuredBrief_UnmarshalLegacy asserts that a legacy
// technical_brief artifact (pre-Agent-B, containing only `brief` and
// `work_id`) unmarshals cleanly into the structuredBrief type without
// erroring. All slice fields must end up as nil/empty, NOT missing, so
// that range-loops downstream are safe.
func TestStructuredBrief_UnmarshalLegacy(t *testing.T) {
	legacyJSON := `{"brief":"legacy prose content","work_id":"ee978d47-d0fe-49ba-821c-c6f174407727"}`

	var brief structuredBrief
	if err := json.Unmarshal([]byte(legacyJSON), &brief); err != nil {
		t.Fatalf("unmarshal legacy brief: %v", err)
	}

	if brief.Brief != "legacy prose content" {
		t.Errorf("Brief = %q, want legacy prose content", brief.Brief)
	}
	if brief.Narrative != "" {
		t.Errorf("Narrative = %q, want empty (legacy briefs don't carry narrative)", brief.Narrative)
	}
	// proseContent() must yield the legacy prose so decomposeIntoTasks
	// can still produce tasks against pre-Agent-B work items.
	if got := brief.proseContent(); got != "legacy prose content" {
		t.Errorf("proseContent() = %q, want legacy prose content", got)
	}
	// Slice fields should be zero-valued. Ranging over them must not panic.
	for _, slice := range [][]string{
		brief.AffectedFiles,
		brief.AffectedModules,
		brief.AffectedZones,
		brief.CrossRepoTouchpoints,
		brief.RelevantPatterns,
		brief.RelevantAntiPatterns,
	} {
		if len(slice) != 0 {
			t.Errorf("expected empty slice on legacy brief, got %v", slice)
		}
	}
}

// TestStructuredBrief_UnmarshalStructured asserts that a post-Agent-B
// brief round-trips all the structured fields.
func TestStructuredBrief_UnmarshalStructured(t *testing.T) {
	structuredJSON := `{
		"brief":"legacy copy",
		"narrative":"new canonical prose",
		"work_id":"abc",
		"affected_files":["cmd/foo/main.go","internal/bar/bar.go"],
		"affected_modules":["foo","bar"],
		"affected_zones":["api"],
		"cross_repo_touchpoints":["repo-a","repo-b"],
		"relevant_patterns":["idempotent-handler"],
		"relevant_anti_patterns":["global-mutable-state"]
	}`

	var brief structuredBrief
	if err := json.Unmarshal([]byte(structuredJSON), &brief); err != nil {
		t.Fatalf("unmarshal structured brief: %v", err)
	}
	if brief.proseContent() != "new canonical prose" {
		t.Errorf("proseContent() = %q, want new canonical prose", brief.proseContent())
	}
	if len(brief.AffectedFiles) != 2 {
		t.Errorf("AffectedFiles len = %d, want 2", len(brief.AffectedFiles))
	}
	if len(brief.CrossRepoTouchpoints) != 2 {
		t.Errorf("CrossRepoTouchpoints len = %d, want 2", len(brief.CrossRepoTouchpoints))
	}
	if len(brief.RelevantPatterns) != 1 || brief.RelevantPatterns[0] != "idempotent-handler" {
		t.Errorf("RelevantPatterns = %v", brief.RelevantPatterns)
	}
}

// TestIntersectOrRetain verifies the file-scope clamping rule: when the
// planner's structured brief carries affected_files, dispatched subtasks
// should get the intersection with the LLM's assigned files; when the
// brief is legacy (empty scope), the LLM's set passes through verbatim
// rather than being replaced with an empty slice.
func TestIntersectOrRetain(t *testing.T) {
	tests := []struct {
		name   string
		wanted []string
		scope  []string
		want   []string
	}{
		{
			name:   "empty scope returns wanted unchanged (legacy brief)",
			wanted: []string{"a.go", "b.go"},
			scope:  nil,
			want:   []string{"a.go", "b.go"},
		},
		{
			name:   "empty wanted falls back to scope",
			wanted: nil,
			scope:  []string{"a.go"},
			want:   []string{"a.go"},
		},
		{
			name:   "both empty returns nil",
			wanted: nil,
			scope:  nil,
			want:   nil,
		},
		{
			name:   "intersection preserves wanted ordering",
			wanted: []string{"b.go", "a.go", "c.go"},
			scope:  []string{"a.go", "b.go"},
			want:   []string{"b.go", "a.go"},
		},
		{
			name:   "empty intersection returns wanted (planner guess outside scope)",
			wanted: []string{"unknown.go"},
			scope:  []string{"a.go", "b.go"},
			want:   []string{"unknown.go"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := intersectOrRetain(tt.wanted, tt.scope)
			if !equalSlices(got, tt.want) {
				t.Errorf("intersectOrRetain(%v, %v) = %v, want %v", tt.wanted, tt.scope, got, tt.want)
			}
		})
	}
}

// TestFormatBulletList asserts empty-input produces empty string (so
// decomposeIntoTasks can safely drop the section) and non-empty input
// produces properly formatted Markdown bullets.
func TestFormatBulletList(t *testing.T) {
	if got := formatBulletList(nil); got != "" {
		t.Errorf("formatBulletList(nil) = %q, want empty string", got)
	}
	if got := formatBulletList([]string{}); got != "" {
		t.Errorf("formatBulletList([]) = %q, want empty string", got)
	}

	got := formatBulletList([]string{"one", "two"})
	want := "- one\n- two\n"
	if got != want {
		t.Errorf("formatBulletList([one, two]) = %q, want %q", got, want)
	}
	if !strings.HasPrefix(got, "- ") {
		t.Errorf("formatBulletList output should start with markdown bullet, got %q", got)
	}
}

// TestExtractFilePathsFromDescription checks the fallback parser that
// recovers file paths from prose when the structured files field is empty.
func TestExtractFilePathsFromDescription(t *testing.T) {
	tests := []struct {
		name string
		desc string
		want []string
	}{
		{
			name: "empty description",
			desc: "",
			want: nil,
		},
		{
			name: "no paths in prose",
			desc: "Implement the login page with proper authentication.",
			want: nil,
		},
		{
			name: "paths in prose",
			desc: "Edit src/components/LoginForm.tsx to add email validation and update src/api/auth.ts for the new endpoint.",
			want: []string{"src/components/LoginForm.tsx", "src/api/auth.ts"},
		},
		{
			name: "paths in Files marker",
			desc: "## Step 1: Add form\n\nAdd a contact form.\nFiles: [src/components/Contact.tsx src/pages/contact.ts]\n",
			want: []string{"src/components/Contact.tsx", "src/pages/contact.ts"},
		},
		{
			name: "deduplicates paths",
			desc: "First update src/app/page.tsx then also fix src/app/page.tsx for styling.",
			want: []string{"src/app/page.tsx"},
		},
		{
			name: "skips dot-prefixed and extensionless",
			desc: "Check .env and run npm install",
			want: nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractFilePathsFromDescription(tt.desc)
			if !equalSlices(got, tt.want) {
				t.Errorf("extractFilePathsFromDescription() = %v, want %v", got, tt.want)
			}
		})
	}
}

func equalSlices(a, b []string) bool {
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
