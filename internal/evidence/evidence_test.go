package evidence

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func intPtr(v int) *int { return &v }

func timePtr(t time.Time) *time.Time { return &t }

// sampleEvidence returns a fully-populated, valid v1 record.
func sampleEvidence(t *testing.T) ImplementationEvidence {
	t.Helper()
	start := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	repo := "3f6b3a2e-6f0e-4b1c-9a3f-9a1d6d0c1b2e"
	ev := ImplementationEvidence{
		SchemaVersion: SchemaVersion,
		WorkID:        "9d1b2f3a-1111-4222-8333-444455556666",
		TaskID:        "7a7a7a7a-2222-4333-8444-555566667777",
		RepositoryID:  &repo,
		BaseSHA:       "0de2b26812bfc966ac61835e1b6aae2f3282cfcd",
		HeadSHA:       "2de333cabcdef0123456789abcdef0123456789a",
		Branch:        "agent/work/task",
		ChangedFiles:  []string{"internal/foo/foo.go", "internal/foo/foo_test.go"},
		Checks: []CheckResult{
			{
				Kind:       CheckKindTest,
				Command:    "go test ./...",
				ExitCode:   intPtr(0),
				Status:     CheckPassed,
				StartedAt:  timePtr(start),
				FinishedAt: timePtr(start.Add(30 * time.Second)),
				OutputRef:  "artifact://check-output/abc123",
			},
			{
				Kind:       CheckKindLint,
				Command:    "golangci-lint run",
				Status:     CheckSkipped,
				SkipReason: "linter not installed in repository environment",
			},
		},
		Outcome:      OutcomePartiallyVerified,
		StartedAt:    start,
		FinishedAt:   start.Add(2 * time.Minute),
		ArtifactRefs: []string{"artifact://diff/abc123"},
		Reason:       "lint skipped: linter not installed",
	}
	if err := ev.Validate(); err != nil {
		t.Fatalf("sample evidence must validate: %v", err)
	}
	return ev
}

func TestCheckResult_JSONRoundTrip(t *testing.T) {
	start := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	in := CheckResult{
		Kind:       CheckKindBuild,
		Command:    "go build ./...",
		ExitCode:   intPtr(1),
		Status:     CheckFailed,
		StartedAt:  timePtr(start),
		FinishedAt: timePtr(start.Add(time.Second)),
		OutputRef:  "artifact://check-output/build",
	}
	data, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, key := range []string{`"kind"`, `"command"`, `"exit_code"`, `"status"`, `"started_at"`, `"finished_at"`, `"output_ref"`} {
		if !strings.Contains(string(data), key) {
			t.Errorf("JSON missing snake_case key %s: %s", key, data)
		}
	}
	var out CheckResult
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(in, out) {
		t.Errorf("round trip mismatch:\n in=%+v\nout=%+v", in, out)
	}
}

func TestCheckResult_JSON_NilExitCodeIsExplicitNull(t *testing.T) {
	data, err := json.Marshal(CheckResult{Kind: CheckKindTest, Status: CheckSkipped, SkipReason: "no runner"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"exit_code":null`) {
		t.Errorf("nil exit_code must serialise as explicit null, got %s", data)
	}
	if !strings.Contains(string(data), `"skip_reason":"no runner"`) {
		t.Errorf("skip_reason missing: %s", data)
	}
}

func TestImplementationEvidence_JSONRoundTrip(t *testing.T) {
	in := sampleEvidence(t)
	data, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, key := range []string{
		`"schema_version":1`, `"work_id"`, `"task_id"`, `"repository_id"`, `"base_sha"`, `"head_sha"`,
		`"branch"`, `"changed_files"`, `"checks"`, `"outcome"`, `"started_at"`, `"finished_at"`, `"artifact_refs"`,
		`"reason":"lint skipped: linter not installed"`,
	} {
		if !strings.Contains(string(data), key) {
			t.Errorf("JSON missing key %s: %s", key, data)
		}
	}
	var out ImplementationEvidence
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(in, out) {
		t.Errorf("round trip mismatch:\n in=%+v\nout=%+v", in, out)
	}

	parsed, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !reflect.DeepEqual(in, *parsed) {
		t.Errorf("Parse mismatch:\n in=%+v\nout=%+v", in, *parsed)
	}
}

func TestImplementationEvidence_JSON_ReasonOmittedWhenEmpty(t *testing.T) {
	in := sampleEvidence(t)
	in.Reason = ""
	data, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), `"reason"`) {
		t.Errorf("empty reason must be omitted from JSON, got %s", data)
	}
}

func TestImplementationEvidence_JSON_NullRepositoryID(t *testing.T) {
	in := sampleEvidence(t)
	in.RepositoryID = nil
	data, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"repository_id":null`) {
		t.Errorf("nil repository_id must serialise as explicit null, got %s", data)
	}
	var out ImplementationEvidence
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	if out.RepositoryID != nil {
		t.Errorf("RepositoryID = %v, want nil", *out.RepositoryID)
	}
}

func TestEnumTypes_JSONRoundTrip(t *testing.T) {
	type wrapper struct {
		Kind    CheckKind   `json:"kind"`
		Status  CheckStatus `json:"status"`
		Outcome Outcome     `json:"outcome"`
	}
	in := wrapper{Kind: CheckKindDiagnostic, Status: CheckNotRun, Outcome: OutcomeUnverified}
	data, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"kind":"diagnostic","status":"not_run","outcome":"unverified"}`
	if string(data) != want {
		t.Errorf("enum JSON = %s, want %s", data, want)
	}
	var out wrapper
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	if out != in {
		t.Errorf("round trip = %+v, want %+v", out, in)
	}
}

func TestParse_RejectsUnknownSchemaVersion(t *testing.T) {
	for _, version := range []int{0, 2, 99} {
		ev := sampleEvidence(t)
		ev.SchemaVersion = version
		data, err := json.Marshal(ev)
		if err != nil {
			t.Fatal(err)
		}
		_, err = Parse(data)
		if !errors.Is(err, ErrUnknownSchemaVersion) {
			t.Errorf("schema_version=%d: Parse error = %v, want ErrUnknownSchemaVersion", version, err)
		}
		if err := ev.Validate(); !errors.Is(err, ErrUnknownSchemaVersion) {
			t.Errorf("schema_version=%d: Validate error = %v, want ErrUnknownSchemaVersion", version, err)
		}
	}
}

func TestParse_RejectsMalformedJSON(t *testing.T) {
	if _, err := Parse([]byte(`{"schema_version": 1, "checks": "not-an-array"}`)); err == nil {
		t.Error("Parse accepted malformed checks")
	}
}

func TestDeriveOutcome(t *testing.T) {
	check := func(s CheckStatus) CheckResult {
		c := CheckResult{Kind: CheckKindTest, Command: "x", Status: s}
		if s == CheckSkipped {
			c.SkipReason = "r"
		}
		return c
	}
	cases := []struct {
		name   string
		checks []CheckResult
		want   Outcome
	}{
		{"no checks (nil)", nil, OutcomeUnverified},
		{"no checks (empty)", []CheckResult{}, OutcomeUnverified},
		{"single passed", []CheckResult{check(CheckPassed)}, OutcomeVerified},
		{"all passed", []CheckResult{check(CheckPassed), check(CheckPassed)}, OutcomeVerified},
		{"single failed", []CheckResult{check(CheckFailed)}, OutcomeFailed},
		{"failed among passed", []CheckResult{check(CheckPassed), check(CheckFailed), check(CheckPassed)}, OutcomeFailed},
		{"failed among skipped", []CheckResult{check(CheckSkipped), check(CheckFailed)}, OutcomeFailed},
		{"failed among not_run", []CheckResult{check(CheckNotRun), check(CheckFailed)}, OutcomeFailed},
		{"single skipped", []CheckResult{check(CheckSkipped)}, OutcomeUnverified},
		{"single not_run", []CheckResult{check(CheckNotRun)}, OutcomeUnverified},
		{"all skipped or not_run", []CheckResult{check(CheckSkipped), check(CheckNotRun), check(CheckSkipped)}, OutcomeUnverified},
		{"passed and skipped", []CheckResult{check(CheckPassed), check(CheckSkipped)}, OutcomePartiallyVerified},
		{"passed and not_run", []CheckResult{check(CheckPassed), check(CheckNotRun)}, OutcomePartiallyVerified},
		{"passed, skipped and not_run", []CheckResult{check(CheckSkipped), check(CheckPassed), check(CheckNotRun)}, OutcomePartiallyVerified},
		{"unknown status never counts as passed", []CheckResult{check(CheckStatus("executed"))}, OutcomeUnverified},
		{"unknown status alongside passed", []CheckResult{check(CheckPassed), check(CheckStatus("executed"))}, OutcomePartiallyVerified},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := DeriveOutcome(tc.checks); got != tc.want {
				t.Errorf("DeriveOutcome = %q, want %q", got, tc.want)
			}
		})
	}
}

// A skipped check must be visible as "skipped" with its reason and must never
// be rendered, derived, or validated as passed -- even when a producer attaches
// a zero exit code to it.
func TestSkippedCheckNeverRendersAsPassed(t *testing.T) {
	skipped := CheckResult{
		Kind:       CheckKindTest,
		Command:    "npm test",
		ExitCode:   intPtr(0),
		Status:     CheckSkipped,
		SkipReason: "no test runner detected in repository",
	}

	data, err := json.Marshal(skipped)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"status":"skipped"`) {
		t.Errorf("skipped check JSON must carry status skipped: %s", data)
	}
	if strings.Contains(string(data), `"passed"`) || strings.Contains(string(data), "executed") {
		t.Errorf("skipped check JSON must not mention passed/executed: %s", data)
	}
	if !strings.Contains(string(data), `"skip_reason":"no test runner detected in repository"`) {
		t.Errorf("skip reason missing from JSON: %s", data)
	}

	if got := DeriveOutcome([]CheckResult{skipped}); got != OutcomeUnverified {
		t.Errorf("lone skipped check derived %q, want unverified", got)
	}
	if got := DeriveOutcome([]CheckResult{skipped, {Kind: CheckKindBuild, Status: CheckPassed}}); got != OutcomePartiallyVerified {
		t.Errorf("skipped+passed derived %q, want partially_verified", got)
	}

	// A producer cannot claim "verified" while any check is skipped.
	ev := sampleEvidence(t)
	ev.Checks = []CheckResult{skipped, {Kind: CheckKindBuild, Command: "go build ./...", ExitCode: intPtr(0), Status: CheckPassed}}
	ev.Outcome = OutcomeVerified
	if err := ev.Validate(); err == nil || !strings.Contains(err.Error(), "inconsistent") {
		t.Errorf("Validate accepted verified outcome with a skipped check: %v", err)
	}
	ev.Outcome = OutcomePartiallyVerified
	if err := ev.Validate(); err != nil {
		t.Errorf("Validate rejected consistent partially_verified: %v", err)
	}
}

func TestValidate_SkippedRequiresReason(t *testing.T) {
	ev := sampleEvidence(t)
	ev.Checks = []CheckResult{{Kind: CheckKindTest, Command: "go test ./...", Status: CheckSkipped}}
	ev.Outcome = OutcomeUnverified
	err := ev.Validate()
	if err == nil || !strings.Contains(err.Error(), "skip_reason") {
		t.Errorf("Validate error = %v, want skip_reason violation", err)
	}

	// not_run does not require a reason.
	ev.Checks[0].Status = CheckNotRun
	if err := ev.Validate(); err != nil {
		t.Errorf("not_run without reason should validate: %v", err)
	}
}

func TestValidate_OutcomeMustMatchDerived(t *testing.T) {
	cases := []struct {
		name    string
		checks  []CheckResult
		outcome Outcome
		wantErr bool
	}{
		{"no checks claimed verified", nil, OutcomeVerified, true},
		{"no checks unverified", nil, OutcomeUnverified, false},
		{"failed claimed verified", []CheckResult{{Kind: CheckKindTest, Status: CheckFailed}}, OutcomeVerified, true},
		{"failed claimed failed", []CheckResult{{Kind: CheckKindTest, Status: CheckFailed}}, OutcomeFailed, false},
		{"passed claimed unverified", []CheckResult{{Kind: CheckKindTest, Status: CheckPassed}}, OutcomeUnverified, true},
		{"passed claimed verified", []CheckResult{{Kind: CheckKindTest, Status: CheckPassed}}, OutcomeVerified, false},
		{"unknown outcome string", nil, Outcome("success"), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev := sampleEvidence(t)
			ev.Checks = tc.checks
			ev.Outcome = tc.outcome
			err := ev.Validate()
			if (err != nil) != tc.wantErr {
				t.Errorf("Validate error = %v, wantErr=%v", err, tc.wantErr)
			}
		})
	}
}

// A verified or partially_verified outcome is only meaningful against a
// specific commit; failed and unverified records (including everything the
// legacy adapter produces for old-shape messages) need no head_sha.
func TestValidate_VerifiedOutcomesRequireHeadSHA(t *testing.T) {
	passed := CheckResult{Kind: CheckKindTest, Command: "go test ./...", Status: CheckPassed}
	skipped := CheckResult{Kind: CheckKindLint, Command: "lint", Status: CheckSkipped, SkipReason: "no linter"}
	failed := CheckResult{Kind: CheckKindOther, Command: "push", Status: CheckFailed}
	cases := []struct {
		name    string
		checks  []CheckResult
		outcome Outcome
		headSHA string
		wantErr bool
	}{
		{"verified with head_sha", []CheckResult{passed}, OutcomeVerified, "abc123", false},
		{"verified without head_sha", []CheckResult{passed}, OutcomeVerified, "", true},
		{"partially_verified with head_sha", []CheckResult{passed, skipped}, OutcomePartiallyVerified, "abc123", false},
		{"partially_verified without head_sha", []CheckResult{passed, skipped}, OutcomePartiallyVerified, "", true},
		{"failed without head_sha", []CheckResult{failed}, OutcomeFailed, "", false},
		{"unverified without head_sha", []CheckResult{skipped}, OutcomeUnverified, "", false},
		{"unverified with no checks and no head_sha", nil, OutcomeUnverified, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev := sampleEvidence(t)
			ev.Checks = tc.checks
			ev.Outcome = tc.outcome
			ev.HeadSHA = tc.headSHA
			err := ev.Validate()
			if (err != nil) != tc.wantErr {
				t.Errorf("Validate error = %v, wantErr=%v", err, tc.wantErr)
			}
			if tc.wantErr && err != nil && !strings.Contains(err.Error(), "head_sha") {
				t.Errorf("error %q should name head_sha", err)
			}
		})
	}
}

func TestValidate_RejectsUnknownEnums(t *testing.T) {
	ev := sampleEvidence(t)
	ev.Checks = []CheckResult{{Kind: CheckKind("unit"), Status: CheckPassed}}
	ev.Outcome = OutcomeVerified
	if err := ev.Validate(); err == nil || !strings.Contains(err.Error(), "kind") {
		t.Errorf("unknown kind: Validate error = %v", err)
	}

	ev.Checks = []CheckResult{{Kind: CheckKindTest, Status: CheckStatus("executed")}}
	ev.Outcome = OutcomeUnverified
	if err := ev.Validate(); err == nil || !strings.Contains(err.Error(), "status") {
		t.Errorf("unknown status: Validate error = %v", err)
	}
}
