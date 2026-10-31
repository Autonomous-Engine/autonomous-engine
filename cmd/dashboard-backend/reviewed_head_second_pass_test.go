package main

import (
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/models"
)

// TestReviewedHead_SecondPassRequestChangesAfterDeferredApproval: the
// verifier's approve_pending_second_pass is never an approval, so when the
// second pass then requests changes, the newest head with a branch is the
// reference (never the deferred approval's), an older cycle's real approval
// still wins over it, and a later cycle's approval of a newer head wins
// over both.
func TestReviewedHead_SecondPassRequestChangesAfterDeferredApproval(t *testing.T) {
	shaA, shaB, shaC := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "cccccccccccccccccccccccccccccccccccccccc"
	cases := []struct {
		name       string
		artifacts  []*models.WorkItemArtifact
		wantBranch string
		wantSHA    string
	}{
		{
			name: "deferred approval then second-pass request_changes: the newest head",
			artifacts: []*models.WorkItemArtifact{
				reviewArtifact(t, map[string]string{"kind": "verifier", "decision": "approve_pending_second_pass", "branch": "b1", "head_sha": shaA}),
				reviewArtifact(t, map[string]string{"kind": "second_pass", "decision": "request_changes", "branch": "b1", "head_sha": shaB}),
			},
			wantBranch: "b1", wantSHA: shaB,
		},
		{
			name: "second-pass request_changes is newest even when it names the deferred head",
			artifacts: []*models.WorkItemArtifact{
				reviewArtifact(t, map[string]string{"kind": "verifier", "decision": "request_changes", "branch": "b1", "head_sha": shaA}),
				reviewArtifact(t, map[string]string{"kind": "verifier", "decision": "approve_pending_second_pass", "branch": "b1", "head_sha": shaB}),
				reviewArtifact(t, map[string]string{"kind": "second_pass", "decision": "request_changes", "branch": "b1", "head_sha": shaB}),
			},
			wantBranch: "b1", wantSHA: shaB,
		},
		{
			name: "an older cycle's real approval wins over a later deferred-then-rejected head",
			artifacts: []*models.WorkItemArtifact{
				reviewArtifact(t, map[string]string{"kind": "second_pass", "decision": "approve", "branch": "b1", "head_sha": shaA}),
				reviewArtifact(t, map[string]string{"kind": "verifier", "decision": "approve_pending_second_pass", "branch": "b1", "head_sha": shaB}),
				reviewArtifact(t, map[string]string{"kind": "second_pass", "decision": "request_changes", "branch": "b1", "head_sha": shaB}),
			},
			wantBranch: "b1", wantSHA: shaA,
		},
		{
			name: "the next cycle's second-pass approval of a newer head wins",
			artifacts: []*models.WorkItemArtifact{
				reviewArtifact(t, map[string]string{"kind": "second_pass", "decision": "approve", "branch": "b1", "head_sha": shaA}),
				reviewArtifact(t, map[string]string{"kind": "verifier", "decision": "approve_pending_second_pass", "branch": "b1", "head_sha": shaB}),
				reviewArtifact(t, map[string]string{"kind": "second_pass", "decision": "request_changes", "branch": "b1", "head_sha": shaB}),
				reviewArtifact(t, map[string]string{"kind": "verifier", "decision": "approve_pending_second_pass", "branch": "b1", "head_sha": shaC}),
				reviewArtifact(t, map[string]string{"kind": "second_pass", "decision": "approve", "branch": "b1", "head_sha": shaC}),
			},
			wantBranch: "b1", wantSHA: shaC,
		},
		{
			name: "a pending verdict newer than the request_changes is still not an approval",
			artifacts: []*models.WorkItemArtifact{
				reviewArtifact(t, map[string]string{"kind": "second_pass", "decision": "request_changes", "branch": "b1", "head_sha": shaB}),
				reviewArtifact(t, map[string]string{"kind": "verifier", "decision": "approve_pending_second_pass", "branch": "b1", "head_sha": shaC}),
			},
			wantBranch: "b1", wantSHA: shaC,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			branch, sha := reviewedHead(tc.artifacts)
			if branch != tc.wantBranch || sha != tc.wantSHA {
				t.Errorf("reviewedHead = (%q, %q), want (%q, %q)", branch, sha, tc.wantBranch, tc.wantSHA)
			}
		})
	}
}
