package evidence

import (
	"encoding/json"
	"strings"
	"testing"
)

// verifiedEvidence returns a valid schema-1 record whose every check passed,
// i.e. the only shape that may pass the automated release gate.
func verifiedEvidence(t *testing.T) *ImplementationEvidence {
	t.Helper()
	ev := sampleEvidence(t)
	ev.Checks = []CheckResult{ev.Checks[0]}
	ev.Outcome = OutcomeVerified
	if err := ev.Validate(); err != nil {
		t.Fatalf("fixture must be valid: %v", err)
	}
	return &ev
}

// partiallyVerifiedEvidence is sampleEvidence: one passed, one skipped.
func partiallyVerifiedEvidence(t *testing.T) *ImplementationEvidence {
	t.Helper()
	ev := sampleEvidence(t)
	if ev.Outcome != OutcomePartiallyVerified {
		t.Fatalf("sampleEvidence outcome = %q, want partially_verified", ev.Outcome)
	}
	return &ev
}

func failedEvidence(t *testing.T) *ImplementationEvidence {
	t.Helper()
	ev := sampleEvidence(t)
	ev.Checks = append(ev.Checks, CheckResult{
		Kind:     CheckKindOther,
		Command:  "git push",
		ExitCode: intPtr(1),
		Status:   CheckFailed,
	})
	ev.Outcome = OutcomeFailed
	ev.Reason = "push rejected: non-fast-forward"
	if err := ev.Validate(); err != nil {
		t.Fatalf("fixture must be valid: %v", err)
	}
	return &ev
}

func unverifiedEvidence(t *testing.T) *ImplementationEvidence {
	t.Helper()
	ev := sampleEvidence(t)
	ev.Checks = []CheckResult{{
		Kind:       CheckKindTest,
		Command:    "npm test",
		Status:     CheckSkipped,
		SkipReason: "no test runner configured",
	}}
	ev.Outcome = OutcomeUnverified
	if err := ev.Validate(); err != nil {
		t.Fatalf("fixture must be valid: %v", err)
	}
	return &ev
}

// withSecurityFlag wraps a fixture so it carries the D3 security flag.
func withSecurityFlag(base func(t *testing.T) *ImplementationEvidence) func(t *testing.T) *ImplementationEvidence {
	return func(t *testing.T) *ImplementationEvidence {
		t.Helper()
		ev := base(t)
		ev.Flags = []string{FlagSecurityReviewRequired}
		if err := ev.Validate(); err != nil {
			t.Fatalf("flagged fixture must be valid: %v", err)
		}
		return ev
	}
}

// TestReleaseDecision_TruthTable pins the D1.2 / D1.3 / D2 gate: only a valid
// verified record releases; partially_verified goes to human review only when
// the product opted in; everything else, including anything Validate rejects,
// blocks.
func TestReleaseDecision_TruthTable(t *testing.T) {
	cases := []struct {
		name       string
		ev         func(t *testing.T) *ImplementationEvidence
		optIn      bool
		wantAction ReleaseAction
		wantReason []string // substrings the reason must contain
	}{
		{
			name:       "verified releases",
			ev:         verifiedEvidence,
			wantAction: ActionRelease,
			wantReason: []string{"verified"},
		},
		{
			name:       "verified releases regardless of opt-in",
			ev:         verifiedEvidence,
			optIn:      true,
			wantAction: ActionRelease,
			wantReason: []string{"verified"},
		},
		{
			name:       "verified with security_review_required goes to human review (D3)",
			ev:         withSecurityFlag(verifiedEvidence),
			wantAction: ActionHumanReview,
			wantReason: []string{"verified", "security review"},
		},
		{
			name:       "verified with security_review_required goes to human review regardless of opt-in",
			ev:         withSecurityFlag(verifiedEvidence),
			optIn:      true,
			wantAction: ActionHumanReview,
			wantReason: []string{"verified", "security review"},
		},
		{
			name:       "partially_verified with security flag and opt-in goes to human review",
			ev:         withSecurityFlag(partiallyVerifiedEvidence),
			optIn:      true,
			wantAction: ActionHumanReview,
			wantReason: []string{"partially_verified", "security review required"},
		},
		{
			name:       "partially_verified with security flag without opt-in blocks",
			ev:         withSecurityFlag(partiallyVerifiedEvidence),
			wantAction: ActionBlock,
			wantReason: []string{"partially_verified", "security review required"},
		},
		{
			name:       "failed with security flag blocks",
			ev:         withSecurityFlag(failedEvidence),
			optIn:      true,
			wantAction: ActionBlock,
			wantReason: []string{"failed"},
		},
		{
			name: "unknown flag blocks (fail closed)",
			ev: func(t *testing.T) *ImplementationEvidence {
				ev := verifiedEvidence(t)
				ev.Flags = []string{"future_flag"}
				return ev
			},
			optIn:      true,
			wantAction: ActionBlock,
			wantReason: []string{"invalid evidence", "unknown flag"},
		},
		{
			name:       "partially_verified with opt-in goes to human review",
			ev:         partiallyVerifiedEvidence,
			optIn:      true,
			wantAction: ActionHumanReview,
			wantReason: []string{"partially_verified", "opted in"},
		},
		{
			name:       "partially_verified without opt-in blocks",
			ev:         partiallyVerifiedEvidence,
			wantAction: ActionBlock,
			wantReason: []string{"partially_verified", "has not opted in"},
		},
		{
			name:       "failed blocks",
			ev:         failedEvidence,
			optIn:      true,
			wantAction: ActionBlock,
			wantReason: []string{"failed", "push rejected"},
		},
		{
			name:       "unverified blocks",
			ev:         unverifiedEvidence,
			optIn:      true,
			wantAction: ActionBlock,
			wantReason: []string{"unverified"},
		},
		{
			name: "unknown outcome string blocks",
			ev: func(t *testing.T) *ImplementationEvidence {
				ev := verifiedEvidence(t)
				ev.Outcome = "pending"
				return ev
			},
			optIn:      true,
			wantAction: ActionBlock,
			wantReason: []string{"pending"},
		},
		{
			name: "unknown schema version blocks",
			ev: func(t *testing.T) *ImplementationEvidence {
				ev := verifiedEvidence(t)
				ev.SchemaVersion = 2
				return ev
			},
			optIn:      true,
			wantAction: ActionBlock,
			wantReason: []string{"unknown evidence schema version 2"},
		},
		{
			name:       "nil evidence blocks",
			ev:         func(*testing.T) *ImplementationEvidence { return nil },
			optIn:      true,
			wantAction: ActionBlock,
			wantReason: []string{"no evidence attached"},
		},
		{
			name: "verified with empty head_sha blocks",
			ev: func(t *testing.T) *ImplementationEvidence {
				ev := verifiedEvidence(t)
				ev.HeadSHA = ""
				return ev
			},
			wantAction: ActionBlock,
			wantReason: []string{"verified", "head_sha"},
		},
		{
			name: "verified claim without the checks to back it blocks",
			ev: func(t *testing.T) *ImplementationEvidence {
				ev := partiallyVerifiedEvidence(t)
				ev.Outcome = OutcomeVerified
				return ev
			},
			wantAction: ActionBlock,
			wantReason: []string{"inconsistent"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := ReleaseDecision(tc.ev(t), ReleasePolicy{PartialVerificationOptIn: tc.optIn})
			if d.Action != tc.wantAction {
				t.Fatalf("action = %q, want %q (reason %q)", d.Action, tc.wantAction, d.Reason)
			}
			if strings.TrimSpace(d.Reason) == "" {
				t.Fatal("reason must be non-empty")
			}
			for _, want := range tc.wantReason {
				if !strings.Contains(d.Reason, want) {
					t.Errorf("reason %q does not mention %q", d.Reason, want)
				}
			}
		})
	}
}

// TestReleaseDecision_OnlyVerifiedReleases sweeps every known outcome and
// several unknown ones under both policies: release must appear exactly for
// the valid verified record and human_review only for opted-in
// partially_verified.
func TestReleaseDecision_OnlyVerifiedReleases(t *testing.T) {
	outcomes := []Outcome{
		OutcomeVerified, OutcomePartiallyVerified, OutcomeFailed, OutcomeUnverified,
		"", "pending", "skipped", "success", "VERIFIED",
	}
	for _, optIn := range []bool{false, true} {
		for _, o := range outcomes {
			// Build the record whose checks best match the claimed outcome so
			// that a legitimate verified record is the only one Validate
			// accepts as verified.
			var ev *ImplementationEvidence
			switch o {
			case OutcomeVerified:
				ev = verifiedEvidence(t)
			case OutcomePartiallyVerified:
				ev = partiallyVerifiedEvidence(t)
			case OutcomeFailed:
				ev = failedEvidence(t)
			case OutcomeUnverified:
				ev = unverifiedEvidence(t)
			default:
				ev = verifiedEvidence(t)
				ev.Outcome = o
			}
			d := ReleaseDecision(ev, ReleasePolicy{PartialVerificationOptIn: optIn})
			if (d.Action == ActionRelease) != (o == OutcomeVerified) {
				t.Errorf("outcome %q optIn=%v: action %q", o, optIn, d.Action)
			}
			if d.Action == ActionHumanReview && !(o == OutcomePartiallyVerified && optIn) {
				t.Errorf("outcome %q optIn=%v: human_review is reserved for opted-in partially_verified", o, optIn)
			}
			if d.Reason == "" {
				t.Errorf("outcome %q optIn=%v: empty reason", o, optIn)
			}
		}
	}
}

func TestReleaseDecisionFromJSON(t *testing.T) {
	t.Run("malformed JSON blocks", func(t *testing.T) {
		d := ReleaseDecisionFromJSON([]byte(`{"schema_version": 1, "outcome": `), ReleasePolicy{PartialVerificationOptIn: true})
		if d.Action != ActionBlock {
			t.Fatalf("action = %q, want block", d.Action)
		}
		if !strings.Contains(d.Reason, "decode evidence") {
			t.Errorf("reason %q should say the payload could not be decoded", d.Reason)
		}
	})

	t.Run("empty payload blocks", func(t *testing.T) {
		d := ReleaseDecisionFromJSON(nil, ReleasePolicy{PartialVerificationOptIn: true})
		if d.Action != ActionBlock || !strings.Contains(d.Reason, "no evidence attached") {
			t.Fatalf("decision = %+v, want block / no evidence attached", d)
		}
	})

	t.Run("JSON null and empty object block as no evidence", func(t *testing.T) {
		for _, raw := range []string{"null", "{}", " {} ", "\n"} {
			d := ReleaseDecisionFromJSON([]byte(raw), ReleasePolicy{PartialVerificationOptIn: true})
			if d.Action != ActionBlock || !strings.Contains(d.Reason, "no evidence attached") {
				t.Errorf("%q: decision = %+v, want block / no evidence attached", raw, d)
			}
		}
	})

	t.Run("JSON array is malformed evidence", func(t *testing.T) {
		d := ReleaseDecisionFromJSON([]byte("[]"), ReleasePolicy{})
		if d.Action != ActionBlock || !strings.Contains(d.Reason, "decode evidence") {
			t.Fatalf("decision = %+v, want block / decode evidence", d)
		}
	})

	t.Run("unknown schema version blocks", func(t *testing.T) {
		ev := verifiedEvidence(t)
		ev.SchemaVersion = 2
		data, err := json.Marshal(ev)
		if err != nil {
			t.Fatal(err)
		}
		d := ReleaseDecisionFromJSON(data, ReleasePolicy{})
		if d.Action != ActionBlock || !strings.Contains(d.Reason, "unknown evidence schema version 2") {
			t.Fatalf("decision = %+v", d)
		}
	})

	t.Run("verified wire record releases", func(t *testing.T) {
		data, err := json.Marshal(verifiedEvidence(t))
		if err != nil {
			t.Fatal(err)
		}
		d := ReleaseDecisionFromJSON(data, ReleasePolicy{})
		if d.Action != ActionRelease {
			t.Fatalf("decision = %+v, want release", d)
		}
	})

	t.Run("matches ReleaseDecision on the same record", func(t *testing.T) {
		ev := partiallyVerifiedEvidence(t)
		data, err := json.Marshal(ev)
		if err != nil {
			t.Fatal(err)
		}
		for _, optIn := range []bool{false, true} {
			pol := ReleasePolicy{PartialVerificationOptIn: optIn}
			if a, b := ReleaseDecision(ev, pol), ReleaseDecisionFromJSON(data, pol); a != b {
				t.Errorf("optIn=%v: ReleaseDecision %+v != FromJSON %+v", optIn, a, b)
			}
		}
	})
}

// TestReleaseDecision_DoesNotMutateInput guards the helper's purity: a
// consumer may pass a shared record and re-use it afterwards.
func TestReleaseDecision_DoesNotMutateInput(t *testing.T) {
	ev := partiallyVerifiedEvidence(t)
	before, _ := json.Marshal(ev)
	_ = ReleaseDecision(ev, ReleasePolicy{PartialVerificationOptIn: true})
	_ = ReleaseDecision(ev, ReleasePolicy{})
	after, _ := json.Marshal(ev)
	if string(before) != string(after) {
		t.Fatalf("ReleaseDecision mutated its input:\n%s\n%s", before, after)
	}
}
