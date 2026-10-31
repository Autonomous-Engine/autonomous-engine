package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/plan"
	"github.com/autonomousengine/autonomous-engine/internal/verifier"
	"github.com/google/uuid"
)

// R3 P1: a non-code criterion reaches human review as a recorded gap. The
// model approves every code criterion; the verdict still escalates, and
// both the human_review transition reason and the verdict artifact name
// the gap.
func TestVerifierStageNonCodeCriterionEscalatesWithTheGap(t *testing.T) {
	f, repo := fixtureRepo(t, "03-unhandled-nil")
	m := &scriptedModel{answers: []string{verdictApprove}}
	criteria := append([]plan.Criterion(nil), f.Criteria...)
	criteria = append(criteria, plan.Criterion{
		ID: "AC-9", Text: "The change is deployed to staging", Source: plan.Source{Kind: plan.SourceDerived, Reason: "rollout"},
		Risk: plan.RiskLow, NonCode: true, NonCodeReason: `cited only by task 3 "Deploy to staging", which the planner dropped (process work)`,
	})
	reqs := verifier.Requirements{Criteria: criteria, TaskSpecs: []verifier.TaskSpec{{TaskID: "task-1", Spec: f.TaskSpec}}}
	v := verifyAt(context.Background(), verifierRun{adapter: m}, uuid.New(), reqs, verifier.Evidence{}, repo.Path, repo.BaseSHA, repo.BaseSHA, repo.HeadSHA)
	const gap = `criterion AC-9 is non-code: cited only by task 3 "Deploy to staging", which the planner dropped (process work); verify manually`
	if len(m.reqs) != 1 || strings.Contains(m.reqs[0].Messages[1].Content, "deployed to staging") {
		t.Fatalf("the model was asked about the non-code criterion (%d calls)", len(m.reqs))
	}
	d := verifier.Gate(v)
	if d.Action != verifier.ActionEscalate {
		t.Fatalf("gate = %+v verdict = %+v", d, v)
	}
	p := planAfterVerifier(v, d, true, false)
	if p.decision == nil || !p.decision.escalate || !strings.Contains(p.decision.reason, gap) {
		t.Fatalf("human_review reason does not carry the gap: %+v", p.decision)
	}
	art := verifierArtifact(uuid.New(), v, d, p, "agent/x", repo.HeadSHA, repo.HeadSHA)
	var content verifierArtifactContent
	if err := json.Unmarshal(art.Content, &content); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(content.Feedback, gap) || len(content.Verdict.Gaps) != 1 || content.Verdict.Gaps[0] != gap {
		t.Fatalf("artifact lacks the gap: feedback=%q gaps=%q", content.Feedback, content.Verdict.Gaps)
	}
}
