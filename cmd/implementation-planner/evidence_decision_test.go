package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/evidence"
)

const (
	testWorkID = "0f1e2d3c-0000-4000-8000-000000000001"
	testTaskID = "0f1e2d3c-0000-4000-8000-000000000002"
	// otherID is a work item that is not the one the message names; a
	// record carrying it does not belong to the message.
	otherID = "0f1e2d3c-0000-4000-8000-0000000000ff"
)

// foreignTaskJSON is a valid verified record whose task_id is another
// subtask's: the payload's own ids are unchanged, so the only thing wrong
// with it is that it is not this message's evidence.
func foreignTaskJSON(t *testing.T) json.RawMessage {
	return evidenceJSON(t, true, func(ev *evidence.ImplementationEvidence) { ev.TaskID = otherID })
}

// Two repositories: the task's own and another one.
const (
	repoA = "0f1e2d3c-0000-4000-8000-0000000000e1"
	repoB = "0f1e2d3c-0000-4000-8000-0000000000e2"
)

func repoIDPtr(s string) *string { return &s }

// repoJSON is a valid verified record that names repositoryID.
func repoJSON(t *testing.T, repositoryID string) json.RawMessage {
	return evidenceJSON(t, true, func(ev *evidence.ImplementationEvidence) { ev.RepositoryID = repoIDPtr(repositoryID) })
}

// baseEvidence is a valid schema-1 verified record: one passed test check
// against a concrete head SHA. Same fixture as cmd/status-orchestrator so
// both consumers are judged against identical inputs.
func baseEvidence() evidence.ImplementationEvidence {
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	exit := 0
	return evidence.ImplementationEvidence{
		SchemaVersion: evidence.SchemaVersion,
		WorkID:        testWorkID,
		TaskID:        testTaskID,
		BaseSHA:       "1111111111111111111111111111111111111111",
		HeadSHA:       "2222222222222222222222222222222222222222",
		Branch:        "ae/task-1",
		ChangedFiles:  []string{"main.go"},
		Checks: []evidence.CheckResult{{
			Kind:     evidence.CheckKindTest,
			Command:  "go test ./...",
			ExitCode: &exit,
			Status:   evidence.CheckPassed,
		}},
		Outcome:      evidence.OutcomeVerified,
		StartedAt:    now,
		FinishedAt:   now.Add(time.Minute),
		ArtifactRefs: []string{},
	}
}

// evidenceJSON marshals a mutated baseEvidence. It validates the record
// unless the mutation is meant to produce an invalid one, so a fixture
// drift is caught here rather than hidden as an unexpected block.
func evidenceJSON(t *testing.T, wantValid bool, mutate func(*evidence.ImplementationEvidence)) json.RawMessage {
	t.Helper()
	ev := baseEvidence()
	if mutate != nil {
		mutate(&ev)
	}
	if err := ev.Validate(); (err == nil) != wantValid {
		t.Fatalf("fixture validity = %v (err %v), want valid=%v", err == nil, err, wantValid)
	}
	b, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	return b
}

func verifiedJSON(t *testing.T) json.RawMessage { return evidenceJSON(t, true, nil) }

func partiallyVerifiedJSON(t *testing.T) json.RawMessage {
	return evidenceJSON(t, true, func(ev *evidence.ImplementationEvidence) {
		ev.Checks = append(ev.Checks, evidence.CheckResult{
			Kind:       evidence.CheckKindLint,
			Command:    "golangci-lint run",
			Status:     evidence.CheckSkipped,
			SkipReason: "no linter configured",
		})
		ev.Outcome = evidence.OutcomePartiallyVerified
	})
}

func failedJSON(t *testing.T) json.RawMessage {
	return evidenceJSON(t, true, func(ev *evidence.ImplementationEvidence) {
		exit := 1
		ev.Checks[0].Status = evidence.CheckFailed
		ev.Checks[0].ExitCode = &exit
		ev.Outcome = evidence.OutcomeFailed
		ev.Reason = "tests failed on 2222222"
	})
}

func unverifiedJSON(t *testing.T) json.RawMessage {
	return evidenceJSON(t, true, func(ev *evidence.ImplementationEvidence) {
		ev.Checks = []evidence.CheckResult{{
			Kind:       evidence.CheckKindTest,
			Status:     evidence.CheckSkipped,
			SkipReason: "no test runner detected",
		}}
		ev.Outcome = evidence.OutcomeUnverified
		ev.Reason = "no test runner detected"
	})
}

func unknownSchemaJSON(t *testing.T) json.RawMessage {
	return evidenceJSON(t, false, func(ev *evidence.ImplementationEvidence) {
		ev.SchemaVersion = evidence.SchemaVersion + 1
	})
}

// Policies as products.LoadReleasePolicyForItem would return them: opted in,
// explicitly not opted in, and the zero policy for an unknown product or a
// failed lookup (which must behave exactly like "not opted in").
func optedIn() evidence.ReleasePolicy { return evidence.ReleasePolicy{PartialVerificationOptIn: true} }
func notOptedIn() evidence.ReleasePolicy {
	return evidence.ReleasePolicy{PartialVerificationOptIn: false}
}
func unknownProduct() evidence.ReleasePolicy { return evidence.ReleasePolicy{} }

// TestDecideDependentRelease carries the same cases as the status-orchestrator
// table (TestDecideSubtaskTransition) so both consumers of
// implementation.complete agree on every input: passed there is release
// here, human_review is human_review, blocked is block.
func TestDecideDependentRelease(t *testing.T) {
	cases := []struct {
		name    string
		payload implementationCompletePayload
		policy  evidence.ReleasePolicy
		window  evidence.CompatWindow
		// repositoryID is the task's repository from its work item row
		// (nil: unknown, not checked), as releasePolicyForTask returns it.
		repositoryID *string
		wantAction   evidence.ReleaseAction
		wantReason   []string // substrings that must appear in the reason
	}{
		{
			name:       "verified releases",
			payload:    implementationCompletePayload{TaskID: testTaskID, WorkID: testWorkID, Success: true, Evidence: verifiedJSON(t)},
			policy:     notOptedIn(),
			window:     evidence.CompatWindowLegacy,
			wantAction: evidence.ActionRelease,
			wantReason: []string{"outcome verified", "2222222222222222222222222222222222222222"},
		},
		{
			name:       "verified releases in strict mode too",
			payload:    implementationCompletePayload{TaskID: testTaskID, WorkID: testWorkID, Success: true, Evidence: verifiedJSON(t)},
			policy:     unknownProduct(),
			window:     evidence.CompatWindowStrict,
			wantAction: evidence.ActionRelease,
			wantReason: []string{"outcome verified"},
		},
		{
			name:       "partially verified with opt-in goes to human review and does not release",
			payload:    implementationCompletePayload{TaskID: testTaskID, WorkID: testWorkID, Success: true, Evidence: partiallyVerifiedJSON(t)},
			policy:     optedIn(),
			window:     evidence.CompatWindowLegacy,
			wantAction: evidence.ActionHumanReview,
			wantReason: []string{"partially_verified", "opted in", "human review", "lint skipped", "no linter configured"},
		},
		{
			name:       "partially verified without opt-in blocks",
			payload:    implementationCompletePayload{TaskID: testTaskID, WorkID: testWorkID, Success: true, Evidence: partiallyVerifiedJSON(t)},
			policy:     notOptedIn(),
			window:     evidence.CompatWindowLegacy,
			wantAction: evidence.ActionBlock,
			wantReason: []string{"partially_verified", "has not opted in"},
		},
		{
			name:       "partially verified with unknown product blocks",
			payload:    implementationCompletePayload{TaskID: testTaskID, WorkID: testWorkID, Success: true, Evidence: partiallyVerifiedJSON(t)},
			policy:     unknownProduct(),
			window:     evidence.CompatWindowStrict,
			wantAction: evidence.ActionBlock,
			wantReason: []string{"has not opted in"},
		},
		{
			name:       "failed blocks even with opt-in",
			payload:    implementationCompletePayload{TaskID: testTaskID, WorkID: testWorkID, Success: false, Reason: "tests failed on 2222222", Evidence: failedJSON(t)},
			policy:     optedIn(),
			window:     evidence.CompatWindowLegacy,
			wantAction: evidence.ActionBlock,
			wantReason: []string{"outcome failed", "tests failed on 2222222"},
		},
		{
			name:       "unverified blocks even with opt-in",
			payload:    implementationCompletePayload{TaskID: testTaskID, WorkID: testWorkID, Success: true, Evidence: unverifiedJSON(t)},
			policy:     optedIn(),
			window:     evidence.CompatWindowLegacy,
			wantAction: evidence.ActionBlock,
			wantReason: []string{"outcome unverified"},
		},
		{
			name:       "unknown schema version blocks in legacy mode",
			payload:    implementationCompletePayload{TaskID: testTaskID, WorkID: testWorkID, Success: true, Evidence: unknownSchemaJSON(t)},
			policy:     optedIn(),
			window:     evidence.CompatWindowLegacy,
			wantAction: evidence.ActionBlock,
			wantReason: []string{"unknown evidence schema version 2"},
		},
		{
			name:       "unknown schema version blocks in strict mode",
			payload:    implementationCompletePayload{TaskID: testTaskID, WorkID: testWorkID, Success: true, Evidence: unknownSchemaJSON(t)},
			policy:     optedIn(),
			window:     evidence.CompatWindowStrict,
			wantAction: evidence.ActionBlock,
			wantReason: []string{"unknown evidence schema version 2"},
		},
		{
			name:       "no evidence in legacy mode with success=true releases",
			payload:    implementationCompletePayload{TaskID: testTaskID, WorkID: testWorkID, Success: true},
			policy:     notOptedIn(),
			window:     evidence.CompatWindowLegacy,
			wantAction: evidence.ActionRelease,
			wantReason: []string{"legacy"},
		},
		{
			name:       "no evidence in legacy mode with success=false blocks with the legacy reason",
			payload:    implementationCompletePayload{TaskID: testTaskID, WorkID: testWorkID, Success: false, Reason: "LLM call failed"},
			policy:     optedIn(),
			window:     evidence.CompatWindowLegacy,
			wantAction: evidence.ActionBlock,
			wantReason: []string{"implementation failed: LLM call failed"},
		},
		{
			name:       "no evidence in strict mode blocks even with success=true",
			payload:    implementationCompletePayload{TaskID: testTaskID, WorkID: testWorkID, Success: true},
			policy:     optedIn(),
			window:     evidence.CompatWindowStrict,
			wantAction: evidence.ActionBlock,
			wantReason: []string{"no evidence attached", "strict"},
		},
		{
			name:       "no evidence in strict mode carries the producer's failure reason",
			payload:    implementationCompletePayload{TaskID: testTaskID, WorkID: testWorkID, Success: false, Reason: "LLM call failed"},
			policy:     optedIn(),
			window:     evidence.CompatWindowStrict,
			wantAction: evidence.ActionBlock,
			wantReason: []string{"no evidence attached", "LLM call failed"},
		},
		{
			name:       "explicit JSON null evidence is treated as absent",
			payload:    implementationCompletePayload{TaskID: testTaskID, WorkID: testWorkID, Success: true, Evidence: json.RawMessage("null")},
			policy:     notOptedIn(),
			window:     evidence.CompatWindowLegacy,
			wantAction: evidence.ActionRelease,
			wantReason: []string{"legacy"},
		},
		{
			name:       "empty object evidence is present and blocks in legacy mode",
			payload:    implementationCompletePayload{TaskID: testTaskID, WorkID: testWorkID, Success: true, Evidence: json.RawMessage("{}")},
			policy:     optedIn(),
			window:     evidence.CompatWindowLegacy,
			wantAction: evidence.ActionBlock,
			wantReason: []string{"no evidence attached"},
		},
		{
			name:       "malformed evidence blocks in legacy mode",
			payload:    implementationCompletePayload{TaskID: testTaskID, WorkID: testWorkID, Success: true, Evidence: json.RawMessage(`"not an object"`)},
			policy:     optedIn(),
			window:     evidence.CompatWindowLegacy,
			wantAction: evidence.ActionBlock,
			wantReason: []string{"invalid evidence"},
		},
		{
			name:       "verified evidence contradicted by success=false blocks",
			payload:    implementationCompletePayload{TaskID: testTaskID, WorkID: testWorkID, Success: false, Reason: "push failed", Evidence: verifiedJSON(t)},
			policy:     optedIn(),
			window:     evidence.CompatWindowLegacy,
			wantAction: evidence.ActionBlock,
			wantReason: []string{"success=false", "push failed"},
		},
		{
			name:       "verified evidence for another task blocks and releases nothing (identity mismatch)",
			payload:    implementationCompletePayload{TaskID: testTaskID, WorkID: testWorkID, Success: true, Evidence: foreignTaskJSON(t)},
			policy:     optedIn(),
			window:     evidence.CompatWindowStrict,
			wantAction: evidence.ActionBlock,
			wantReason: []string{"does not belong", "task_id", otherID, testTaskID},
		},
		{
			name:       "verified evidence for another work item blocks (identity mismatch)",
			payload:    implementationCompletePayload{TaskID: testTaskID, WorkID: otherID, Success: true, Evidence: verifiedJSON(t)},
			policy:     optedIn(),
			window:     evidence.CompatWindowLegacy,
			wantAction: evidence.ActionBlock,
			wantReason: []string{"does not belong", "work_id", testWorkID, otherID},
		},
		{
			name:         "verified evidence for another repository blocks and releases nothing (identity mismatch)",
			payload:      implementationCompletePayload{TaskID: testTaskID, WorkID: testWorkID, Success: true, Evidence: repoJSON(t, repoA)},
			policy:       optedIn(),
			window:       evidence.CompatWindowStrict,
			repositoryID: repoIDPtr(repoB),
			wantAction:   evidence.ActionBlock,
			wantReason:   []string{"does not belong", "repository_id", repoA, repoB},
		},
		{
			name:         "verified evidence for the task's own repository releases",
			payload:      implementationCompletePayload{TaskID: testTaskID, WorkID: testWorkID, Success: true, Evidence: repoJSON(t, repoA)},
			policy:       notOptedIn(),
			window:       evidence.CompatWindowStrict,
			repositoryID: repoIDPtr(repoA),
			wantAction:   evidence.ActionRelease,
			wantReason:   []string{"outcome verified"},
		},
		{
			name:         "verified evidence without a repository is not repository-checked",
			payload:      implementationCompletePayload{TaskID: testTaskID, WorkID: testWorkID, Success: true, Evidence: verifiedJSON(t)},
			policy:       notOptedIn(),
			window:       evidence.CompatWindowStrict,
			repositoryID: repoIDPtr(repoA),
			wantAction:   evidence.ActionRelease,
			wantReason:   []string{"outcome verified"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := decideDependentReleaseFor(tc.payload, tc.policy, tc.window, tc.repositoryID)
			if d.Action != tc.wantAction {
				t.Fatalf("action = %q, want %q (reason %q)", d.Action, tc.wantAction, d.Reason)
			}
			if strings.TrimSpace(d.Reason) == "" {
				t.Fatalf("reason must be non-empty")
			}
			for _, want := range tc.wantReason {
				if !strings.Contains(d.Reason, want) {
					t.Errorf("reason %q does not contain %q", d.Reason, want)
				}
			}
		})
	}
}

// TestDecideDependentReleaseOnlyVerifiedReleases sweeps every outcome under
// both policies and both windows: release is produced for verified only,
// and human_review for partially_verified with opt-in only.
func TestDecideDependentReleaseOnlyVerifiedReleases(t *testing.T) {
	fixtures := map[evidence.Outcome]json.RawMessage{
		evidence.OutcomeVerified:          verifiedJSON(t),
		evidence.OutcomePartiallyVerified: partiallyVerifiedJSON(t),
		evidence.OutcomeFailed:            failedJSON(t),
		evidence.OutcomeUnverified:        unverifiedJSON(t),
	}
	for outcome, raw := range fixtures {
		for _, policy := range []evidence.ReleasePolicy{unknownProduct(), notOptedIn(), optedIn()} {
			for _, window := range []evidence.CompatWindow{evidence.CompatWindowLegacy, evidence.CompatWindowStrict} {
				success := outcome != evidence.OutcomeFailed
				d := decideDependentRelease(implementationCompletePayload{TaskID: testTaskID, WorkID: testWorkID, Success: success, Evidence: raw}, policy, window)
				optIn := policy.PartialVerificationOptIn
				want := evidence.ActionBlock
				switch {
				case outcome == evidence.OutcomeVerified:
					want = evidence.ActionRelease
				case outcome == evidence.OutcomePartiallyVerified && optIn:
					want = evidence.ActionHumanReview
				}
				if d.Action != want {
					t.Errorf("outcome=%s optIn=%v window=%s: action = %q, want %q", outcome, optIn, window, d.Action, want)
				}

				// The same record on a message for another task or work
				// item never releases dependents or reaches human review.
				for name, p := range map[string]implementationCompletePayload{
					"other task":      {TaskID: otherID, WorkID: testWorkID, Success: success, Evidence: raw},
					"other work item": {TaskID: testTaskID, WorkID: otherID, Success: success, Evidence: raw},
					"no ids":          {Success: success, Evidence: raw},
				} {
					if d := decideDependentRelease(p, policy, window); d.Action != evidence.ActionBlock {
						t.Errorf("%s: outcome=%s optIn=%v window=%s: action = %q, want block (reason %q)", name, outcome, optIn, window, d.Action, d.Reason)
					}
				}
			}
		}
	}
}

func TestHasEvidence(t *testing.T) {
	cases := []struct {
		raw  string
		want bool
	}{
		{"", false},
		{"   ", false},
		{"null", false},
		{" null ", false},
		{"{}", true},
		{`{"schema_version":1}`, true},
		{`"x"`, true},
	}
	for _, tc := range cases {
		p := implementationCompletePayload{Evidence: json.RawMessage(tc.raw)}
		if got := p.hasEvidence(); got != tc.want {
			t.Errorf("hasEvidence(%q) = %v, want %v", tc.raw, got, tc.want)
		}
	}
}
