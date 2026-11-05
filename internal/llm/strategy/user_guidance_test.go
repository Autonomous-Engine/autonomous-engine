package strategy

import (
	"strings"
	"testing"
)

// Reviewer feedback from "Request changes" rides in user_guidance; the PRD
// prompt must show it right after the pain point, not buried among the
// unordered keys (F34).
func TestBuildUserPromptRendersUserGuidanceAfterPainPoint(t *testing.T) {
	// Keys outside the priority list render in map order, so one run can
	// pass by chance; the section must follow the pain point every time.
	for i := 0; i < 30; i++ {
		got := buildUserPrompt(Intent{Type: IntentGeneratePRD, Context: map[string]string{
			"pain_point":    "Headers declared GB2312 need GB18030.",
			"user_guidance": "Use the existing Option type.",
			"codex_context": "modules…",
			"source":        "dashboard",
			"title":         "Add parser option",
		}})
		pp := strings.Index(got, "## Pain Point")
		if pp < 0 {
			t.Fatalf("no pain point section:\n%s", got)
		}
		rest := got[pp+len("## Pain Point"):]
		next := strings.Index(rest, "\n## ")
		if next < 0 || !strings.HasPrefix(rest[next+1:], "## User Guidance") {
			t.Fatalf("run %d: the section after Pain Point is not User Guidance:\n%s", i, got)
		}
	}
}
