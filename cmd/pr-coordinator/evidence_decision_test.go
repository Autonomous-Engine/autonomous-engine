package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/evidence"
	"github.com/autonomousengine/autonomous-engine/internal/models"
	"github.com/autonomousengine/autonomous-engine/internal/products"
	"github.com/google/uuid"
)

const (
	testWorkID  = "0f1e2d3c-0000-4000-8000-000000000001"
	testTaskID  = "0f1e2d3c-0000-4000-8000-000000000002"
	testHeadSHA = "2222222222222222222222222222222222222222"
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

// storedIdentity is the expectation decideUnseenSubtask passes for the
// fixture subtask when its repository is unknown.
func storedIdentity() evidence.MessageIdentity {
	return evidence.MessageIdentity{WorkID: testWorkID, TaskID: testTaskID}
}

// Two repositories: the subtask's own and another one.
const (
	repoA = "0f1e2d3c-0000-4000-8000-0000000000e1"
	repoB = "0f1e2d3c-0000-4000-8000-0000000000e2"
)

func repoIDPtr(s string) *string { return &s }

// repoJSON is a valid verified record that names repositoryID.
func repoJSON(t *testing.T, repositoryID string) json.RawMessage {
	return evidenceJSON(t, true, func(ev *evidence.ImplementationEvidence) { ev.RepositoryID = repoIDPtr(repositoryID) })
}

// plannerFiles is what the planner-sourced files_changed field carries. It
// deliberately differs from the evidence's changed_files so a test can tell
// which source a decision used.
var plannerFiles = []string{"planner_guess.go", "stale.go"}

// baseEvidence is a valid schema-1 verified record: one passed test check
// against a concrete head SHA, with git-derived changed files that differ
// from plannerFiles.
func baseEvidence() evidence.ImplementationEvidence {
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	exit := 0
	return evidence.ImplementationEvidence{
		SchemaVersion: evidence.SchemaVersion,
		WorkID:        testWorkID,
		TaskID:        testTaskID,
		BaseSHA:       "1111111111111111111111111111111111111111",
		HeadSHA:       testHeadSHA,
		Branch:        "agent/work/task",
		ChangedFiles:  []string{"internal/real.go", "cmd/real_test.go"},
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

func unknownSchemaJSON(t *testing.T) json.RawMessage {
	return evidenceJSON(t, false, func(ev *evidence.ImplementationEvidence) {
		ev.SchemaVersion = evidence.SchemaVersion + 1
	})
}

func optedIn() evidence.ReleasePolicy    { return evidence.ReleasePolicy{PartialVerificationOptIn: true} }
func notOptedIn() evidence.ReleasePolicy { return evidence.ReleasePolicy{} }

// payloadWith builds a message whose legacy fields always carry plannerFiles
// and success=true unless overridden, so every case exercises the
// "evidence beats planner" rule.
func payloadWith(ev json.RawMessage, success bool) implementationCompletePayload {
	return implementationCompletePayload{
		TaskID:       testTaskID,
		WorkID:       testWorkID,
		FilesChanged: append([]string{}, plannerFiles...),
		Summary:      "Add the widget",
		Success:      success,
		Evidence:     ev,
	}
}

func equalStrings(a, b []string) bool {
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

func TestDecideSubtaskPR(t *testing.T) {
	evidenceFiles := baseEvidence().ChangedFiles
	cases := []struct {
		name    string
		payload implementationCompletePayload
		policy  evidence.ReleasePolicy
		window  evidence.CompatWindow
		// repositoryID is the subtask's repository from its work item row
		// (nil: unknown, not checked), as loadPolicyForTask returns it.
		repositoryID *string
		wantAction   evidence.ReleaseAction
		wantLabel    string
		wantSHA      string
		wantFiles    []string
		wantLegacy   bool
		wantReason   []string // substrings that must appear in the reason
	}{
		{
			name:       "verified releases and takes files and SHA from evidence",
			payload:    payloadWith(verifiedJSON(t), true),
			policy:     notOptedIn(),
			window:     evidence.CompatWindowLegacy,
			wantAction: evidence.ActionRelease,
			wantLabel:  outcomeLabelVerified,
			wantSHA:    testHeadSHA,
			wantFiles:  evidenceFiles,
			wantReason: []string{"outcome verified", testHeadSHA},
		},
		{
			name:       "verified releases in strict mode too",
			payload:    payloadWith(verifiedJSON(t), true),
			policy:     evidence.ReleasePolicy{},
			window:     evidence.CompatWindowStrict,
			wantAction: evidence.ActionRelease,
			wantLabel:  outcomeLabelVerified,
			wantSHA:    testHeadSHA,
			wantFiles:  evidenceFiles,
		},
		{
			name:       "verified evidence with success=false is contradictory and blocks",
			payload:    payloadWith(verifiedJSON(t), false),
			policy:     optedIn(),
			window:     evidence.CompatWindowLegacy,
			wantAction: evidence.ActionBlock,
			wantLabel:  outcomeLabelBlocked,
			wantSHA:    testHeadSHA,
			wantFiles:  evidenceFiles,
			wantReason: []string{"success=false"},
		},
		{
			name:       "partially verified with opt-in requires human review",
			payload:    payloadWith(partiallyVerifiedJSON(t), true),
			policy:     optedIn(),
			window:     evidence.CompatWindowLegacy,
			wantAction: evidence.ActionHumanReview,
			wantLabel:  outcomeLabelPartiallyVerified,
			wantSHA:    testHeadSHA,
			wantFiles:  evidenceFiles,
			wantReason: []string{"partially_verified", "human review", "lint skipped", "no linter configured"},
		},
		{
			name:       "partially verified without opt-in blocks",
			payload:    payloadWith(partiallyVerifiedJSON(t), true),
			policy:     notOptedIn(),
			window:     evidence.CompatWindowLegacy,
			wantAction: evidence.ActionBlock,
			wantLabel:  outcomeLabelBlocked,
			wantSHA:    testHeadSHA,
			wantFiles:  evidenceFiles,
			wantReason: []string{"has not opted in"},
		},
		{
			name:       "partially verified with unknown product blocks",
			payload:    payloadWith(partiallyVerifiedJSON(t), true),
			policy:     evidence.ReleasePolicy{},
			window:     evidence.CompatWindowStrict,
			wantAction: evidence.ActionBlock,
			wantLabel:  outcomeLabelBlocked,
			wantSHA:    testHeadSHA,
			wantFiles:  evidenceFiles,
		},
		{
			name:       "failed evidence blocks with the failed label",
			payload:    payloadWith(failedJSON(t), false),
			policy:     optedIn(),
			window:     evidence.CompatWindowLegacy,
			wantAction: evidence.ActionBlock,
			wantLabel:  outcomeLabelFailed,
			wantSHA:    testHeadSHA,
			wantFiles:  evidenceFiles,
			wantReason: []string{"outcome failed", "tests failed on 2222222"},
		},
		{
			name:       "failed evidence with success=true still blocks",
			payload:    payloadWith(failedJSON(t), true),
			policy:     optedIn(),
			window:     evidence.CompatWindowLegacy,
			wantAction: evidence.ActionBlock,
			wantLabel:  outcomeLabelFailed,
			wantSHA:    testHeadSHA,
			wantFiles:  evidenceFiles,
		},
		{
			// The record still decodes, so the SHA and files are carried
			// for the log; the block means no PR is opened from them.
			name:       "unknown schema version blocks in legacy mode",
			payload:    payloadWith(unknownSchemaJSON(t), true),
			policy:     optedIn(),
			window:     evidence.CompatWindowLegacy,
			wantAction: evidence.ActionBlock,
			wantLabel:  outcomeLabelBlocked,
			wantSHA:    testHeadSHA,
			wantFiles:  evidenceFiles,
			wantReason: []string{"unknown evidence schema version"},
		},
		{
			name:       "unknown schema version blocks in strict mode",
			payload:    payloadWith(unknownSchemaJSON(t), true),
			policy:     optedIn(),
			window:     evidence.CompatWindowStrict,
			wantAction: evidence.ActionBlock,
			wantLabel:  outcomeLabelBlocked,
			wantSHA:    testHeadSHA,
			wantFiles:  evidenceFiles,
			wantReason: []string{"unknown evidence schema version"},
		},
		{
			name:       "malformed evidence blocks and yields no files",
			payload:    payloadWith(json.RawMessage(`{"schema_version":`), true),
			policy:     optedIn(),
			window:     evidence.CompatWindowLegacy,
			wantAction: evidence.ActionBlock,
			wantLabel:  outcomeLabelBlocked,
			wantSHA:    "",
			wantFiles:  nil,
			wantReason: []string{"invalid evidence"},
		},
		{
			name:       "empty object is no evidence and blocks even in legacy mode",
			payload:    payloadWith(json.RawMessage(`{}`), true),
			policy:     optedIn(),
			window:     evidence.CompatWindowLegacy,
			wantAction: evidence.ActionBlock,
			wantLabel:  outcomeLabelBlocked,
			wantFiles:  nil,
			wantReason: []string{"no evidence attached"},
		},
		{
			name:       "no evidence, legacy window, success falls back to planner files",
			payload:    payloadWith(nil, true),
			policy:     notOptedIn(),
			window:     evidence.CompatWindowLegacy,
			wantAction: evidence.ActionRelease,
			wantLabel:  outcomeLabelLegacy,
			wantSHA:    "",
			wantFiles:  plannerFiles,
			wantLegacy: true,
			wantReason: []string{"legacy message without evidence"},
		},
		{
			name:       "no evidence, legacy window, failure blocks",
			payload:    withReason(payloadWith(nil, false), "build broke"),
			policy:     notOptedIn(),
			window:     evidence.CompatWindowLegacy,
			wantAction: evidence.ActionBlock,
			wantLabel:  outcomeLabelFailed,
			wantFiles:  plannerFiles,
			wantLegacy: true,
			wantReason: []string{"implementation failed", "build broke"},
		},
		{
			name:       "no evidence, strict window, success blocks",
			payload:    payloadWith(nil, true),
			policy:     optedIn(),
			window:     evidence.CompatWindowStrict,
			wantAction: evidence.ActionBlock,
			wantLabel:  outcomeLabelBlocked,
			wantFiles:  nil,
			wantReason: []string{"no evidence attached", "EVIDENCE_COMPAT_WINDOW=strict"},
		},
		{
			name:       "JSON null evidence is no evidence",
			payload:    payloadWith(json.RawMessage(`null`), true),
			policy:     optedIn(),
			window:     evidence.CompatWindowStrict,
			wantAction: evidence.ActionBlock,
			wantLabel:  outcomeLabelBlocked,
			wantReason: []string{"no evidence attached"},
		},
		{
			// A record for another subtask is not this message's evidence:
			// no PR, and its SHA and files are not carried anywhere, not
			// even into the log line, so nothing downstream can pick them
			// up as this subtask's facts.
			name:       "verified evidence for another task blocks and carries no SHA or files (identity mismatch)",
			payload:    payloadWith(foreignTaskJSON(t), true),
			policy:     optedIn(),
			window:     evidence.CompatWindowStrict,
			wantAction: evidence.ActionBlock,
			wantLabel:  outcomeLabelBlocked,
			wantSHA:    "",
			wantFiles:  nil,
			wantReason: []string{"does not belong", "task_id", otherID, testTaskID},
		},
		{
			name:       "verified evidence for another work item blocks (identity mismatch)",
			payload:    withWorkID(payloadWith(verifiedJSON(t), true), otherID),
			policy:     optedIn(),
			window:     evidence.CompatWindowLegacy,
			wantAction: evidence.ActionBlock,
			wantLabel:  outcomeLabelBlocked,
			wantSHA:    "",
			wantFiles:  nil,
			wantReason: []string{"does not belong", "work_id", testWorkID, otherID},
		},
		{
			name:         "verified evidence for another repository blocks and carries no SHA or files (identity mismatch)",
			payload:      payloadWith(repoJSON(t, repoA), true),
			policy:       optedIn(),
			window:       evidence.CompatWindowStrict,
			repositoryID: repoIDPtr(repoB),
			wantAction:   evidence.ActionBlock,
			wantLabel:    outcomeLabelBlocked,
			wantSHA:      "",
			wantFiles:    nil,
			wantReason:   []string{"does not belong", "repository_id", repoA, repoB},
		},
		{
			name:         "verified evidence for the subtask's own repository releases with its SHA and files",
			payload:      payloadWith(repoJSON(t, repoA), true),
			policy:       notOptedIn(),
			window:       evidence.CompatWindowStrict,
			repositoryID: repoIDPtr(repoA),
			wantAction:   evidence.ActionRelease,
			wantLabel:    outcomeLabelVerified,
			wantSHA:      testHeadSHA,
			wantFiles:    evidenceFiles,
			wantReason:   []string{"outcome verified"},
		},
		{
			name:         "verified evidence without a repository is not repository-checked",
			payload:      payloadWith(verifiedJSON(t), true),
			policy:       notOptedIn(),
			window:       evidence.CompatWindowStrict,
			repositoryID: repoIDPtr(repoA),
			wantAction:   evidence.ActionRelease,
			wantLabel:    outcomeLabelVerified,
			wantSHA:      testHeadSHA,
			wantFiles:    evidenceFiles,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := decideSubtaskPRFor(tc.payload, tc.policy, tc.window, tc.repositoryID)
			if got.Action != tc.wantAction {
				t.Fatalf("action = %q, want %q (reason %q)", got.Action, tc.wantAction, got.Reason)
			}
			if got.Label != tc.wantLabel {
				t.Errorf("label = %q, want %q", got.Label, tc.wantLabel)
			}
			if got.TaskID != testTaskID {
				t.Errorf("task id = %q, want %q", got.TaskID, testTaskID)
			}
			if got.Reason == "" {
				t.Errorf("reason is empty")
			}
			for _, want := range tc.wantReason {
				if !strings.Contains(got.Reason, want) {
					t.Errorf("reason %q does not contain %q", got.Reason, want)
				}
			}
			if got.HeadSHA != tc.wantSHA {
				t.Errorf("head sha = %q, want %q", got.HeadSHA, tc.wantSHA)
			}
			if !equalStrings(got.ChangedFiles, tc.wantFiles) {
				t.Errorf("changed files = %v, want %v", got.ChangedFiles, tc.wantFiles)
			}
			if got.Legacy != tc.wantLegacy {
				t.Errorf("legacy = %v, want %v", got.Legacy, tc.wantLegacy)
			}
		})
	}
}

func withReason(p implementationCompletePayload, reason string) implementationCompletePayload {
	p.Reason = reason
	return p
}

func withWorkID(p implementationCompletePayload, workID string) implementationCompletePayload {
	p.WorkID = workID
	return p
}

// TestForeignEvidenceNeverOpensPR sweeps every outcome, policy and window
// with a record whose ids do not match the message: the outcome is always
// block and the foreign record's SHA and files are never carried.
func TestForeignEvidenceNeverOpensPR(t *testing.T) {
	fixtures := map[string]json.RawMessage{
		"verified":           verifiedJSON(t),
		"partially_verified": partiallyVerifiedJSON(t),
		"failed":             failedJSON(t),
		"unverified": evidenceJSON(t, true, func(ev *evidence.ImplementationEvidence) {
			ev.Checks = []evidence.CheckResult{{Kind: evidence.CheckKindTest, Status: evidence.CheckSkipped, SkipReason: "no runner"}}
			ev.Outcome = evidence.OutcomeUnverified
		}),
	}
	for name, raw := range fixtures {
		for _, policy := range []evidence.ReleasePolicy{{}, optedIn()} {
			for _, w := range []evidence.CompatWindow{evidence.CompatWindowLegacy, evidence.CompatWindowStrict} {
				for label, p := range map[string]implementationCompletePayload{
					"other task":      withTaskID(payloadWith(raw, true), otherID),
					"other work item": withWorkID(payloadWith(raw, true), otherID),
					"no ids":          {Success: true, Evidence: raw},
				} {
					got := decideSubtaskPR(p, policy, w)
					if got.Action != evidence.ActionBlock {
						t.Errorf("%s/%s (opt_in=%v, window=%s): action = %s, want block: %s", name, label, policy.PartialVerificationOptIn, w, got.Action, got.Reason)
					}
					if got.HeadSHA != "" || got.ChangedFiles != nil {
						t.Errorf("%s/%s: foreign record's facts carried: sha=%q files=%v", name, label, got.HeadSHA, got.ChangedFiles)
					}
					if got.Label == outcomeLabelVerified || got.Label == outcomeLabelPartiallyVerified {
						t.Errorf("%s/%s: labelled %q", name, label, got.Label)
					}
				}
			}
		}
	}
}

func withTaskID(p implementationCompletePayload, taskID string) implementationCompletePayload {
	p.TaskID = taskID
	return p
}

// TestFilesAndSHAComeFromEvidenceNotPlanner pins D1.4/D1.5: whenever a
// decodable evidence record is present, the planner's files_changed is
// ignored entirely — it is never merged in, even when it lists extra files.
func TestFilesAndSHAComeFromEvidenceNotPlanner(t *testing.T) {
	for _, ev := range []json.RawMessage{verifiedJSON(t), partiallyVerifiedJSON(t), failedJSON(t)} {
		got := decideSubtaskPR(payloadWith(ev, true), optedIn(), evidence.CompatWindowLegacy)
		if !equalStrings(got.ChangedFiles, baseEvidence().ChangedFiles) {
			t.Errorf("changed files = %v, want evidence files %v", got.ChangedFiles, baseEvidence().ChangedFiles)
		}
		for _, f := range plannerFiles {
			for _, g := range got.ChangedFiles {
				if f == g {
					t.Errorf("planner file %q leaked into changed files %v", f, got.ChangedFiles)
				}
			}
		}
		if got.HeadSHA != testHeadSHA {
			t.Errorf("head sha = %q, want %q", got.HeadSHA, testHeadSHA)
		}
	}
	// Evidence whose git diff was empty stays empty: the planner's list is
	// not a substitute for git.
	empty := evidenceJSON(t, true, func(ev *evidence.ImplementationEvidence) { ev.ChangedFiles = nil })
	got := decideSubtaskPR(payloadWith(empty, true), optedIn(), evidence.CompatWindowLegacy)
	if len(got.ChangedFiles) != 0 {
		t.Errorf("changed files = %v, want none (evidence had none)", got.ChangedFiles)
	}
}

// TestOnlyVerifiedReleases is the positive allow-list check for this
// consumer (D1.2): every outcome except verified is non-release.
func TestOnlyVerifiedReleases(t *testing.T) {
	outcomes := []struct {
		name string
		ev   json.RawMessage
	}{
		{"partially_verified", partiallyVerifiedJSON(t)},
		{"failed", failedJSON(t)},
		{"unknown schema", unknownSchemaJSON(t)},
		{"unverified", evidenceJSON(t, true, func(ev *evidence.ImplementationEvidence) {
			ev.Checks = []evidence.CheckResult{{Kind: evidence.CheckKindTest, Status: evidence.CheckSkipped, SkipReason: "no runner"}}
			ev.Outcome = evidence.OutcomeUnverified
		})},
	}
	for _, o := range outcomes {
		for _, policy := range []evidence.ReleasePolicy{{}, optedIn(), notOptedIn()} {
			for _, w := range []evidence.CompatWindow{evidence.CompatWindowLegacy, evidence.CompatWindowStrict} {
				got := decideSubtaskPR(payloadWith(o.ev, true), policy, w)
				if got.Action == evidence.ActionRelease {
					t.Errorf("%s (opt_in=%v, window=%s) released: %s", o.name, policy.PartialVerificationOptIn, w, got.Reason)
				}
				if got.Label == outcomeLabelVerified {
					t.Errorf("%s labelled verified", o.name)
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
		{"garbage", true},
	}
	for _, tc := range cases {
		p := implementationCompletePayload{Evidence: json.RawMessage(tc.raw)}
		if got := p.hasEvidence(); got != tc.want {
			t.Errorf("hasEvidence(%q) = %v, want %v", tc.raw, got, tc.want)
		}
	}
}

func TestPRTitleFor(t *testing.T) {
	verified := decideSubtaskPR(payloadWith(verifiedJSON(t), true), notOptedIn(), evidence.CompatWindowLegacy)
	partial := decideSubtaskPR(payloadWith(partiallyVerifiedJSON(t), true), optedIn(), evidence.CompatWindowLegacy)

	if got := prTitleFor("[repo] Add widget", []subtaskOutcome{verified}); got != "[repo] Add widget" {
		t.Errorf("verified title = %q, want unchanged", got)
	}
	got := prTitleFor("[repo] Add widget", []subtaskOutcome{partial})
	if !strings.HasPrefix(got, partialVerificationTitlePrefix) {
		t.Errorf("partial title = %q, want prefix %q", got, partialVerificationTitlePrefix)
	}
	if strings.Count(got, partialVerificationTitlePrefix) != 1 || !strings.HasSuffix(got, "[repo] Add widget") {
		t.Errorf("partial title = %q, want exactly one prefix and the base title", got)
	}
	// Idempotent: a base that already carries the prefix is not double-marked.
	if again := prTitleFor(got, []subtaskOutcome{partial}); again != got {
		t.Errorf("re-prefixed title = %q, want %q", again, got)
	}
	// Mixed: any partial subtask marks the PR.
	if mixed := prTitleFor("x", []subtaskOutcome{verified, partial}); !strings.HasPrefix(mixed, partialVerificationTitlePrefix) {
		t.Errorf("mixed title = %q, want prefix", mixed)
	}
	if requiresHumanReview([]subtaskOutcome{verified}) {
		t.Errorf("verified requires human review")
	}
	if !requiresHumanReview([]subtaskOutcome{verified, partial}) {
		t.Errorf("mixed does not require human review")
	}
}

func TestRenderPRBody(t *testing.T) {
	verified := decideSubtaskPR(payloadWith(verifiedJSON(t), true), notOptedIn(), evidence.CompatWindowLegacy)
	verified.Summary = "Wire the verified thing"

	t.Run("verified body has outcome, SHA, evidence files and no partial marker", func(t *testing.T) {
		body := renderPRBody(prBodyInput{
			Title:      "[repo] Add widget",
			Branch:     "agent/w/t",
			BaseBranch: "main",
			DiffStat:   " internal/real.go | 2 +-\n",
			Outcomes:   []subtaskOutcome{verified},
		})
		for _, want := range []string{
			"## Summary", "[repo] Add widget", "Branch: `agent/w/t`", "Base branch: `main`",
			"Verified head SHA: `" + testHeadSHA + "`",
			"## Subtasks", "Wire the verified thing", "**verified**",
			"## Changes", "internal/real.go | 2 +-",
			"## Changed files", "- `internal/real.go`", "- `cmd/real_test.go`",
			"verification evidence",
		} {
			if !strings.Contains(body, want) {
				t.Errorf("body missing %q:\n%s", want, body)
			}
		}
		if strings.Contains(body, partialVerificationBodyMarker) {
			t.Errorf("verified body carries the partial marker:\n%s", body)
		}
		for _, f := range plannerFiles {
			if strings.Contains(body, f) {
				t.Errorf("planner file %q leaked into body:\n%s", f, body)
			}
		}
	})

	t.Run("partially verified body carries the marker, reason and SHA", func(t *testing.T) {
		partial := decideSubtaskPR(payloadWith(partiallyVerifiedJSON(t), true), optedIn(), evidence.CompatWindowLegacy)
		partial.Summary = "Add the partially checked thing"
		body := renderPRBody(prBodyInput{Title: "t", Branch: "b", BaseBranch: "main", Outcomes: []subtaskOutcome{partial}})
		for _, want := range []string{
			partialVerificationBodyMarker,
			"human review",
			"Add the partially checked thing", "**partially verified**",
			testHeadSHA,
			"lint skipped (no linter configured)",
		} {
			if !strings.Contains(body, want) {
				t.Errorf("body missing %q:\n%s", want, body)
			}
		}
		// The marker must be the first thing a reviewer sees.
		if !strings.HasPrefix(body, "> **"+partialVerificationBodyMarker+"**") {
			t.Errorf("body does not open with the marker:\n%s", body)
		}
	})

	t.Run("failed subtask is labelled failed and mixed PR shows every outcome", func(t *testing.T) {
		failed := decideSubtaskPR(payloadWith(failedJSON(t), false), optedIn(), evidence.CompatWindowLegacy)
		failed.TaskID = "0f1e2d3c-0000-4000-8000-000000000003"
		failed.Summary = "Broke the tests"
		body := renderPRBody(prBodyInput{Title: "t", Branch: "b", BaseBranch: "main", Outcomes: []subtaskOutcome{verified, failed}})
		for _, want := range []string{"**verified**", "**failed**", "Broke the tests", "tests failed on 2222222"} {
			if !strings.Contains(body, want) {
				t.Errorf("body missing %q:\n%s", want, body)
			}
		}
	})

	t.Run("legacy subtask says the files are not git-verified and has no SHA", func(t *testing.T) {
		legacy := decideSubtaskPR(payloadWith(nil, true), evidence.ReleasePolicy{}, evidence.CompatWindowLegacy)
		body := renderPRBody(prBodyInput{Title: "t", Branch: "b", BaseBranch: "main", Outcomes: []subtaskOutcome{legacy}})
		for _, want := range []string{"**" + outcomeLabelLegacy + "**", "not git-verified", "- `planner_guess.go`"} {
			if !strings.Contains(body, want) {
				t.Errorf("body missing %q:\n%s", want, body)
			}
		}
		if strings.Contains(body, "Verified head SHA") {
			t.Errorf("legacy body claims a verified head SHA:\n%s", body)
		}
	})

	t.Run("diff is truncated for the PR body", func(t *testing.T) {
		big := strings.Repeat("+line\n", 2000)
		body := renderPRBody(prBodyInput{Title: "t", Branch: "b", BaseBranch: "main", Diff: big, Outcomes: []subtaskOutcome{verified}})
		if !strings.Contains(body, "(truncated)") {
			t.Errorf("long diff not truncated")
		}
		if len(body) > maxDiffInBody+2000 {
			t.Errorf("body length %d far exceeds the diff cap %d", len(body), maxDiffInBody)
		}
	})
}

func TestChangedFilesForAndHeadSHA(t *testing.T) {
	a := subtaskOutcome{ChangedFiles: []string{"b.go", "a.go"}, HeadSHA: "aaa"}
	b := subtaskOutcome{ChangedFiles: []string{"a.go", "c.go"}, HeadSHA: "aaa"}
	c := subtaskOutcome{ChangedFiles: nil, HeadSHA: "bbb"}

	if got := changedFilesFor([]subtaskOutcome{a, b}); !equalStrings(got, []string{"a.go", "b.go", "c.go"}) {
		t.Errorf("changedFilesFor = %v, want deduplicated sorted union", got)
	}
	if got := changedFilesFor(nil); got == nil || len(got) != 0 {
		t.Errorf("changedFilesFor(nil) = %v, want empty non-nil slice", got)
	}
	if got, ok := verifiedHeadSHA([]subtaskOutcome{a, b}); !ok || got != "aaa" {
		t.Errorf("verifiedHeadSHA agreeing = (%q, %v), want (aaa, true)", got, ok)
	}
	if got, ok := verifiedHeadSHA([]subtaskOutcome{a, c}); ok || got != "" {
		t.Errorf("verifiedHeadSHA disagreeing = (%q, %v), want (\"\", false)", got, ok)
	}
	if got, ok := verifiedHeadSHA([]subtaskOutcome{{}}); ok || got != "" {
		t.Errorf("verifiedHeadSHA none = (%q, %v), want (\"\", false)", got, ok)
	}
}

func TestOutcomeForStoredEvidence(t *testing.T) {
	// A passed subtask whose implementation.complete event was not seen
	// (coordinator restart) is decided from the persisted record: the same
	// rules apply, files and SHA still come from evidence.
	partial := baseEvidence()
	partial.Checks = append(partial.Checks, evidence.CheckResult{Kind: evidence.CheckKindLint, Status: evidence.CheckSkipped, SkipReason: "no linter"})
	partial.Outcome = evidence.OutcomePartiallyVerified
	if err := partial.Validate(); err != nil {
		t.Fatalf("fixture: %v", err)
	}

	got := decideSubtaskPRFromStored(&partial, optedIn(), storedIdentity())
	if got.Action != evidence.ActionHumanReview || got.Label != outcomeLabelPartiallyVerified {
		t.Errorf("stored partial opted in = (%s, %s), want human_review/partially verified", got.Action, got.Label)
	}
	if got.HeadSHA != testHeadSHA || !equalStrings(got.ChangedFiles, baseEvidence().ChangedFiles) {
		t.Errorf("stored outcome did not carry evidence SHA/files: %+v", got)
	}
	if got.TaskID != testTaskID {
		t.Errorf("stored outcome task id = %q, want %q", got.TaskID, testTaskID)
	}
	if got := decideSubtaskPRFromStored(&partial, evidence.ReleasePolicy{}, storedIdentity()); got.Action != evidence.ActionBlock {
		t.Errorf("stored partial without opt-in = %s, want block", got.Action)
	}
	if got := decideSubtaskPRFromStored(nil, optedIn(), storedIdentity()); got.Action != evidence.ActionBlock {
		t.Errorf("nil stored evidence = %s, want block", got.Action)
	}

	// The stored record must belong to the subtask being decided: a record
	// for another task, work item, or repository blocks and carries nothing.
	repoX, repoY := "0f1e2d3c-0000-4000-8000-0000000000e1", "0f1e2d3c-0000-4000-8000-0000000000e2"
	withRepo := partial
	withRepo.RepositoryID = &repoX
	for name, tc := range map[string]struct {
		ev     *evidence.ImplementationEvidence
		expect evidence.MessageIdentity
		want   string // reason substring
	}{
		"other task":      {&partial, evidence.MessageIdentity{WorkID: testWorkID, TaskID: otherID}, "task_id"},
		"other work item": {&partial, evidence.MessageIdentity{WorkID: otherID, TaskID: testTaskID}, "work_id"},
		"other repo":      {&withRepo, evidence.MessageIdentity{WorkID: testWorkID, TaskID: testTaskID, RepositoryID: &repoY}, "repository_id"},
	} {
		got := decideSubtaskPRFromStored(tc.ev, optedIn(), tc.expect)
		if got.Action != evidence.ActionBlock || !strings.Contains(got.Reason, "does not belong") || !strings.Contains(got.Reason, tc.want) {
			t.Errorf("%s: got %+v, want identity block naming %s", name, got, tc.want)
		}
		if got.HeadSHA != "" || got.ChangedFiles != nil {
			t.Errorf("%s: foreign stored record's facts carried: %+v", name, got)
		}
		if got.TaskID != tc.expect.TaskID {
			t.Errorf("%s: task id = %q, want the expected %q, not the record's", name, got.TaskID, tc.expect.TaskID)
		}
	}
	// Same repository on both sides, or unknown on either, is not a mismatch.
	if got := decideSubtaskPRFromStored(&withRepo, optedIn(), evidence.MessageIdentity{WorkID: testWorkID, TaskID: testTaskID, RepositoryID: &repoX}); got.Action != evidence.ActionHumanReview {
		t.Errorf("same repo: got %+v, want human_review", got)
	}
	if got := decideSubtaskPRFromStored(&withRepo, optedIn(), storedIdentity()); got.Action != evidence.ActionHumanReview {
		t.Errorf("repo unknown on the subtask: got %+v, want human_review", got)
	}
	if got := decideSubtaskPRFromStored(&partial, optedIn(), evidence.MessageIdentity{WorkID: testWorkID, TaskID: testTaskID, RepositoryID: &repoY}); got.Action != evidence.ActionHumanReview {
		t.Errorf("repo unknown on the record: got %+v, want human_review", got)
	}
}

func TestOutcomeForUnseenSubtask(t *testing.T) {
	// No completion event and no stored evidence: work-item state alone is
	// never evidence, so this blocks regardless of the compatibility window
	// (which only governs how a message without evidence is read).
	got := outcomeForUnseenSubtask(testTaskID)
	if got.Action != evidence.ActionBlock || got.Label != outcomeLabelBlocked || got.Legacy {
		t.Errorf("unseen = %+v, want block", got)
	}
	for _, want := range []string{"no completion event", "no stored evidence", testTaskID, "cannot open a PR"} {
		if !strings.Contains(got.Reason, want) {
			t.Errorf("reason %q does not contain %q", got.Reason, want)
		}
	}
	if got.TaskID != testTaskID {
		t.Errorf("task id = %q, want %q", got.TaskID, testTaskID)
	}
}

// Fakes for the restart fallback: an in-memory work-item lookup, product
// store and evidence store.
type fakeItems map[uuid.UUID]*models.WorkItem

func (f fakeItems) GetWorkItem(_ context.Context, id uuid.UUID) (*models.WorkItem, error) {
	if it, ok := f[id]; ok {
		return it, nil
	}
	return nil, errors.New("work item not found")
}

type fakeProducts map[uuid.UUID]*products.Product

func (f fakeProducts) GetProduct(_ context.Context, id uuid.UUID) (*products.Product, error) {
	if p, ok := f[id]; ok {
		return p, nil
	}
	return nil, errors.New("product not found")
}

type fakeEvidence struct {
	rows []evidence.Verification
	err  error
}

func (f fakeEvidence) GetByWorkItem(_ context.Context, _ uuid.UUID) ([]evidence.Verification, error) {
	return f.rows, f.err
}

func TestDecideUnseenSubtaskUsesEachSiblingsOwnPolicy(t *testing.T) {
	// Two sibling subtasks of one parent, on different products: A's
	// product has opted in to partial verification, B's has not. Both have
	// stored partially_verified evidence. The fallback must judge each
	// under its own product, not the current message's.
	parentID := uuid.MustParse(testWorkID)
	optedInProduct := uuid.New()
	strictProduct := uuid.New()
	taskA := uuid.New()
	taskB := uuid.New()
	taskC := uuid.New() // passed in the DB, but no stored evidence

	items := fakeItems{
		parentID: {ID: parentID, Type: models.WorkItemTypeFeature},
		taskA:    {ID: taskA, Type: models.WorkItemTypeSubtask, ParentID: &parentID, ProductID: &optedInProduct},
		taskB:    {ID: taskB, Type: models.WorkItemTypeSubtask, ParentID: &parentID, ProductID: &strictProduct},
		taskC:    {ID: taskC, Type: models.WorkItemTypeSubtask, ParentID: &parentID, ProductID: &optedInProduct},
	}
	store := fakeProducts{
		optedInProduct: {ID: optedInProduct, PartialVerificationOptIn: true},
		strictProduct:  {ID: strictProduct, PartialVerificationOptIn: false},
	}
	partialFor := func(taskID uuid.UUID, sha string) evidence.Verification {
		ev := baseEvidence()
		ev.TaskID = taskID.String()
		ev.HeadSHA = sha
		ev.Checks = append(ev.Checks, evidence.CheckResult{Kind: evidence.CheckKindLint, Status: evidence.CheckSkipped, SkipReason: "no linter"})
		ev.Outcome = evidence.OutcomePartiallyVerified
		if err := ev.Validate(); err != nil {
			t.Fatalf("fixture: %v", err)
		}
		return evidence.Verification{Evidence: ev}
	}
	shaA := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	shaB := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	evStore := fakeEvidence{rows: []evidence.Verification{partialFor(taskA, shaA), partialFor(taskB, shaB)}}

	a := decideUnseenSubtask(context.Background(), items, store, evStore, parentID, taskA.String())
	if a.Action != evidence.ActionHumanReview || a.Label != outcomeLabelPartiallyVerified || a.HeadSHA != shaA {
		t.Errorf("sibling A (opted in) = %+v, want human_review with head %s", a, shaA)
	}
	b := decideUnseenSubtask(context.Background(), items, store, evStore, parentID, taskB.String())
	if b.Action != evidence.ActionBlock || b.HeadSHA != shaB {
		t.Errorf("sibling B (not opted in) = %+v, want block with head %s", b, shaB)
	}
	if !strings.Contains(b.Reason, "has not opted in") {
		t.Errorf("sibling B reason %q should say the product has not opted in", b.Reason)
	}
	c := decideUnseenSubtask(context.Background(), items, store, evStore, parentID, taskC.String())
	if c.Action != evidence.ActionBlock || !strings.Contains(c.Reason, "no stored evidence") {
		t.Errorf("sibling C (no record) = %+v, want block for missing evidence", c)
	}

	// A subtask without its own product inherits the parent's; a store
	// error yields the zero policy, so partial blocks.
	taskD := uuid.New()
	items[taskD] = &models.WorkItem{ID: taskD, Type: models.WorkItemTypeSubtask, ParentID: &parentID}
	items[parentID].ProductID = &optedInProduct
	evStore.rows = append(evStore.rows, partialFor(taskD, shaA))
	if d := decideUnseenSubtask(context.Background(), items, store, evStore, parentID, taskD.String()); d.Action != evidence.ActionHumanReview {
		t.Errorf("sibling D (inherits opted-in parent) = %+v, want human_review", d)
	}
	if d := decideUnseenSubtask(context.Background(), items, fakeProducts{}, evStore, parentID, taskD.String()); d.Action != evidence.ActionBlock {
		t.Errorf("sibling D with failing product store = %+v, want block", d)
	}
	if e := decideUnseenSubtask(context.Background(), items, store, fakeEvidence{err: errors.New("db down")}, parentID, taskA.String()); e.Action != evidence.ActionBlock {
		t.Errorf("evidence store error = %+v, want block", e)
	}

	// The fallback checks the stored record's identity against the subtask
	// it is deciding, including the subtask's repository when the work item
	// row names one: a record for another repository blocks, a record for
	// the same repository (or with no repository) does not.
	repoX, repoY := uuid.New(), uuid.New()
	taskE := uuid.New()
	items[taskE] = &models.WorkItem{ID: taskE, Type: models.WorkItemTypeSubtask, ParentID: &parentID, ProductID: &optedInProduct, RepositoryID: &repoY}
	foreignRepo := partialFor(taskE, shaA)
	repoXStr := repoX.String()
	foreignRepo.Evidence.RepositoryID = &repoXStr
	evStore.rows = append(evStore.rows, foreignRepo)
	if e := decideUnseenSubtask(context.Background(), items, store, evStore, parentID, taskE.String()); e.Action != evidence.ActionBlock || !strings.Contains(e.Reason, "repository_id") || e.HeadSHA != "" {
		t.Errorf("sibling E (stored record for another repository) = %+v, want identity block without SHA", e)
	}
	repoYStr := repoY.String()
	evStore.rows[len(evStore.rows)-1].Evidence.RepositoryID = &repoYStr
	if e := decideUnseenSubtask(context.Background(), items, store, evStore, parentID, taskE.String()); e.Action != evidence.ActionHumanReview || e.HeadSHA != shaA {
		t.Errorf("sibling E (stored record for its own repository) = %+v, want human_review", e)
	}
	evStore.rows[len(evStore.rows)-1].Evidence.RepositoryID = nil
	if e := decideUnseenSubtask(context.Background(), items, store, evStore, parentID, taskE.String()); e.Action != evidence.ActionHumanReview {
		t.Errorf("sibling E (stored record without repository) = %+v, want human_review", e)
	}
	// A stored record filed under this task id but claiming another work
	// item is not this subtask's evidence.
	foreignWork := partialFor(taskA, shaA)
	foreignWork.Evidence.WorkID = otherID
	if e := decideUnseenSubtask(context.Background(), items, store, fakeEvidence{rows: []evidence.Verification{foreignWork}}, parentID, taskA.String()); e.Action != evidence.ActionBlock || !strings.Contains(e.Reason, "work_id") {
		t.Errorf("stored record for another work item = %+v, want identity block", e)
	}
}
