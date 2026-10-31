package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/evidence"
	"github.com/autonomousengine/autonomous-engine/internal/models"
	"github.com/autonomousengine/autonomous-engine/internal/verifypolicy"
	"github.com/google/uuid"
)

// staticPolicySource is a verificationPolicySource returning a fixed answer
// and recording the repository ids it was asked about.
type staticPolicySource struct {
	policy verifypolicy.Policy
	found  bool
	err    error
	asked  []uuid.UUID
}

func (s *staticPolicySource) LoadVerificationPolicy(_ context.Context, repoID uuid.UUID) (verifypolicy.Policy, bool, error) {
	s.asked = append(s.asked, repoID)
	if s.err != nil {
		return verifypolicy.Default(repoID), false, s.err
	}
	p := s.policy
	p.RepositoryID = repoID
	return p, s.found, nil
}

// policySource gives in a repository id (when it has none) and returns a
// source that answers with p as a stored policy.
func policySource(t *testing.T, in *finalizeInputs, p verifypolicy.Policy) *staticPolicySource {
	t.Helper()
	if in.task.RepositoryID == "" {
		in.task.RepositoryID = uuid.NewString()
	}
	if p.Source == "" {
		p.Source = verifypolicy.SourceAdmin
	}
	return &staticPolicySource{policy: p.Normalize(), found: true}
}

// partialPolicySource is the stored policy that explicitly accepts a
// repository with no executable check (D2 opt-in at the repository level).
func partialPolicySource(t *testing.T, in *finalizeInputs) *staticPolicySource {
	t.Helper()
	p := verifypolicy.Default(uuid.Nil)
	p.NoChecksPolicy = verifypolicy.NoChecksPartialWithHumanReview
	return policySource(t, in, p)
}

// assertPolicyBlocked checks the block path: failed outcome, test check
// skipped with the policy reason, the run-level policy check failed, no
// push, the item blocked and the assignment failed.
func assertPolicyBlocked(t *testing.T, f *finalizeFixture, sink *recordingSink) {
	t.Helper()
	ev := publishedEvidence(t, f, sink)
	if ev.Outcome != evidence.OutcomeFailed {
		t.Fatalf("outcome = %q, want failed (no executable checks, policy block): %+v", ev.Outcome, ev.Checks)
	}
	test := checkOfKind(t, ev, evidence.CheckKindTest)
	if test.Status != evidence.CheckSkipped || test.SkipReason != policyBlockedSkipReason {
		t.Fatalf("test check = %+v, want skipped with %q", test, policyBlockedSkipReason)
	}
	other := checkOfKind(t, ev, evidence.CheckKindOther)
	if other.Status != evidence.CheckFailed || other.Command != policyCheckCommand {
		t.Fatalf("run-level check = %+v, want failed %q", other, policyCheckCommand)
	}
	msg := sink.published[0]
	if msg.Success || msg.Verified {
		t.Fatalf("success=%v verified=%v, want a failure (never ambiguous success)", msg.Success, msg.Verified)
	}
	if !strings.Contains(msg.Reason, "no executable checks and repository policy blocks") {
		t.Fatalf("reason %q must say the repository policy blocked a run with no executable checks", msg.Reason)
	}
	if pushed := f.remoteSHA(t); pushed != "" {
		t.Fatalf("pushed %s although the gate failed", pushed)
	}
	if len(sink.blocked) != 1 || !strings.Contains(sink.blocked[0], "no executable checks and repository policy blocks") {
		t.Fatalf("blocked = %v, want one block naming the policy", sink.blocked)
	}
	if len(sink.statuses) != 1 || sink.statuses[0] != models.AssignmentStatusFailed {
		t.Fatalf("assignment statuses = %v, want [failed]", sink.statuses)
	}
	for _, opt := range []bool{false, true} {
		if d := evidence.ReleaseDecision(ev, evidence.ReleasePolicy{PartialVerificationOptIn: opt}); d.Action != evidence.ActionBlock {
			t.Fatalf("release decision (opt-in %v) = %+v, want block", opt, d)
		}
	}
}

// TestFinalizePolicyBlocksWhenNoChecks is the roadmap proof for M3-03: with
// no test runner the default (fail-closed) policy blocks instead of
// reporting ambiguous success — whether the repository has no stored policy,
// the store is missing, the lookup fails, the task names no repository, or
// the stored policy says block. Import diagnostics passing does not change
// it.
func TestFinalizePolicyBlocksWhenNoChecks(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, in *finalizeInputs) *staticPolicySource
	}{
		{"no stored policy", func(t *testing.T, in *finalizeInputs) *staticPolicySource {
			in.task.RepositoryID = uuid.NewString()
			return &staticPolicySource{policy: verifypolicy.Default(uuid.Nil), found: false}
		}},
		{"no policy store", func(t *testing.T, in *finalizeInputs) *staticPolicySource {
			in.task.RepositoryID = uuid.NewString()
			return nil
		}},
		{"lookup error", func(t *testing.T, in *finalizeInputs) *staticPolicySource {
			in.task.RepositoryID = uuid.NewString()
			return &staticPolicySource{err: errors.New("connection refused")}
		}},
		{"task without repository", func(t *testing.T, in *finalizeInputs) *staticPolicySource {
			in.task.RepositoryID = ""
			return &staticPolicySource{policy: verifypolicy.Policy{NoChecksPolicy: verifypolicy.NoChecksPartialWithHumanReview}, found: true}
		}},
		{"stored block policy", func(t *testing.T, in *finalizeInputs) *staticPolicySource {
			return policySource(t, in, verifypolicy.Default(uuid.Nil))
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFinalizeFixture(t)
			f.commit(t, "feature.txt", "value=3\n", "feat: no runner")
			f.commitJS(t, jsClean)

			in := f.inputs()
			in.testCommand = ""
			src := tc.setup(t, &in)
			if src != nil {
				in.policies = src
			}
			sink := &recordingSink{}
			if finalizeImplementation(context.Background(), in, sink) {
				t.Fatalf("finalize succeeded with no executable checks under a blocking policy: %+v", sink.published)
			}
			assertPolicyBlocked(t, f, sink)
			if tc.name == "task without repository" && len(src.asked) != 0 {
				t.Fatalf("policy store asked about %v for a task without a repository", src.asked)
			}
		})
	}
}

// TestFinalizePolicyPartialWithHumanReview: an explicit
// partial_with_human_review policy keeps today's D2 path —
// partially_verified, pushed, success — and the release gate then sends it
// to human review only when the product has opted in.
func TestFinalizePolicyPartialWithHumanReview(t *testing.T) {
	f := newFinalizeFixture(t)
	f.commit(t, "feature.txt", "value=3\n", "feat: no runner")
	head := f.commitJS(t, jsClean)

	in := f.inputs()
	in.testCommand = ""
	src := partialPolicySource(t, &in)
	in.policies = src
	sink := &recordingSink{}
	if !finalizeImplementation(context.Background(), in, sink) {
		t.Fatalf("finalize failed under partial_with_human_review: %+v", sink.published)
	}
	ev := publishedEvidence(t, f, sink)
	if ev.Outcome != evidence.OutcomePartiallyVerified || f.remoteSHA(t) != head {
		t.Fatalf("outcome=%q pushed=%s, want partially_verified at %s", ev.Outcome, f.remoteSHA(t), head)
	}
	if !strings.Contains(ev.Reason, "no_checks_policy=partial_with_human_review") {
		t.Fatalf("reason %q must name the repository policy", ev.Reason)
	}
	if len(src.asked) != 1 || src.asked[0].String() != in.task.RepositoryID {
		t.Fatalf("policy looked up for %v, want [%s]", src.asked, in.task.RepositoryID)
	}
	if d := evidence.ReleaseDecision(ev, evidence.ReleasePolicy{PartialVerificationOptIn: true}); d.Action != evidence.ActionHumanReview {
		t.Fatalf("decision with product opt-in = %+v, want human_review", d)
	}
	if d := evidence.ReleaseDecision(ev, evidence.ReleasePolicy{}); d.Action != evidence.ActionBlock {
		t.Fatalf("decision without product opt-in = %+v, want block", d)
	}
}

// TestFinalizePolicyOverridesTestCommand: the policy's test_command
// replaces detection — it supplies a command where none was detected, and
// replaces a detected one.
func TestFinalizePolicyOverridesTestCommand(t *testing.T) {
	requireSh(t)
	for _, detected := range []string{"", "sh -c 'exit 7'"} {
		t.Run("detected="+detected, func(t *testing.T) {
			f := newFinalizeFixture(t)
			head := f.commit(t, "feature.txt", "value=2\n", "feat: passes")

			in := f.inputs()
			in.testCommand = detected
			override := " " + fixtureTestCommand + " "
			p := verifypolicy.Default(uuid.Nil)
			p.TestCommand = &override
			in.policies = policySource(t, &in, p)
			sink := &recordingSink{}
			if !finalizeImplementation(context.Background(), in, sink) {
				t.Fatalf("finalize failed with an overriding test command: %+v", sink.published)
			}
			ev := publishedEvidence(t, f, sink)
			if ev.Outcome != evidence.OutcomeVerified || ev.HeadSHA != head {
				t.Fatalf("outcome=%q head=%s, want verified at %s", ev.Outcome, ev.HeadSHA, head)
			}
			if test := checkOfKind(t, ev, evidence.CheckKindTest); test.Command != fixtureTestCommand || test.Status != evidence.CheckPassed {
				t.Fatalf("test check = %+v, want passed with the policy command %q", test, fixtureTestCommand)
			}
		})
	}
}

// TestFinalizePolicyRequiredLintIsNotRun: a policy requiring lint (which
// the agent cannot run yet) records lint as not_run with
// "not implemented: lint", so a passing test run is capped at
// partially_verified and can never be vacuously verified.
func TestFinalizePolicyRequiredLintIsNotRun(t *testing.T) {
	requireSh(t)
	f := newFinalizeFixture(t)
	f.commit(t, "feature.txt", "value=2\n", "feat: passes")

	in := f.inputs()
	p := verifypolicy.Default(uuid.Nil)
	p.RequiredChecks = []evidence.CheckKind{evidence.CheckKindTest, evidence.CheckKindLint, evidence.CheckKindBuild}
	in.policies = policySource(t, &in, p)
	sink := &recordingSink{}
	if !finalizeImplementation(context.Background(), in, sink) {
		t.Fatalf("finalize failed: %+v", sink.published)
	}
	ev := publishedEvidence(t, f, sink)
	if ev.Outcome != evidence.OutcomePartiallyVerified {
		t.Fatalf("outcome = %q, want partially_verified (required lint/build not run)", ev.Outcome)
	}
	for _, k := range []evidence.CheckKind{evidence.CheckKindLint, evidence.CheckKindBuild} {
		c := checkOfKind(t, ev, k)
		if c.Status != evidence.CheckNotRun || c.SkipReason != "not implemented: "+string(k) || c.ExitCode != nil {
			t.Fatalf("%s check = %+v, want not_run with skip_reason %q", k, c, "not implemented: "+string(k))
		}
	}
	if test := checkOfKind(t, ev, evidence.CheckKindTest); test.Status != evidence.CheckPassed {
		t.Fatalf("test check = %+v, want passed", test)
	}
	msg := sink.published[0]
	if !msg.Success || msg.Verified {
		t.Fatalf("success=%v verified=%v, want success without verified", msg.Success, msg.Verified)
	}
	if !strings.Contains(ev.Reason, "lint") || !strings.Contains(ev.Reason, "partially_verified") {
		t.Fatalf("reason %q must name the unimplemented required checks", ev.Reason)
	}
	if d := evidence.ReleaseDecision(ev, evidence.ReleasePolicy{}); d.Action != evidence.ActionBlock {
		t.Fatalf("decision without opt-in = %+v, want block", d)
	}
}

// TestFinalizePolicySecurityReviewFlag: a changed file matching
// security_review_paths adds the security_review_required flag, and the
// release gate turns otherwise-verified evidence into human review (D3).
// A change touching no such path carries no flag and releases.
func TestFinalizePolicySecurityReviewFlag(t *testing.T) {
	requireSh(t)
	cases := []struct {
		name     string
		file     string
		wantFlag bool
	}{
		{"auth path", "internal/auth/login.go", true},
		{"migration", "db/migrations/052_x.up.sql", true},
		{"ordinary file", "docs/readme.txt", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFinalizeFixture(t)
			f.commit(t, "feature.txt", "value=2\n", "feat: passes")
			if err := os.MkdirAll(filepath.Join(f.worktree, filepath.Dir(tc.file)), 0o755); err != nil {
				t.Fatal(err)
			}
			f.commit(t, tc.file, "package x\n", "feat: touch "+tc.file)

			in := f.inputs()
			in.policies = policySource(t, &in, verifypolicy.Default(uuid.Nil))
			sink := &recordingSink{}
			if !finalizeImplementation(context.Background(), in, sink) {
				t.Fatalf("finalize failed: %+v", sink.published)
			}
			ev := publishedEvidence(t, f, sink)
			if ev.Outcome != evidence.OutcomeVerified {
				t.Fatalf("outcome = %q, want verified (the flag does not change the outcome)", ev.Outcome)
			}
			d := evidence.ReleaseDecision(ev, evidence.ReleasePolicy{PartialVerificationOptIn: true})
			if tc.wantFlag {
				if !reflect.DeepEqual(ev.Flags, []string{evidence.FlagSecurityReviewRequired}) {
					t.Fatalf("flags = %v, want [%s]", ev.Flags, evidence.FlagSecurityReviewRequired)
				}
				if !strings.Contains(ev.Reason, tc.file) {
					t.Fatalf("reason %q must name the security-sensitive file", ev.Reason)
				}
				if d.Action != evidence.ActionHumanReview {
					t.Fatalf("decision = %+v, want human_review for a security-sensitive verified change", d)
				}
				return
			}
			if len(ev.Flags) != 0 || ev.Reason != "" {
				t.Fatalf("flags=%v reason=%q, want none for an ordinary change", ev.Flags, ev.Reason)
			}
			if d.Action != evidence.ActionRelease {
				t.Fatalf("decision = %+v, want release", d)
			}
		})
	}
}
