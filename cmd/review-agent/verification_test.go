package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/evidence"
	"github.com/autonomousengine/autonomous-engine/internal/verifier/stage"
	"github.com/google/uuid"
)

// stageEvidence is a valid verified record for work/task in repo.
func stageEvidence(work, task, repo uuid.UUID, head string, outcome evidence.Outcome) evidence.ImplementationEvidence {
	now := time.Now().UTC().Truncate(time.Second)
	exit := 0
	r := repo.String()
	ev := evidence.ImplementationEvidence{
		SchemaVersion: evidence.SchemaVersion,
		WorkID:        work.String(),
		TaskID:        task.String(),
		RepositoryID:  &r,
		BaseSHA:       strings.Repeat("a", 40),
		HeadSHA:       head,
		Branch:        "agent/" + work.String() + "/" + task.String()[:8],
		ChangedFiles:  []string{"a.go"},
		Checks: []evidence.CheckResult{{
			Kind: evidence.CheckKindTest, Command: "go test ./...", ExitCode: &exit, Status: evidence.CheckPassed,
			StartedAt: &now, FinishedAt: &now, OutputRef: "artifact://x",
		}},
		Outcome:    outcome,
		StartedAt:  now,
		FinishedAt: now.Add(time.Minute),
	}
	switch outcome {
	case evidence.OutcomePartiallyVerified:
		ev.Checks = append(ev.Checks, evidence.CheckResult{Kind: evidence.CheckKindLint, Command: "lint", Status: evidence.CheckSkipped, SkipReason: "no linter"})
	case evidence.OutcomeFailed:
		bad := 1
		ev.Checks[0].ExitCode, ev.Checks[0].Status, ev.Reason = &bad, evidence.CheckFailed, "tests failed"
	}
	return ev
}

func implMessage(t *testing.T, work, task uuid.UUID, ev *evidence.ImplementationEvidence, success bool) implementationCompleteMessage {
	t.Helper()
	m := implementationCompleteMessage{WorkID: work.String(), TaskID: task.String(), Success: success}
	if ev != nil {
		raw, err := json.Marshal(ev)
		if err != nil {
			t.Fatal(err)
		}
		m.Evidence = raw
	}
	return m
}

// V1: only a releasable implementation is verified, at the evidence's head;
// everything else is recorded as not_run (the downstream consumers block it
// with their own reason) and a message without evidence is Legacy.
func TestPlanVerification(t *testing.T) {
	work, task, repo := uuid.New(), uuid.New(), uuid.New()
	repoS := repo.String()
	head := strings.Repeat("b", 40)
	verified := stageEvidence(work, task, repo, head, evidence.OutcomeVerified)
	partial := stageEvidence(work, task, repo, head, evidence.OutcomePartiallyVerified)
	failed := stageEvidence(work, task, repo, head, evidence.OutcomeFailed)
	foreign := stageEvidence(work, uuid.New(), repo, head, evidence.OutcomeVerified)

	cases := []struct {
		name     string
		m        implementationCompleteMessage
		policy   evidence.ReleasePolicy
		window   evidence.CompatWindow
		verify   bool
		decision stage.Decision
		legacy   bool
		reason   string
	}{
		{name: "verified", m: implMessage(t, work, task, &verified, true), verify: true},
		{name: "partial opted in", m: implMessage(t, work, task, &partial, true), policy: evidence.ReleasePolicy{PartialVerificationOptIn: true}, verify: true},
		{name: "partial not opted in", m: implMessage(t, work, task, &partial, true), decision: stage.DecisionNotRun, reason: "not releasable"},
		{name: "failed", m: implMessage(t, work, task, &failed, false), decision: stage.DecisionNotRun, reason: "not releasable"},
		{name: "verified but success=false", m: implMessage(t, work, task, &verified, false), decision: stage.DecisionNotRun, reason: "success=false"},
		{name: "another subtask's record", m: implMessage(t, work, task, &foreign, true), decision: stage.DecisionNotRun, reason: "not releasable"},
		{name: "no evidence legacy", m: implMessage(t, work, task, nil, true), window: evidence.CompatWindowLegacy, decision: stage.DecisionNotRun, legacy: true, reason: "runs on pr.ready"},
		{name: "no evidence strict", m: implMessage(t, work, task, nil, true), window: evidence.CompatWindowStrict, decision: stage.DecisionNotRun, legacy: true, reason: "strict"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := planVerification(c.m, c.policy, c.window, &repoS)
			if got.Verify != c.verify {
				t.Fatalf("verify = %t, want %t (%+v)", got.Verify, c.verify, got)
			}
			if c.verify {
				if got.HeadSHA != head || got.Branch != verified.Branch {
					t.Errorf("head/branch = %s %s, want the evidence's %s %s", got.HeadSHA, got.Branch, head, verified.Branch)
				}
				return
			}
			if got.Decision != c.decision || got.Legacy != c.legacy || !strings.Contains(got.Reason, c.reason) {
				t.Errorf("got %+v, want decision %s legacy %t reason containing %q", got, c.decision, c.legacy, c.reason)
			}
		})
	}
}

func TestSubtaskBranchMatchesPRCoordinator(t *testing.T) {
	work := uuid.MustParse("63f733e0-de21-40ad-b652-2cff5190ee40")
	task := uuid.MustParse("99a8e484-99b0-44a0-abe0-320efa2aaa5e")
	if got, want := subtaskBranch(work, task), "agent/63f733e0-de21-40ad-b652-2cff5190ee40/99a8e484"; got != want {
		t.Fatalf("branch = %s, want %s", got, want)
	}
}
