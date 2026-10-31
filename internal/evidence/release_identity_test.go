package evidence

import (
	"encoding/json"
	"strings"
	"testing"
)

const (
	identWorkID = "0f1e2d3c-0000-4000-8000-0000000000a1"
	identTaskID = "0f1e2d3c-0000-4000-8000-0000000000a2"
	identRepoID = "0f1e2d3c-0000-4000-8000-0000000000a3"
)

func strPtr(s string) *string { return &s }

// identityEvidence returns a valid record of the given outcome whose ids are
// the identity constants, so a test can construct the matching or
// mismatching expectation explicitly.
func identityEvidence(t *testing.T, outcome Outcome) *ImplementationEvidence {
	t.Helper()
	var ev *ImplementationEvidence
	switch outcome {
	case OutcomeVerified:
		ev = verifiedEvidence(t)
	case OutcomePartiallyVerified:
		ev = partiallyVerifiedEvidence(t)
	case OutcomeFailed:
		ev = failedEvidence(t)
	case OutcomeUnverified:
		ev = unverifiedEvidence(t)
	default:
		t.Fatalf("no fixture for outcome %q", outcome)
	}
	ev.WorkID = identWorkID
	ev.TaskID = identTaskID
	ev.RepositoryID = strPtr(identRepoID)
	if err := ev.Validate(); err != nil {
		t.Fatalf("fixture must be valid: %v", err)
	}
	return ev
}

func identityJSON(t *testing.T, ev *ImplementationEvidence) []byte {
	t.Helper()
	b, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func matching() MessageIdentity {
	return MessageIdentity{WorkID: identWorkID, TaskID: identTaskID, RepositoryID: strPtr(identRepoID)}
}

// TestReleaseDecisionForMessage_Identity is the truth table for the identity
// check: a matching identity gives exactly ReleaseDecisionFromJSON's answer;
// any mismatch on work_id or task_id blocks; a repository mismatch blocks
// only when both sides know the repository; whitespace never matters.
func TestReleaseDecisionForMessage_Identity(t *testing.T) {
	other := "0f1e2d3c-0000-4000-8000-0000000000ff"
	cases := []struct {
		name       string
		mutate     func(ev *ImplementationEvidence) // record-side change
		expect     MessageIdentity
		wantMatch  bool     // true: same result as ReleaseDecisionFromJSON
		wantReason []string // substrings a mismatch reason must contain
	}{
		{
			name:      "matching identity",
			expect:    matching(),
			wantMatch: true,
		},
		{
			name:       "work_id mismatch",
			expect:     MessageIdentity{WorkID: other, TaskID: identTaskID, RepositoryID: strPtr(identRepoID)},
			wantReason: []string{"does not belong", "work_id", identWorkID, other},
		},
		{
			name:       "task_id mismatch",
			expect:     MessageIdentity{WorkID: identWorkID, TaskID: other, RepositoryID: strPtr(identRepoID)},
			wantReason: []string{"does not belong", "task_id", identTaskID, other},
		},
		{
			name:       "repository mismatch when both known",
			expect:     MessageIdentity{WorkID: identWorkID, TaskID: identTaskID, RepositoryID: strPtr(other)},
			wantReason: []string{"does not belong", "repository_id", identRepoID, other},
		},
		{
			name:      "repository unknown on the message side is not checked",
			expect:    MessageIdentity{WorkID: identWorkID, TaskID: identTaskID, RepositoryID: nil},
			wantMatch: true,
		},
		{
			name:      "repository unknown on the record side is not checked",
			mutate:    func(ev *ImplementationEvidence) { ev.RepositoryID = nil },
			expect:    MessageIdentity{WorkID: identWorkID, TaskID: identTaskID, RepositoryID: strPtr(other)},
			wantMatch: true,
		},
		{
			name:      "repository unknown on both sides is not checked",
			mutate:    func(ev *ImplementationEvidence) { ev.RepositoryID = nil },
			expect:    MessageIdentity{WorkID: identWorkID, TaskID: identTaskID, RepositoryID: nil},
			wantMatch: true,
		},
		{
			name: "whitespace-only differences match",
			mutate: func(ev *ImplementationEvidence) {
				ev.WorkID = " " + identWorkID + "\n"
				ev.RepositoryID = strPtr("\t" + identRepoID)
			},
			expect:    MessageIdentity{WorkID: identWorkID + "  ", TaskID: "\t" + identTaskID, RepositoryID: strPtr(identRepoID + " ")},
			wantMatch: true,
		},
		{
			name:       "empty expected work_id does not match a populated record",
			expect:     MessageIdentity{WorkID: "", TaskID: identTaskID},
			wantReason: []string{"work_id"},
		},
		{
			name:       "empty expected task_id does not match a populated record",
			expect:     MessageIdentity{WorkID: identWorkID, TaskID: ""},
			wantReason: []string{"task_id"},
		},
		{
			name:       "case differences are a mismatch (exact equality)",
			expect:     MessageIdentity{WorkID: strings.ToUpper(identWorkID), TaskID: identTaskID},
			wantReason: []string{"work_id"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, outcome := range []Outcome{OutcomeVerified, OutcomePartiallyVerified, OutcomeFailed, OutcomeUnverified} {
				for _, optIn := range []bool{false, true} {
					ev := identityEvidence(t, outcome)
					if tc.mutate != nil {
						tc.mutate(ev)
					}
					raw := identityJSON(t, ev)
					policy := ReleasePolicy{PartialVerificationOptIn: optIn}
					got := ReleaseDecisionForMessage(raw, policy, tc.expect)
					if strings.TrimSpace(got.Reason) == "" {
						t.Fatalf("%s optIn=%v: reason must be non-empty", outcome, optIn)
					}
					if tc.wantMatch {
						want := ReleaseDecisionFromJSON(raw, policy)
						if got != want {
							t.Fatalf("%s optIn=%v: got %+v, want ReleaseDecisionFromJSON's %+v", outcome, optIn, got, want)
						}
						continue
					}
					if got.Action != ActionBlock {
						t.Fatalf("%s optIn=%v: action = %q, want block (reason %q)", outcome, optIn, got.Action, got.Reason)
					}
					for _, want := range tc.wantReason {
						if !strings.Contains(got.Reason, want) {
							t.Errorf("%s optIn=%v: reason %q does not contain %q", outcome, optIn, got.Reason, want)
						}
					}
				}
			}
		})
	}
}

// TestReleaseDecisionForMessage_UndecodableMatchesFromJSON: the decode
// probing (empty, null, {}, malformed) gives the same block and reason as
// ReleaseDecisionFromJSON, whatever the expectation, because there is no
// record to compare.
func TestReleaseDecisionForMessage_UndecodableMatchesFromJSON(t *testing.T) {
	payloads := map[string][]byte{
		"nil":           nil,
		"empty":         []byte(""),
		"whitespace":    []byte("  \n"),
		"null":          []byte("null"),
		"empty object":  []byte("{}"),
		"malformed":     []byte(`{"schema_version":`),
		"not an object": []byte(`"x"`),
	}
	for name, raw := range payloads {
		for _, expect := range []MessageIdentity{matching(), {WorkID: "other", TaskID: "other"}} {
			got := ReleaseDecisionForMessage(raw, ReleasePolicy{}, expect)
			want := ReleaseDecisionFromJSON(raw, ReleasePolicy{})
			if got != want {
				t.Errorf("%s: got %+v, want %+v", name, got, want)
			}
			if got.Action != ActionBlock {
				t.Errorf("%s: action = %q, want block", name, got.Action)
			}
		}
	}
}

// TestReleaseDecisionForMessage_UnknownSchema: an unknown schema version
// blocks either way; with a matching identity the reason is
// ReleaseDecisionFromJSON's, with a foreign identity the reason names the
// identity (the record is rejected before its content is judged).
func TestReleaseDecisionForMessage_UnknownSchema(t *testing.T) {
	unknown := identityEvidence(t, OutcomeVerified)
	unknown.SchemaVersion = SchemaVersion + 1
	raw := identityJSON(t, unknown)

	got := ReleaseDecisionForMessage(raw, ReleasePolicy{}, matching())
	if want := ReleaseDecisionFromJSON(raw, ReleasePolicy{}); got != want {
		t.Errorf("matching: got %+v, want %+v", got, want)
	}
	if got.Action != ActionBlock || !strings.Contains(got.Reason, "unknown evidence schema version") {
		t.Errorf("matching: got %+v, want block for unknown schema", got)
	}

	got = ReleaseDecisionForMessage(raw, ReleasePolicy{}, MessageIdentity{WorkID: "other", TaskID: "other"})
	if got.Action != ActionBlock || !strings.Contains(got.Reason, "does not belong") {
		t.Errorf("foreign: got %+v, want identity block", got)
	}
}

// TestReleaseDecisionForMessage_MismatchNeverReleases sweeps every outcome,
// both policies, and every single-field mismatch: no mismatch ever yields
// release or human_review.
func TestReleaseDecisionForMessage_MismatchNeverReleases(t *testing.T) {
	other := "0f1e2d3c-0000-4000-8000-0000000000ff"
	mismatches := map[string]MessageIdentity{
		"work_id":       {WorkID: other, TaskID: identTaskID, RepositoryID: strPtr(identRepoID)},
		"task_id":       {WorkID: identWorkID, TaskID: other, RepositoryID: strPtr(identRepoID)},
		"repository_id": {WorkID: identWorkID, TaskID: identTaskID, RepositoryID: strPtr(other)},
		"all":           {WorkID: other, TaskID: other, RepositoryID: strPtr(other)},
		"empty":         {},
	}
	outcomes := []Outcome{OutcomeVerified, OutcomePartiallyVerified, OutcomeFailed, OutcomeUnverified}
	for name, expect := range mismatches {
		for _, outcome := range outcomes {
			for _, optIn := range []bool{false, true} {
				raw := identityJSON(t, identityEvidence(t, outcome))
				got := ReleaseDecisionForMessage(raw, ReleasePolicy{PartialVerificationOptIn: optIn}, expect)
				if got.Action == ActionRelease || got.Action == ActionHumanReview {
					t.Errorf("%s mismatch, outcome=%s optIn=%v: action = %q, want block", name, outcome, optIn, got.Action)
				}
			}
		}
	}
}

// TestCheckIdentity pins the helper the consumers use to decide whether a
// decoded record's git facts may be carried at all.
func TestCheckIdentity(t *testing.T) {
	ev := identityEvidence(t, OutcomeVerified)
	if err := CheckIdentity(ev, matching()); err != nil {
		t.Fatalf("matching: %v", err)
	}
	if err := CheckIdentity(ev, MessageIdentity{WorkID: identWorkID, TaskID: "x"}); err == nil {
		t.Fatalf("task mismatch: want error")
	}
	if err := CheckIdentity(nil, matching()); err == nil {
		t.Fatalf("nil record: want error")
	}
}
