package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/evidence"
	"github.com/autonomousengine/autonomous-engine/internal/models"
	"github.com/autonomousengine/autonomous-engine/internal/verifier/stage"
	"github.com/google/uuid"
)

func verifiedOutcome(t *testing.T) subtaskOutcome {
	t.Helper()
	o := decideSubtaskPR(implementationCompletePayload{TaskID: testTaskID, WorkID: testWorkID, Success: true, Evidence: evidenceJSON(t, true, nil)}, evidence.ReleasePolicy{}, evidence.CompatWindowStrict)
	if o.Action != evidence.ActionRelease {
		t.Fatalf("fixture outcome = %+v", o)
	}
	return o
}

// V2: the verification decision on top of the evidence decision. D1: a PR
// needs a releasable outcome AND an approve decision for the same head.
func TestApplyVerification(t *testing.T) {
	legacy := decideSubtaskPR(implementationCompletePayload{TaskID: testTaskID, WorkID: testWorkID, Success: true}, evidence.ReleasePolicy{}, evidence.CompatWindowLegacy)
	failed := decideSubtaskPR(implementationCompletePayload{TaskID: testTaskID, WorkID: testWorkID, Success: false, Reason: "boom"}, evidence.ReleasePolicy{}, evidence.CompatWindowLegacy)
	cases := []struct {
		name       string
		o          subtaskOutcome
		v          verification
		action     evidence.ReleaseAction
		label      string
		secondPass bool
	}{
		{"approve", verifiedOutcome(t), verification{Known: true, Decision: stage.DecisionApprove, HeadSHA: testHeadSHA}, evidence.ActionRelease, outcomeLabelVerified, false},
		{"approve pending second pass", verifiedOutcome(t), verification{Known: true, Decision: stage.DecisionApprovePendingSecondPass, HeadSHA: testHeadSHA}, evidence.ActionRelease, outcomeLabelVerified, true},
		{"approve of another head", verifiedOutcome(t), verification{Known: true, Decision: stage.DecisionApprove, HeadSHA: strings.Repeat("9", 40)}, evidence.ActionBlock, outcomeLabelVerificationHeadDiffer, false},
		{"escalate", verifiedOutcome(t), verification{Known: true, Decision: stage.DecisionEscalate, Reason: "criteria the verifier could not verify: AC-2"}, evidence.ActionBlock, outcomeLabelVerifierEscalated, false},
		{"request changes", verifiedOutcome(t), verification{Known: true, Decision: stage.DecisionRequestChanges}, evidence.ActionBlock, outcomeLabelVerifierChanges, false},
		{"error", verifiedOutcome(t), verification{Known: true, Decision: stage.DecisionError}, evidence.ActionBlock, outcomeLabelVerificationFailed, false},
		{"not run on verified evidence", verifiedOutcome(t), verification{Known: true, Decision: stage.DecisionNotRun}, evidence.ActionBlock, outcomeLabelVerificationNotRun, false},
		{"unknown", verifiedOutcome(t), verification{}, evidence.ActionBlock, outcomeLabelVerificationMissing, false},
		{"legacy message keeps its path", legacy, verification{Known: true, Decision: stage.DecisionNotRun, Legacy: true}, evidence.ActionRelease, outcomeLabelLegacy, false},
		{"a blocked subtask stays blocked even if approved", failed, verification{Known: true, Decision: stage.DecisionApprove}, evidence.ActionBlock, outcomeLabelFailed, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := applyVerification(c.o, c.v)
			if got.Action != c.action || got.Label != c.label || got.SecondPassPending != c.secondPass {
				t.Fatalf("got action=%s label=%q secondPass=%t reason=%q, want %s %q %t", got.Action, got.Label, got.SecondPassPending, got.Reason, c.action, c.label, c.secondPass)
			}
			if got.Verification == nil || got.Verification.Decision != c.v.Decision {
				t.Errorf("verification not attached: %+v", got.Verification)
			}
		})
	}
	esc := applyVerification(verifiedOutcome(t), verification{Known: true, Decision: stage.DecisionEscalate, Reason: "criteria the verifier could not verify: AC-2"})
	if !strings.Contains(esc.Reason, "AC-2") {
		t.Errorf("escalation reason lost the verdict: %q", esc.Reason)
	}
}

// The second-pass label goes inside the partial-verification marker, so
// "[PARTIALLY VERIFIED — human review required] " stays the title's prefix;
// both are idempotent.
func TestVerificationTitle(t *testing.T) {
	approved := verifiedOutcome(t)
	approved = applyVerification(approved, verification{Known: true, Decision: stage.DecisionApprovePendingSecondPass, HeadSHA: testHeadSHA})
	partial := approved
	partial.Action = evidence.ActionHumanReview
	got := prTitleFor(verificationTitle("[repo] Title", []subtaskOutcome{partial}), []subtaskOutcome{partial})
	if want := partialVerificationTitlePrefix + stage.SecondPassTitlePrefix + "[repo] Title"; got != want {
		t.Fatalf("title = %q, want %q", got, want)
	}
	if again := prTitleFor(verificationTitle(got, []subtaskOutcome{partial}), []subtaskOutcome{partial}); again != got {
		t.Errorf("not idempotent: %q", again)
	}
	final := applyVerification(verifiedOutcome(t), verification{Known: true, Decision: stage.DecisionApprove, HeadSHA: testHeadSHA})
	if got := verificationTitle("[repo] Title", []subtaskOutcome{final}); got != "[repo] Title" {
		t.Errorf("a final approval is unlabelled, got %q", got)
	}
}

// "## Verification": decision, head, reason, every criterion with its note,
// and the gaps — after the requirements section.
func TestRenderVerificationSection(t *testing.T) {
	o := applyVerification(verifiedOutcome(t), verification{
		Known: true, Decision: stage.DecisionApprovePendingSecondPass, HeadSHA: testHeadSHA,
		Reason:   "every criterion met; no critical or major defect",
		Criteria: []stage.CriterionSummary{{ID: "AC-1", Status: "met", Note: "a|b"}, {ID: "AC-2", Status: "not_verifiable", Note: "non-code"}},
		Gaps:     []string{"criterion AC-2 is non-code: satisfied by the PR text; verify manually"},
	})
	o.Summary = "Step 1"
	body := renderPRBody(prBodyInput{Title: "T", Branch: "agent/x", Outcomes: []subtaskOutcome{o},
		Requirements: nil})
	for _, want := range []string{
		"## Verification",
		"- Step 1: **approve_pending_second_pass** — head `" + testHeadSHA + "` — every criterion met",
		"| AC-1 | met | a\\|b |",
		"| AC-2 | not_verifiable | non-code |",
		"Gaps a human must check:",
		"criterion AC-2 is non-code: satisfied by the PR text; verify manually",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body lacks %q:\n%s", want, body)
		}
	}
	if strings.Index(body, "## Verification") > strings.Index(body, "## Subtasks") {
		t.Error("## Verification must come before the subtask list")
	}
	if line := verificationPromptLine([]subtaskOutcome{o}); !strings.Contains(line, "approve_pending_second_pass (1/2 criteria met)") || !strings.Contains(line, "1 gap(s)") {
		t.Errorf("prompt line = %q", line)
	}
	if renderVerificationSection([]subtaskOutcome{verifiedOutcome(t)}) != "" {
		t.Error("no verification → no section")
	}
}

func TestPayloadFromVerification(t *testing.T) {
	impl, _ := json.Marshal(map[string]any{"task_id": testTaskID, "work_id": testWorkID, "success": true, "evidence": evidenceJSON(t, true, nil)})
	p := payloadFromVerification(stage.Message{WorkID: testWorkID, TaskID: testTaskID, Implementation: impl})
	if !p.Success || !p.hasEvidence() || p.TaskID != testTaskID {
		t.Fatalf("payload = %+v", p)
	}
	empty := payloadFromVerification(stage.Message{WorkID: testWorkID, TaskID: testTaskID})
	if empty.Success || empty.hasEvidence() || empty.WorkID != testWorkID {
		t.Fatalf("a message without the payload must decide nothing releasable: %+v", empty)
	}
}

// Review M2: all-or-nothing. One approved and one escalated subtask of the
// round → refused, naming the escalated one; an evidence-blocked sibling
// or one from an earlier round does not refuse; a legacy message keeps its
// path.
func TestAllOrNothingRefusal(t *testing.T) {
	approved := applyVerification(verifiedOutcome(t), verification{Known: true, Decision: stage.DecisionApprove, HeadSHA: testHeadSHA})
	escalated := applyVerification(verifiedOutcome(t), verification{Known: true, Decision: stage.DecisionEscalate, Reason: "AC-2 not verifiable"})
	failed := applyVerification(decideSubtaskPR(implementationCompletePayload{TaskID: testTaskID, WorkID: testWorkID, Success: false, Reason: "x"}, evidence.ReleasePolicy{}, evidence.CompatWindowLegacy),
		verification{Known: true, Decision: stage.DecisionNotRun})
	legacy := applyVerification(decideSubtaskPR(implementationCompletePayload{TaskID: testTaskID, WorkID: testWorkID, Success: true}, evidence.ReleasePolicy{}, evidence.CompatWindowLegacy),
		verification{Known: true, Decision: stage.DecisionNotRun, Legacy: true})
	unverified := applyVerification(verifiedOutcome(t), verification{})

	a := &models.WorkItem{ID: uuid.New(), Type: models.WorkItemTypeSubtask, State: models.WorkItemStatePassed}
	b := &models.WorkItem{ID: uuid.New(), Type: models.WorkItemTypeSubtask, State: models.WorkItemStatePassed}
	outcomes := map[uuid.UUID]subtaskOutcome{}
	of := func(c *models.WorkItem) subtaskOutcome { return outcomes[c.ID] }
	kids := []*models.WorkItem{a, b}

	outcomes[a.ID], outcomes[b.ID] = approved, escalated
	if r := allOrNothingRefusal(kids, nil, time.Time{}, of); !strings.Contains(r, b.ID.String()) || !strings.Contains(r, "escalate") {
		t.Errorf("approve + escalate → %q, want a refusal naming %s", r, b.ID)
	}
	outcomes[b.ID] = failed
	if r := allOrNothingRefusal(kids, nil, time.Time{}, of); r != "" {
		t.Errorf("an evidence-blocked sibling must not refuse: %q", r)
	}
	outcomes[b.ID] = legacy
	if r := allOrNothingRefusal(kids, nil, time.Time{}, of); r != "" {
		t.Errorf("a legacy-window sibling must not refuse: %q", r)
	}
	outcomes[b.ID] = unverified
	if r := allOrNothingRefusal(kids, nil, time.Time{}, of); !strings.Contains(r, "decision none") {
		t.Errorf("an unverified sibling must refuse: %q", r)
	}
	// b passed before the parent re-entered implementation: earlier round.
	start := time.Now()
	outcomes[b.ID] = escalated
	if r := allOrNothingRefusal(kids, map[uuid.UUID]time.Time{b.ID: start.Add(-time.Hour)}, start, of); r != "" {
		t.Errorf("an earlier round's verdict must not refuse: %q", r)
	}
	blocked := &models.WorkItem{ID: uuid.New(), Type: models.WorkItemTypeSubtask, State: models.WorkItemStateBlocked}
	outcomes[blocked.ID] = escalated
	outcomes[b.ID] = approved
	if r := allOrNothingRefusal([]*models.WorkItem{a, b, blocked}, nil, time.Time{}, of); r != "" {
		t.Errorf("a blocked subtask is not part of the release: %q", r)
	}
}

// pr-coordinator subscribes to verification.complete only.
func TestCoordinator_SubscribesToVerificationComplete(t *testing.T) {
	main := stripComments(coordinatorSources(t)["main.go"])
	if !strings.Contains(main, "kb.Subscribe(bus.TopicVerificationComplete, consumerGroup, handler)") {
		t.Error("main.go must subscribe the handler to verification.complete")
	}
	for name, src := range coordinatorSources(t) {
		if strings.Contains(stripComments(src), "bus.TopicImplementationComplete") {
			t.Errorf("%s still references implementation.complete; the PR waits for the verification stage", name)
		}
	}
}
