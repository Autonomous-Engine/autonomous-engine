package main

import (
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/evidence"
	"github.com/autonomousengine/autonomous-engine/internal/gitref"
)

const (
	shaA = "1111111111111111111111111111111111111111"
	shaB = "2222222222222222222222222222222222222222"
)

// The resolution rule lives in internal/gitref; this pins the review-agent
// policy on top of it: a branch that is gone from the remote blocks review
// (there is no PR branch to review), unlike QA which may accept a SHA that
// is still reachable via the merge.
func TestReviewTargetPolicy(t *testing.T) {
	tests := []struct {
		name    string
		head    string
		tip     string
		window  evidence.CompatWindow
		wantSHA string
		blocked bool
		legacy  bool
		reason  string
	}{
		{name: "sha present and equals tip", head: shaA, tip: shaA, window: evidence.CompatWindowStrict, wantSHA: shaA},
		{name: "sha present and differs → block", head: shaA, tip: shaB, window: evidence.CompatWindowLegacy, blocked: true, reason: gitref.BranchMovedReason(shaA, shaB)},
		{name: "sha present, branch gone → block", head: shaA, tip: "", window: evidence.CompatWindowLegacy, blocked: true, reason: gitref.BranchMovedReason(shaA, "")},
		{name: "sha absent, legacy → tip with warning", head: "", tip: shaB, window: evidence.CompatWindowLegacy, wantSHA: shaB, legacy: true, reason: "pr.ready carries no verified head_sha"},
		{name: "sha absent, strict → block", head: "", tip: shaB, window: evidence.CompatWindowStrict, blocked: true, reason: evidence.CompatWindowEnv + "=strict"},
		{name: "sha malformed, strict → block", head: "--upload-pack=x", tip: shaB, window: evidence.CompatWindowStrict, blocked: true, reason: "malformed head_sha"},
		{name: "sha malformed, legacy → tip", head: "HEAD~1", tip: shaB, window: evidence.CompatWindowLegacy, wantSHA: shaB, legacy: true, reason: "malformed head_sha"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveReviewTarget(tc.head, tc.tip, tc.window)
			if got.Blocked != tc.blocked || got.Legacy != tc.legacy || got.SHA != tc.wantSHA {
				t.Fatalf("got %+v, want blocked=%t legacy=%t sha=%q", got, tc.blocked, tc.legacy, tc.wantSHA)
			}
			if !strings.Contains(got.Reason, tc.reason) {
				t.Fatalf("Reason = %q, want it to contain %q", got.Reason, tc.reason)
			}
		})
	}
}

// sanitizeHeadSHA is what the handler applies to payload.HeadSHA at entry:
// a malformed value reads as absent and is never handed to git.
func TestSanitizeHeadSHAAtEntry(t *testing.T) {
	cases := []struct {
		raw  string
		want string
	}{
		{"", ""},
		{" " + strings.ToUpper(shaA) + "\n", shaA},
		{"--upload-pack=x", ""},
		{"origin/main", ""},
		{"abc", ""},
	}
	for _, tc := range cases {
		if got := sanitizeHeadSHA("work-1", tc.raw); got != tc.want {
			t.Errorf("sanitizeHeadSHA(%q) = %q, want %q", tc.raw, got, tc.want)
		}
	}
}

func TestPromptLines(t *testing.T) {
	if got := headSHALine(shaA, false); !strings.Contains(got, "Verified head SHA under review: "+shaA) {
		t.Fatalf("verified line = %q", got)
	}
	if got := headSHALine(shaA, true); !strings.Contains(got, "Branch tip under review: "+shaA) || !strings.Contains(got, "legacy") {
		t.Fatalf("legacy line = %q", got)
	}
	if got := headSHALine("", true); got != "" {
		t.Fatalf("no SHA must produce no line, got %q", got)
	}
	if got := filesChangedLine([]string{"a.go"}, true); !strings.Contains(got, "from verification evidence") || !strings.Contains(got, "a.go") {
		t.Fatalf("evidence files line = %q", got)
	}
	if got := filesChangedLine([]string{"a.go"}, false); !strings.Contains(got, "not git-verified") {
		t.Fatalf("legacy files line = %q", got)
	}
}
