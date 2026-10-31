package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/llm/adapter"
	"github.com/autonomousengine/autonomous-engine/internal/llm/tools"
	"github.com/autonomousengine/autonomous-engine/internal/verifier"
	"github.com/google/uuid"
)

// scriptedModel answers each completion with the next scripted content and
// records every request it received.
type scriptedModel struct {
	mu      sync.Mutex
	answers []string
	reqs    []adapter.CompletionRequest
}

func (m *scriptedModel) Complete(_ context.Context, req adapter.CompletionRequest) (*adapter.CompletionResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.reqs = append(m.reqs, req)
	if len(m.answers) == 0 {
		return nil, errors.New("script exhausted")
	}
	a := m.answers[0]
	m.answers = m.answers[1:]
	return &adapter.CompletionResponse{Content: a, FinishReason: "stop", InputTokens: 10, OutputTokens: 200}, nil
}
func (m *scriptedModel) Stream(context.Context, adapter.CompletionRequest) (<-chan adapter.StreamChunk, error) {
	return nil, errors.New("no streaming")
}
func (m *scriptedModel) Capabilities() adapter.ModelCapabilities {
	return adapter.ModelCapabilities{SupportsFunctions: true, MaxContextTokens: 100000}
}
func (m *scriptedModel) HealthCheck(context.Context) error { return nil }
func (m *scriptedModel) Name() string                      { return "fake/verifier" }

// fixtureRepo materializes a seeded fixture (a real git repository with a
// base and a head commit).
func fixtureRepo(t *testing.T, name string) (verifier.Fixture, verifier.FixtureRepo) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	fx, err := verifier.LoadFixtures(filepath.Join("..", "..", filepath.FromSlash(verifier.SeededDir)))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range fx {
		if f.Name == name {
			repo, err := f.Materialize(context.Background(), filepath.Join(t.TempDir(), "clone"))
			if err != nil {
				t.Fatal(err)
			}
			return f, repo
		}
	}
	t.Fatalf("no fixture %s", name)
	return verifier.Fixture{}, verifier.FixtureRepo{}
}

const prDescriptionMarker = "PR-DESCRIPTION: the author says this is complete and correct"

// verify runs the stage the handler runs, against the 03-unhandled-nil
// fixture, with a scripted model.
func verify(t *testing.T, answers ...string) (verifier.Verdict, *scriptedModel, verifier.Fixture) {
	t.Helper()
	f, repo := fixtureRepo(t, "03-unhandled-nil")
	m := &scriptedModel{answers: answers}
	reqs := verifier.Requirements{Criteria: f.Criteria, TaskSpecs: []verifier.TaskSpec{{TaskID: "task-1", Spec: f.TaskSpec}}}
	var root string
	v := verifyAt(context.Background(), verifierRun{
		adapter: m,
		info:    verifier.ModelInfo{Provider: "fake", ModelID: "verifier", Source: verifier.SourceSharedWithReviewer, SharedWithAuthor: true},
		newExecutor: func(path string) *tools.Executor {
			root = path
			return tools.NewExecutor(path)
		},
	}, uuid.New(), reqs, verifier.Evidence{}, repo.Path, repo.BaseSHA, repo.BaseSHA, repo.HeadSHA)
	if root == "" {
		t.Fatal("no scratch was created")
	}
	if _, err := os.Stat(filepath.Dir(root)); !os.IsNotExist(err) {
		t.Errorf("scratch %s not discarded: %v", root, err)
	}
	// The model never saw the author's prose: neither the PR description
	// (it rides on pr.ready, which the stage is not given) nor the
	// fixture's author summary.
	for _, r := range m.reqs {
		for _, msg := range r.Messages {
			if strings.Contains(msg.Content, prDescriptionMarker) || strings.Contains(msg.Content, f.AuthorSummary) {
				t.Fatalf("verifier request carried author prose: %q", msg.Content)
			}
		}
	}
	return v, m, f
}

const verdictApprove = `{"schema_version":1,"per_criterion":[
 {"id":"AC-1","status":"met","evidence":["user.go:13"],"note":""},
 {"id":"AC-2","status":"met","evidence":["user.go:14"],"note":""}],"defects":[],"overall":"approve","summary":"ok"}`

const verdictRequestChanges = `{"schema_version":1,"per_criterion":[
 {"id":"AC-1","status":"met","evidence":["user.go:13"],"note":""},
 {"id":"AC-2","status":"not_met","evidence":["user.go:14"],"note":"DisplayName(nil) dereferences u"}],
 "defects":[{"severity":"critical","file":"user.go","line":14,"category":"nil_dereference","description":"u.First on a nil *User panics for signed-out visitors","reproduction":"DisplayName(nil)"}],
 "overall":"request_changes"}`

const verdictEscalate = `{"schema_version":1,"per_criterion":[
 {"id":"AC-1","status":"met","evidence":["user.go:13"],"note":""},
 {"id":"AC-2","status":"not_verifiable","evidence":[],"note":"could not run go test in the scratch tree"}],
 "defects":[],"overall":"escalate"}`

func TestVerifierStageApprove(t *testing.T) {
	v, m, _ := verify(t, verdictApprove)
	d := verifier.Gate(v)
	if d.Action != verifier.ActionApprove || v.Error != "" {
		t.Fatalf("gate = %+v verdict = %+v", d, v)
	}
	if len(m.reqs) != 1 || !strings.Contains(m.reqs[0].Messages[1].Content, "DisplayName returns \\\"anonymous\\\"") && !strings.Contains(m.reqs[0].Messages[1].Content, `DisplayName returns "anonymous"`) {
		t.Fatalf("criteria not in the prompt: %d requests", len(m.reqs))
	}
	if !strings.Contains(m.reqs[0].Messages[1].Content, "+func DisplayName") {
		t.Error("diff (pinned base → head) not in the prompt")
	}
	// Second pass on (default): the LLM review runs; off: approve directly.
	if p := planAfterVerifier(v, d, true, true); !p.secondPass || p.decision != nil {
		t.Errorf("second pass on: %+v", p)
	}
	p := planAfterVerifier(v, d, false, true)
	if p.secondPass || p.decision == nil || !p.decision.approve || !p.decision.humanReviewRequired || p.decision.escalate {
		t.Errorf("second pass off: %+v", p)
	}
}

func TestVerifierStageRequestChanges(t *testing.T) {
	v, _, _ := verify(t, verdictRequestChanges)
	d := verifier.Gate(v)
	if d.Action != verifier.ActionRequestChanges {
		t.Fatalf("gate = %+v", d)
	}
	p := planAfterVerifier(v, d, true, true)
	if p.secondPass || p.decision == nil || p.decision.approve || p.decision.escalate {
		t.Fatalf("plan = %+v", p)
	}
	for _, want := range []string{"AC-2 [not_met]", "[critical] user.go:14", "nil_dereference", "Reproduction: DisplayName(nil)"} {
		if !strings.Contains(p.decision.feedback, want) {
			t.Errorf("feedback lacks %q:\n%s", want, p.decision.feedback)
		}
	}
}

func TestVerifierStageEscalate(t *testing.T) {
	v, _, _ := verify(t, verdictEscalate)
	d := verifier.Gate(v)
	if d.Action != verifier.ActionEscalate || !strings.Contains(d.Reason, "AC-2") {
		t.Fatalf("gate = %+v", d)
	}
	p := planAfterVerifier(v, d, true, false)
	if p.secondPass || p.decision == nil || !p.decision.escalate || !strings.Contains(p.decision.reason, "AC-2") {
		t.Fatalf("plan = %+v", p)
	}
}

func TestVerifierStageParseFailureEscalates(t *testing.T) {
	v, m, _ := verify(t, "DECISION: APPROVE — looks good to me", "LGTM, ship it")
	if len(m.reqs) != 2 {
		t.Fatalf("requests = %d, want 2 (one re-prompt)", len(m.reqs))
	}
	d := verifier.Gate(v)
	if d.Action != verifier.ActionEscalate || v.Error == "" {
		t.Fatalf("gate = %+v verdict = %+v", d, v)
	}
	for _, r := range v.PerCriterion {
		if r.Status != verifier.StatusNotVerifiable {
			t.Errorf("%s = %s, want not_verifiable", r.ID, r.Status)
		}
	}
	if p := planAfterVerifier(v, d, true, true); p.decision == nil || !p.decision.escalate {
		t.Fatalf("plan = %+v", p)
	}
}

// A head the clone cannot produce is not verifiable, never a crash.
func TestVerifierStageUnknownHeadEscalates(t *testing.T) {
	f, repo := fixtureRepo(t, "03-unhandled-nil")
	m := &scriptedModel{answers: []string{verdictApprove}}
	v := verifyAt(context.Background(), verifierRun{adapter: m}, uuid.New(),
		verifier.Requirements{Criteria: f.Criteria}, verifier.Evidence{}, repo.Path, repo.BaseSHA, repo.BaseSHA, strings.Repeat("ab", 20))
	if len(m.reqs) != 0 || verifier.Gate(v).Action != verifier.ActionEscalate || !strings.Contains(v.Error, "not verifiable") {
		t.Fatalf("verdict = %+v (calls %d)", v, len(m.reqs))
	}
}

func TestPlanAfterVerifierNoCriteriaForcesSecondPass(t *testing.T) {
	v := verifier.Verdict{SchemaVersion: 1, Overall: verifier.OverallApprove}
	d := verifier.Gate(v)
	if p := planAfterVerifier(v, d, false, false); !p.secondPass {
		t.Fatalf("an approval with nothing to verify must still get the LLM review: %+v", p)
	}
}

func TestVerifierArtifactShape(t *testing.T) {
	v, err := verifier.ParseVerdict(verdictRequestChanges, []string{"AC-1", "AC-2"}, verifier.Grounding{})
	if err != nil {
		t.Fatal(err)
	}
	v.ModelInfo = verifier.ModelInfo{Provider: "p", ModelID: "m", Source: verifier.SourceSharedWithReviewer, SharedWithAuthor: true}
	d := verifier.Gate(v)
	work := uuid.New()
	a := verifierArtifact(work, v, d, planAfterVerifier(v, d, true, true), "agent/x", "abc123", "abc123")
	if a.Type != "review_comments" || a.WorkItemID != work || a.CreatedAt.IsZero() {
		t.Fatalf("artifact = %+v", a)
	}
	var c map[string]any
	if err := json.Unmarshal(a.Content, &c); err != nil {
		t.Fatal(err)
	}
	if c["kind"] != "verifier" || c["decision"] != "request_changes" || c["branch"] != "agent/x" || c["head_sha"] != "abc123" || c["verifier_model_shared_with_author"] != true {
		t.Fatalf("content = %v", c)
	}
	// M4-05 reads defects as a {critical, major, minor} object.
	defects, ok := c["defects"].(map[string]any)
	if !ok || defects["critical"] != float64(1) || defects["major"] != float64(0) {
		t.Fatalf("defects = %v", c["defects"])
	}
	verdict := c["verdict"].(map[string]any)
	if list, ok := verdict["defects"].([]any); !ok || len(list) != 1 {
		t.Fatalf("verdict.defects = %v", verdict["defects"])
	}
	if pc := verdict["per_criterion"].([]any); len(pc) != 2 {
		t.Fatalf("per_criterion = %v", pc)
	}
}

func TestSecondPassEnv(t *testing.T) {
	for v, want := range map[string]bool{"": true, "true": true, "1": true, "false": false, "FALSE": false, "0": false, "no": false, "off": false} {
		t.Setenv(secondPassEnv, v)
		if got := secondPassEnabled(); got != want {
			t.Errorf("%s=%q: %v, want %v", secondPassEnv, v, got, want)
		}
	}
}

// The verdict artifact says "approve" only when the approval is final.
func TestVerifierArtifactDecision(t *testing.T) {
	approve := verifier.Verdict{SchemaVersion: 1, Overall: verifier.OverallApprove,
		PerCriterion: []verifier.CriterionResult{{ID: "AC-1", Status: verifier.StatusMet, Evidence: []verifier.Ref{"a.go:1"}}}}
	escalate := verifier.Escalation([]string{"AC-1"}, "not verifiable: x", verifier.ModelInfo{})
	changes, _ := verifier.ParseVerdict(verdictRequestChanges, []string{"AC-1", "AC-2"}, verifier.Grounding{})
	for _, c := range []struct {
		name       string
		v          verifier.Verdict
		secondPass bool
		want       string
	}{
		{"approve, second pass decides", approve, true, "approve_pending_second_pass"},
		{"approve is final", approve, false, "approve"},
		{"escalate", escalate, true, "escalate"},
		{"request changes", changes, true, "request_changes"},
	} {
		d := verifier.Gate(c.v)
		a := verifierArtifact(uuid.New(), c.v, d, planAfterVerifier(c.v, d, c.secondPass, true), "b", "sha", "sha")
		var got verifierArtifactContent
		if err := json.Unmarshal(a.Content, &got); err != nil {
			t.Fatal(err)
		}
		if got.Decision != c.want {
			t.Errorf("%s: decision = %q, want %q", c.name, got.Decision, c.want)
		}
	}
}

// The verifier's executor is rooted at the scratch repository, outside the
// clone.
func TestVerifierExecutorRootedAtScratch(t *testing.T) {
	_, repo := fixtureRepo(t, "03-unhandled-nil")
	var root string
	m := &scriptedModel{answers: []string{verdictApprove}}
	verifyAt(context.Background(), verifierRun{adapter: m, newExecutor: func(path string) *tools.Executor {
		root = path
		return tools.NewExecutor(path)
	}}, uuid.New(), verifier.Requirements{}, verifier.Evidence{}, repo.Path, repo.BaseSHA, repo.BaseSHA, repo.HeadSHA)
	if root == "" || !strings.HasPrefix(filepath.Base(filepath.Dir(root)), verifier.ScratchPrefix) {
		t.Fatalf("executor root = %q", root)
	}
	if rel, err := filepath.Rel(repo.Path, root); err == nil && !strings.HasPrefix(rel, "..") {
		t.Fatalf("executor root %q is inside the clone", root)
	}
}
