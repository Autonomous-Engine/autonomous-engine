package evidence

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// legacySuccessJSON is what cmd/implementation-agent publishes on
// implementation.complete after the finalization work: the original fields
// plus the additive head_sha / base_sha / branch / verified evidence fields.
const legacySuccessJSON = `{
  "task_id": "7a7a7a7a-2222-4333-8444-555566667777",
  "work_id": "9d1b2f3a-1111-4222-8333-444455556666",
  "files_changed": ["internal/foo/foo.go", "internal/foo/foo_test.go"],
  "test_results": "passed: go test ./...",
  "summary": "Implemented foo.",
  "success": true,
  "head_sha": "2de333cabcdef0123456789abcdef0123456789a",
  "base_sha": "0de2b26812bfc966ac61835e1b6aae2f3282cfcd",
  "branch": "agent/work/task",
  "verified": true
}`

// legacyOldShapeSuccessJSON is the pre-finalization wire shape: no evidence
// fields, and test_results claims execution regardless of what ran.
const legacyOldShapeSuccessJSON = `{
  "task_id": "7a7a7a7a-2222-4333-8444-555566667777",
  "work_id": "9d1b2f3a-1111-4222-8333-444455556666",
  "files_changed": ["internal/foo/foo.go", "internal/foo/foo_test.go"],
  "test_results": "executed via tool loop",
  "summary": "Implemented foo.",
  "success": true
}`

const legacyFailureJSON = `{
  "task_id": "7a7a7a7a-2222-4333-8444-555566667777",
  "work_id": "9d1b2f3a-1111-4222-8333-444455556666",
  "files_changed": null,
  "test_results": "",
  "success": false,
  "reason": "repository environment broken: go.mod missing"
}`

func decodeLegacy(t *testing.T, raw string) LegacyCompleteMessage {
	t.Helper()
	var msg LegacyCompleteMessage
	if err := json.Unmarshal([]byte(raw), &msg); err != nil {
		t.Fatalf("unmarshal legacy JSON: %v", err)
	}
	return msg
}

func TestLegacyCompleteMessage_DecodesProducerFieldNames(t *testing.T) {
	msg := decodeLegacy(t, legacySuccessJSON)
	if msg.TaskID != "7a7a7a7a-2222-4333-8444-555566667777" || msg.WorkID != "9d1b2f3a-1111-4222-8333-444455556666" {
		t.Errorf("ids not decoded: %+v", msg)
	}
	if len(msg.FilesChanged) != 2 || msg.TestResults != "passed: go test ./..." || !msg.Success || msg.Summary != "Implemented foo." {
		t.Errorf("legacy fields not decoded: %+v", msg)
	}
	if msg.HeadSHA != "2de333cabcdef0123456789abcdef0123456789a" || msg.BaseSHA != "0de2b26812bfc966ac61835e1b6aae2f3282cfcd" || msg.Branch != "agent/work/task" || !msg.Verified {
		t.Errorf("evidence fields not decoded: %+v", msg)
	}

	// Round trip must keep the wire names so the type can stand in for the
	// producer's struct in consumers.
	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"task_id"`, `"work_id"`, `"files_changed"`, `"test_results"`, `"summary"`, `"success"`, `"head_sha"`, `"base_sha"`, `"branch"`, `"verified"`} {
		if !strings.Contains(string(data), key) {
			t.Errorf("legacy JSON missing %s: %s", key, data)
		}
	}

	// The old shape still decodes; the additive fields are simply zero.
	old := decodeLegacy(t, legacyOldShapeSuccessJSON)
	if old.HeadSHA != "" || old.BaseSHA != "" || old.Branch != "" || old.Verified {
		t.Errorf("old shape must leave evidence fields zero: %+v", old)
	}
}

func TestFromLegacyCompleteMessage_PassedWithHeadSHAIsPartiallyVerified(t *testing.T) {
	msg := decodeLegacy(t, legacySuccessJSON)
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)

	ev := FromLegacyCompleteMessage(msg, now)
	if err := ev.Validate(); err != nil {
		t.Fatalf("adapted evidence must validate: %v", err)
	}
	if ev.SchemaVersion != SchemaVersion || ev.WorkID != msg.WorkID || ev.TaskID != msg.TaskID {
		t.Errorf("identity not carried: %+v", ev)
	}
	if ev.HeadSHA != msg.HeadSHA || ev.BaseSHA != msg.BaseSHA || ev.Branch != msg.Branch {
		t.Errorf("git identity not copied through: head=%q base=%q branch=%q", ev.HeadSHA, ev.BaseSHA, ev.Branch)
	}
	if ev.RepositoryID != nil {
		t.Errorf("repository_id is not on the legacy payload; got %v", *ev.RepositoryID)
	}
	if ev.Outcome != OutcomePartiallyVerified {
		t.Errorf("Outcome = %q, want partially_verified for success + passed + head_sha (legacy provenance cap, D1.4)", ev.Outcome)
	}
	if len(ev.Checks) != 2 {
		t.Fatalf("Checks = %+v, want the parsed test check plus the provenance check", ev.Checks)
	}
	c := ev.Checks[0]
	if c.Kind != CheckKindTest || c.Status != CheckPassed || c.Command != "go test ./..." || c.ExitCode != nil || c.SkipReason != "" {
		t.Errorf("parsed check = %+v", c)
	}
	assertProvenanceCheck(t, ev.Checks[1])
	for _, optIn := range []bool{false, true} {
		if d := ReleaseDecision(ev, ReleasePolicy{PartialVerificationOptIn: optIn}); d.Action == ActionRelease {
			t.Errorf("optIn=%v: legacy evidence must never release; got %+v", optIn, d)
		}
	}
	if len(ev.ChangedFiles) != 2 || ev.ChangedFiles[0] != "internal/foo/foo.go" {
		t.Errorf("ChangedFiles = %v", ev.ChangedFiles)
	}
	if !ev.StartedAt.Equal(now) || !ev.FinishedAt.Equal(now) {
		t.Errorf("timestamps = %v/%v, want receivedAt", ev.StartedAt, ev.FinishedAt)
	}
	if ev.ArtifactRefs == nil || len(ev.ArtifactRefs) != 0 {
		t.Errorf("ArtifactRefs = %#v, want empty non-nil", ev.ArtifactRefs)
	}
	if ev.Reason != "" {
		t.Errorf("Reason = %q, want empty for a message without reason", ev.Reason)
	}
}

func TestFromLegacyCompleteMessage_TestResultsForms(t *testing.T) {
	base := decodeLegacy(t, legacySuccessJSON)
	cases := []struct {
		name        string
		testResults string
		success     bool
		headSHA     string
		wantStatus  CheckStatus
		wantCommand string
		wantReason  string
		wantOutcome Outcome
		wantChecks  int
	}{
		// wantChecks counts the parsed test check, the unconditional
		// provenance check (kind other, not_run), and the failed kind=other
		// check on success=false runs.
		{
			name: "passed with head_sha is capped at partially_verified", testResults: "passed: npm test", success: true, headSHA: base.HeadSHA,
			wantStatus: CheckPassed, wantCommand: "npm test", wantOutcome: OutcomePartiallyVerified, wantChecks: 2,
		},
		{
			name: "passed without head_sha is downgraded", testResults: "passed: npm test", success: true, headSHA: "",
			wantStatus: CheckNotRun, wantCommand: "npm test", wantReason: "no head_sha", wantOutcome: OutcomeUnverified, wantChecks: 2,
		},
		{
			name: "failed", testResults: "failed: go test ./...", success: false, headSHA: base.HeadSHA,
			wantStatus: CheckFailed, wantCommand: "go test ./...", wantOutcome: OutcomeFailed, wantChecks: 3,
		},
		{
			name: "skipped", testResults: "skipped: no test runner detected", success: true, headSHA: base.HeadSHA,
			wantStatus: CheckSkipped, wantCommand: legacyCommand, wantReason: "no test runner detected", wantOutcome: OutcomeUnverified, wantChecks: 2,
		},
		{
			name: "skipped on a failed run", testResults: "skipped: verification unstable", success: false, headSHA: base.HeadSHA,
			wantStatus: CheckSkipped, wantCommand: legacyCommand, wantReason: "verification unstable", wantOutcome: OutcomeFailed, wantChecks: 3,
		},
		{
			name: "unrecognised text keeps the old behaviour", testResults: "executed via tool loop", success: true, headSHA: base.HeadSHA,
			wantStatus: CheckNotRun, wantCommand: legacyCommand, wantReason: "executed via tool loop", wantOutcome: OutcomeUnverified, wantChecks: 2,
		},
		{
			name: "unknown prefix is not parsed", testResults: "errored: boom", success: true, headSHA: base.HeadSHA,
			wantStatus: CheckNotRun, wantCommand: legacyCommand, wantReason: "errored: boom", wantOutcome: OutcomeUnverified, wantChecks: 2,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msg := base
			msg.TestResults = tc.testResults
			msg.Success = tc.success
			msg.HeadSHA = tc.headSHA
			ev := FromLegacyCompleteMessage(msg, time.Now())
			if err := ev.Validate(); err != nil {
				t.Fatalf("adapted evidence must validate: %v", err)
			}
			if ev.Outcome != tc.wantOutcome {
				t.Errorf("Outcome = %q, want %q", ev.Outcome, tc.wantOutcome)
			}
			if len(ev.Checks) != tc.wantChecks {
				t.Fatalf("Checks = %+v, want %d", ev.Checks, tc.wantChecks)
			}
			c := ev.Checks[0]
			if c.Kind != CheckKindTest || c.Status != tc.wantStatus || c.Command != tc.wantCommand {
				t.Errorf("test check = %+v, want status %q command %q", c, tc.wantStatus, tc.wantCommand)
			}
			if !strings.Contains(c.SkipReason, tc.wantReason) {
				t.Errorf("SkipReason = %q, want it to contain %q", c.SkipReason, tc.wantReason)
			}
			assertProvenanceCheck(t, ev.Checks[1])
			if !tc.success {
				last := ev.Checks[len(ev.Checks)-1]
				if last.Kind != CheckKindOther || last.Status != CheckFailed {
					t.Errorf("failed run must carry a failed kind=other check; last = %+v", last)
				}
			}
		})
	}
}

// The pre-finalization shape (no head_sha / base_sha / branch / verified,
// test_results = "executed via tool loop") must still adapt, and must never
// come out as verified or passed.
func TestFromLegacyCompleteMessage_OldShapeIsUnverified(t *testing.T) {
	msg := decodeLegacy(t, legacyOldShapeSuccessJSON)
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)

	ev := FromLegacyCompleteMessage(msg, now)
	if err := ev.Validate(); err != nil {
		t.Fatalf("adapted evidence must validate: %v", err)
	}
	if ev.Outcome != OutcomeUnverified {
		t.Errorf("Outcome = %q, want unverified (old-shape success proves nothing)", ev.Outcome)
	}
	if ev.BaseSHA != "" || ev.HeadSHA != "" || ev.Branch != "" {
		t.Errorf("old shape has no git identity; got base=%q head=%q branch=%q", ev.BaseSHA, ev.HeadSHA, ev.Branch)
	}
	if len(ev.Checks) != 2 {
		t.Fatalf("Checks = %+v, want the synthetic test check plus the provenance check", ev.Checks)
	}
	c := ev.Checks[0]
	if c.Status != CheckNotRun || c.Kind != CheckKindTest || c.ExitCode != nil {
		t.Errorf("legacy test_results rendered as %+v, want not_run", c)
	}
	assertProvenanceCheck(t, ev.Checks[1])
	if !strings.Contains(c.SkipReason, "executed via tool loop") {
		t.Errorf("legacy test_results text must be preserved in skip_reason: %q", c.SkipReason)
	}
	data, _ := json.Marshal(ev)
	if strings.Contains(string(data), `"status":"passed"`) || strings.Contains(string(data), `"outcome":"verified"`) {
		t.Errorf("old-shape success rendered as verified/passed: %s", data)
	}
}

func TestFromLegacyCompleteMessage_FailureIsFailedAndCarriesReason(t *testing.T) {
	msg := decodeLegacy(t, legacyFailureJSON)
	ev := FromLegacyCompleteMessage(msg, time.Now())
	if err := ev.Validate(); err != nil {
		t.Fatalf("adapted evidence must validate: %v", err)
	}
	if ev.Outcome != OutcomeFailed {
		t.Errorf("Outcome = %q, want failed", ev.Outcome)
	}
	if len(ev.Checks) != 2 || ev.Checks[1].Status != CheckFailed || ev.Checks[1].Kind != CheckKindOther {
		t.Errorf("Checks = %+v, want the provenance check followed by one failed check of kind other", ev.Checks)
	}
	assertProvenanceCheck(t, ev.Checks[0])
	if ev.Reason != "repository environment broken: go.mod missing" {
		t.Errorf("Reason = %q, want the legacy reason", ev.Reason)
	}
	if ev.ChangedFiles == nil || len(ev.ChangedFiles) != 0 {
		t.Errorf("ChangedFiles = %#v, want empty non-nil for null files_changed", ev.ChangedFiles)
	}
	data, _ := json.Marshal(ev)
	if !strings.Contains(string(data), `"reason":"repository environment broken: go.mod missing"`) {
		t.Errorf("reason missing from JSON: %s", data)
	}
}

func TestFromLegacyCompleteMessage_DoesNotAliasInputSlice(t *testing.T) {
	msg := LegacyCompleteMessage{WorkID: "w", TaskID: "t", Success: true, FilesChanged: []string{"a.go"}}
	ev := FromLegacyCompleteMessage(msg, time.Now())
	msg.FilesChanged[0] = "mutated.go"
	if ev.ChangedFiles[0] != "a.go" {
		t.Errorf("ChangedFiles aliases caller slice: %v", ev.ChangedFiles)
	}
}

// assertProvenanceCheck checks the unconditional D1.4 cap the adapter adds:
// a not_run check of kind other whose skip_reason explains that the legacy
// payload cannot prove where changed_files came from.
func assertProvenanceCheck(t *testing.T, c CheckResult) {
	t.Helper()
	if c.Kind != CheckKindOther || c.Status != CheckNotRun || c.Command != legacyCommand || c.SkipReason != legacyProvenanceReason || c.ExitCode != nil {
		t.Errorf("provenance check = %+v, want kind other / not_run / command %q / skip_reason %q", c, legacyCommand, legacyProvenanceReason)
	}
}

// TestFromLegacyCompleteMessage_NeverVerifiedNeverReleases sweeps every
// legacy field the adapter distinguishes (test_results form, success,
// head_sha presence, the producer's verified flag, files_changed) and
// asserts that no combination derives verified, that each record carries
// the provenance check, and that ReleaseDecision never releases it under
// either policy (D1.4: verified needs git-derived changed files, which a
// legacy message cannot prove).
func TestFromLegacyCompleteMessage_NeverVerifiedNeverReleases(t *testing.T) {
	testResults := []string{
		"passed: go test ./...", "passed:", "failed: go test ./...", "skipped: no runner",
		"skipped:", "executed via tool loop", "errored: boom", "",
	}
	heads := []string{"", "2de333cabcdef0123456789abcdef0123456789a"}
	files := [][]string{nil, {"internal/foo/foo.go", "internal/foo/foo_test.go"}}
	seenPartial := false
	for _, tr := range testResults {
		for _, success := range []bool{false, true} {
			for _, head := range heads {
				for _, verified := range []bool{false, true} {
					for _, fc := range files {
						msg := LegacyCompleteMessage{
							TaskID: "t", WorkID: "w", FilesChanged: fc, TestResults: tr,
							Success: success, Verified: verified,
							HeadSHA: head, BaseSHA: "0de2b26812bfc966ac61835e1b6aae2f3282cfcd", Branch: "agent/work/t",
						}
						ev := FromLegacyCompleteMessage(msg, time.Now())
						if err := ev.Validate(); err != nil {
							t.Errorf("test_results=%q success=%v head=%q verified=%v: adapted evidence must validate: %v", tr, success, head, verified, err)
							continue
						}
						assertHasProvenanceCheck(t, ev)
						if ev.Outcome == OutcomeVerified {
							t.Errorf("test_results=%q success=%v head=%q verified=%v: outcome verified from a legacy message", tr, success, head, verified)
						}
						if ev.Outcome == OutcomePartiallyVerified {
							seenPartial = true
						}
						for _, optIn := range []bool{false, true} {
							d := ReleaseDecision(ev, ReleasePolicy{PartialVerificationOptIn: optIn})
							if d.Action == ActionRelease {
								t.Errorf("test_results=%q success=%v head=%q verified=%v optIn=%v: legacy evidence released: %+v", tr, success, head, verified, optIn, d)
							}
						}
					}
				}
			}
		}
	}
	if !seenPartial {
		t.Error("sweep never produced partially_verified; the passed+head_sha path should reach it (and no further)")
	}
}

func assertHasProvenanceCheck(t *testing.T, ev *ImplementationEvidence) {
	t.Helper()
	for _, c := range ev.Checks {
		if c.SkipReason == legacyProvenanceReason {
			assertProvenanceCheck(t, c)
			return
		}
	}
	t.Errorf("no provenance check among %+v", ev.Checks)
}
