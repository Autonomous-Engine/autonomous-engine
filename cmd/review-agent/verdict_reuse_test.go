package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/verifier"
	"github.com/autonomousengine/autonomous-engine/internal/verifier/stage"
	"github.com/autonomousengine/autonomous-engine/internal/workgraph"
	"github.com/google/uuid"
)

func approvedVerdict() (verifier.Verdict, verifier.Decision) {
	v := verifier.Verdict{SchemaVersion: 1, Overall: verifier.OverallApprove, Defects: []verifier.Defect{{Severity: verifier.SeverityMinor, Description: "nit"}},
		PerCriterion: []verifier.CriterionResult{{ID: "AC-1", Status: verifier.StatusMet, Evidence: []verifier.Ref{"a.go:1"}}}}
	return v, verifier.Gate(v)
}

// V3: with the second pass off, the review outcome is decided from the
// stored verdict and recorded against it (no new verdict artifact).
func TestReusedVerdictArtifactRecordsTheOutcomeAgainstTheStoredVerdict(t *testing.T) {
	v, g := approvedVerdict()
	artID := uuid.New()
	sv := stage.StoredVerdict{
		Result:  stage.Result{Decision: stage.DecisionApprove, VerdictArtifactID: &artID},
		Content: stage.NewArtifactContent(v, g, string(stage.DecisionApprove), "agent/x", "abc", "abc", "task"),
	}
	p := planAfterVerifier(v, g, false, true)
	if p.secondPass || p.decision == nil || !p.decision.approve || !p.decision.humanReviewRequired {
		t.Fatalf("plan = %+v", p)
	}
	art := reusedVerdictArtifact(uuid.New(), sv, p, "agent/x", "abc", "abc")
	var c map[string]any
	if err := json.Unmarshal(art.Content, &c); err != nil {
		t.Fatal(err)
	}
	if c["kind"] != artifactKindVerifierReused || c["decision"] != "approve" || c["verdict_artifact_id"] != artID.String() || c["head_sha"] != "abc" || c["branch"] != "agent/x" {
		t.Fatalf("artifact = %v", c)
	}
	if d, ok := c["defects"].(map[string]any); !ok || d["minor"] != float64(1) {
		t.Errorf("defects = %v, want the verdict's counts (work_item_outcomes.review_defects)", c["defects"])
	}
}

// The pr.ready handler looks the stored verdict up before it would build a
// verifier run, and builds one only in the fallback branch.
func TestPRReady_ReusesTheStoredVerdictBeforeRunningTheVerifier(t *testing.T) {
	src := sources(t)["main.go"]
	lookup := strings.Index(src, "storedVerdictFor(msgCtx, pool, workID, verifiedSHA)")
	run := strings.Index(src, "newVerifierRun(ctx, msgCtx, pool, registry, cs, kb, cx, reviewProductID, workID, resolution)")
	if lookup < 0 || run < 0 || lookup > run {
		t.Fatalf("the stored verdict must be looked up before the verifier runs (lookup at %d, run at %d)", lookup, run)
	}
	between := src[lookup:run]
	if !strings.Contains(between, "if stored != nil") || !strings.Contains(between, "} else {") {
		t.Error("the verifier must run only in the branch without a stored verdict")
	}
}

func TestStoredVerdictForLegacyHeadRunsTheVerifier(t *testing.T) {
	sv, why := storedVerdictFor(context.Background(), nil, uuid.New(), "")
	if sv != nil || !strings.Contains(why, "legacy") {
		t.Fatalf("got %v %q", sv, why)
	}
}

// Against a real PostgreSQL: the verdict the stage committed is found for
// its head, not for another one.
func TestIntegration_StoredVerdictFor(t *testing.T) {
	pool := integrationPool(t)
	ctx := context.Background()
	f := seedStageFixture(t, pool)
	v, g := approvedVerdict()
	o := &stage.Outcome{
		Message:  stage.Message{WorkID: f.parent.String(), TaskID: f.subtask.String(), HeadSHA: f.head, Decision: stage.DecisionApprovePendingSecondPass},
		Result:   stage.Result{WorkID: f.parent, TaskID: f.subtask, HeadSHA: f.head, Decision: stage.DecisionApprovePendingSecondPass, VerifierVersion: verifier.Version},
		Artifact: stage.NewArtifact(f.parent, agentID, stage.NewArtifactContent(v, g, string(stage.DecisionApprovePendingSecondPass), "agent/x", f.head, f.head, f.subtask.String())),
	}
	if err := stage.Commit(ctx, pool, workgraph.New(pool), agentID, o); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM work_item_artifacts WHERE work_item_id = $1`, f.parent)
	})
	sv, why := storedVerdictFor(ctx, pool, f.parent, f.head)
	if sv == nil || sv.Content.Gate.Action != verifier.ActionApprove || *sv.Result.VerdictArtifactID != o.Artifact.ID {
		t.Fatalf("stored verdict = %+v (%s)", sv, why)
	}
	if sv, why := storedVerdictFor(ctx, pool, f.parent, strings.Repeat("e", 40)); sv != nil || !strings.Contains(why, "no verdict") {
		t.Fatalf("another head matched: %+v %q", sv, why)
	}
}
