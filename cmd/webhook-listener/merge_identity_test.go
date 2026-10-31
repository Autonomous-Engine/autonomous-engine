package main

import (
	"net/http"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/bus"
	"github.com/google/uuid"
)

// M4-05: pr.merged carries the merge identity (merge_commit_sha, head_sha,
// merged_at, repository) and the revert heuristic's result.

func TestGitHubPRMerged_CarriesMergeIdentity(t *testing.T) {
	work := uuid.New()
	body := prBody("agent/"+work.String(), true)
	pr := body["pull_request"].(map[string]any)
	pr["merge_commit_sha"] = "mmm111"
	pr["merged_at"] = "2026-10-03T12:00:00Z"
	pr["head"] = map[string]any{"ref": "agent/" + work.String(), "sha": "hhh222"}
	db := &fakeDB{repoID: uuid.New()}
	if rec := postGitHub(t, db, "pull_request", body); rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	rows := db.outboxRows(t)
	if len(rows) != 1 || rows[0].topic != bus.TopicPRMerged {
		t.Fatalf("rows = %+v", rows)
	}
	got := payloadWithoutEventID(t, rows[0])
	for k, want := range map[string]any{
		"merge_commit_sha": "mmm111", "head_sha": "hhh222", "merged_at": "2026-10-03T12:00:00Z",
		"repository": "acme/widgets", "work_id": work.String(),
	} {
		if got[k] != want {
			t.Errorf("%s = %v, want %v", k, got[k], want)
		}
	}
	if _, ok := got["reverts_pr_numbers"]; ok {
		t.Errorf("a non-revert PR carries reverts_pr_numbers: %v", got)
	}
	if _, ok := got["body"]; ok {
		t.Error("the PR body must not be forwarded")
	}
}

// A human revert PR (no work item) is forwarded so the reverted PRs'
// outcomes are marked; any other human PR is still skipped.
func TestGitHubPRMerged_HumanRevertIsForwardedWithoutWorkID(t *testing.T) {
	body := prBody("revert-41-agent/x", true)
	pr := body["pull_request"].(map[string]any)
	pr["title"] = `Revert "Add login"`
	pr["body"] = "Reverts acme/widgets#41"
	db := &fakeDB{repoID: uuid.New()}
	if rec := postGitHub(t, db, "pull_request", body); rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	rows := db.outboxRows(t)
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want the revert forwarded", len(rows))
	}
	got := payloadWithoutEventID(t, rows[0])
	nums, _ := got["reverts_pr_numbers"].([]any)
	if got["work_id"] != "" || len(nums) != 1 || nums[0] != float64(41) || got["is_agent"] != false {
		t.Fatalf("payload = %v", got)
	}
}
