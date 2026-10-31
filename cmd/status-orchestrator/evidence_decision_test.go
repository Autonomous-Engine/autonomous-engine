package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/agentregistry"
	"github.com/autonomousengine/autonomous-engine/internal/evidence"
	"github.com/autonomousengine/autonomous-engine/internal/models"
	"github.com/autonomousengine/autonomous-engine/internal/workgraph"
	"github.com/google/uuid"
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

// baseEvidence is a valid schema-1 verified record: one passed test check
// against a concrete head SHA.
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

func TestDecideSubtaskTransition(t *testing.T) {
	cases := []struct {
		name    string
		payload implementationCompletePayload
		policy  evidence.ReleasePolicy
		window  evidence.CompatWindow
		// repositoryID is the subtask's repository from its work item row
		// (nil: unknown, not checked), as the handler passes it.
		repositoryID *string
		wantState    models.WorkItemState
		wantReason   []string // substrings that must appear in the reason
	}{
		{
			name:       "verified releases to passed",
			payload:    implementationCompletePayload{TaskID: testTaskID, WorkID: testWorkID, Success: true, Evidence: verifiedJSON(t)},
			policy:     notOptedIn(),
			window:     evidence.CompatWindowLegacy,
			wantState:  models.WorkItemStatePassed,
			wantReason: []string{"outcome verified", "2222222222222222222222222222222222222222"},
		},
		{
			name:       "verified releases in strict mode too",
			payload:    implementationCompletePayload{TaskID: testTaskID, WorkID: testWorkID, Success: true, Evidence: verifiedJSON(t)},
			policy:     unknownProduct(),
			window:     evidence.CompatWindowStrict,
			wantState:  models.WorkItemStatePassed,
			wantReason: []string{"outcome verified"},
		},
		{
			name:       "partially verified with opt-in goes to human review",
			payload:    implementationCompletePayload{TaskID: testTaskID, WorkID: testWorkID, Success: true, Evidence: partiallyVerifiedJSON(t)},
			policy:     optedIn(),
			window:     evidence.CompatWindowLegacy,
			wantState:  models.WorkItemStateHumanReview,
			wantReason: []string{"partially_verified", "opted in", "human review", "lint skipped", "no linter configured"},
		},
		{
			name:       "partially verified without opt-in blocks",
			payload:    implementationCompletePayload{TaskID: testTaskID, WorkID: testWorkID, Success: true, Evidence: partiallyVerifiedJSON(t)},
			policy:     notOptedIn(),
			window:     evidence.CompatWindowLegacy,
			wantState:  models.WorkItemStateBlocked,
			wantReason: []string{"partially_verified", "has not opted in"},
		},
		{
			name:       "partially verified with unknown product blocks",
			payload:    implementationCompletePayload{TaskID: testTaskID, WorkID: testWorkID, Success: true, Evidence: partiallyVerifiedJSON(t)},
			policy:     unknownProduct(),
			window:     evidence.CompatWindowStrict,
			wantState:  models.WorkItemStateBlocked,
			wantReason: []string{"has not opted in"},
		},
		{
			name:       "failed blocks even with opt-in",
			payload:    implementationCompletePayload{TaskID: testTaskID, WorkID: testWorkID, Success: false, Reason: "tests failed on 2222222", Evidence: failedJSON(t)},
			policy:     optedIn(),
			window:     evidence.CompatWindowLegacy,
			wantState:  models.WorkItemStateBlocked,
			wantReason: []string{"outcome failed", "tests failed on 2222222"},
		},
		{
			name:       "unverified blocks even with opt-in",
			payload:    implementationCompletePayload{TaskID: testTaskID, WorkID: testWorkID, Success: true, Evidence: unverifiedJSON(t)},
			policy:     optedIn(),
			window:     evidence.CompatWindowLegacy,
			wantState:  models.WorkItemStateBlocked,
			wantReason: []string{"outcome unverified"},
		},
		{
			name:       "unknown schema version blocks in legacy mode",
			payload:    implementationCompletePayload{TaskID: testTaskID, WorkID: testWorkID, Success: true, Evidence: unknownSchemaJSON(t)},
			policy:     optedIn(),
			window:     evidence.CompatWindowLegacy,
			wantState:  models.WorkItemStateBlocked,
			wantReason: []string{"unknown evidence schema version 2"},
		},
		{
			name:       "unknown schema version blocks in strict mode",
			payload:    implementationCompletePayload{TaskID: testTaskID, WorkID: testWorkID, Success: true, Evidence: unknownSchemaJSON(t)},
			policy:     optedIn(),
			window:     evidence.CompatWindowStrict,
			wantState:  models.WorkItemStateBlocked,
			wantReason: []string{"unknown evidence schema version 2"},
		},
		{
			name:       "no evidence in legacy mode with success=true passes",
			payload:    implementationCompletePayload{TaskID: testTaskID, WorkID: testWorkID, Success: true},
			policy:     notOptedIn(),
			window:     evidence.CompatWindowLegacy,
			wantState:  models.WorkItemStatePassed,
			wantReason: []string{"legacy"},
		},
		{
			name:       "no evidence in legacy mode with success=false blocks with the legacy reason",
			payload:    implementationCompletePayload{TaskID: testTaskID, WorkID: testWorkID, Success: false, Reason: "LLM call failed"},
			policy:     optedIn(),
			window:     evidence.CompatWindowLegacy,
			wantState:  models.WorkItemStateBlocked,
			wantReason: []string{"implementation failed: LLM call failed"},
		},
		{
			name:       "no evidence in strict mode blocks even with success=true",
			payload:    implementationCompletePayload{TaskID: testTaskID, WorkID: testWorkID, Success: true},
			policy:     optedIn(),
			window:     evidence.CompatWindowStrict,
			wantState:  models.WorkItemStateBlocked,
			wantReason: []string{"no evidence attached", "strict"},
		},
		{
			name:       "no evidence in strict mode carries the producer's failure reason",
			payload:    implementationCompletePayload{TaskID: testTaskID, WorkID: testWorkID, Success: false, Reason: "LLM call failed"},
			policy:     optedIn(),
			window:     evidence.CompatWindowStrict,
			wantState:  models.WorkItemStateBlocked,
			wantReason: []string{"no evidence attached", "LLM call failed"},
		},
		{
			name:       "explicit JSON null evidence is treated as absent",
			payload:    implementationCompletePayload{TaskID: testTaskID, WorkID: testWorkID, Success: true, Evidence: json.RawMessage("null")},
			policy:     notOptedIn(),
			window:     evidence.CompatWindowLegacy,
			wantState:  models.WorkItemStatePassed,
			wantReason: []string{"legacy"},
		},
		{
			name:       "empty object evidence is present and blocks in legacy mode",
			payload:    implementationCompletePayload{TaskID: testTaskID, WorkID: testWorkID, Success: true, Evidence: json.RawMessage("{}")},
			policy:     optedIn(),
			window:     evidence.CompatWindowLegacy,
			wantState:  models.WorkItemStateBlocked,
			wantReason: []string{"no evidence attached"},
		},
		{
			name:       "malformed evidence blocks in legacy mode",
			payload:    implementationCompletePayload{TaskID: testTaskID, WorkID: testWorkID, Success: true, Evidence: json.RawMessage(`"not an object"`)},
			policy:     optedIn(),
			window:     evidence.CompatWindowLegacy,
			wantState:  models.WorkItemStateBlocked,
			wantReason: []string{"invalid evidence"},
		},
		{
			name:       "verified evidence contradicted by success=false blocks",
			payload:    implementationCompletePayload{TaskID: testTaskID, WorkID: testWorkID, Success: false, Reason: "push failed", Evidence: verifiedJSON(t)},
			policy:     optedIn(),
			window:     evidence.CompatWindowLegacy,
			wantState:  models.WorkItemStateBlocked,
			wantReason: []string{"success=false", "push failed"},
		},
		{
			name:       "verified evidence for another task blocks (identity mismatch)",
			payload:    implementationCompletePayload{TaskID: testTaskID, WorkID: testWorkID, Success: true, Evidence: foreignTaskJSON(t)},
			policy:     optedIn(),
			window:     evidence.CompatWindowStrict,
			wantState:  models.WorkItemStateBlocked,
			wantReason: []string{"does not belong", "task_id", otherID, testTaskID},
		},
		{
			name:       "verified evidence for another work item blocks (identity mismatch)",
			payload:    implementationCompletePayload{TaskID: testTaskID, WorkID: otherID, Success: true, Evidence: verifiedJSON(t)},
			policy:     optedIn(),
			window:     evidence.CompatWindowLegacy,
			wantState:  models.WorkItemStateBlocked,
			wantReason: []string{"does not belong", "work_id", testWorkID, otherID},
		},
		{
			name:         "verified evidence for another repository blocks (identity mismatch)",
			payload:      implementationCompletePayload{TaskID: testTaskID, WorkID: testWorkID, Success: true, Evidence: repoJSON(t, repoA)},
			policy:       optedIn(),
			window:       evidence.CompatWindowStrict,
			repositoryID: repoIDPtr(repoB),
			wantState:    models.WorkItemStateBlocked,
			wantReason:   []string{"does not belong", "repository_id", repoA, repoB},
		},
		{
			name:         "verified evidence for the subtask's own repository passes",
			payload:      implementationCompletePayload{TaskID: testTaskID, WorkID: testWorkID, Success: true, Evidence: repoJSON(t, repoA)},
			policy:       notOptedIn(),
			window:       evidence.CompatWindowStrict,
			repositoryID: repoIDPtr(repoA),
			wantState:    models.WorkItemStatePassed,
			wantReason:   []string{"outcome verified"},
		},
		{
			name:         "verified evidence without a repository is not repository-checked",
			payload:      implementationCompletePayload{TaskID: testTaskID, WorkID: testWorkID, Success: true, Evidence: verifiedJSON(t)},
			policy:       notOptedIn(),
			window:       evidence.CompatWindowStrict,
			repositoryID: repoIDPtr(repoA),
			wantState:    models.WorkItemStatePassed,
			wantReason:   []string{"outcome verified"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			state, reason := decideSubtaskTransitionFor(tc.payload, tc.policy, tc.window, tc.repositoryID)
			if state != tc.wantState {
				t.Fatalf("state = %q, want %q (reason %q)", state, tc.wantState, reason)
			}
			if strings.TrimSpace(reason) == "" {
				t.Fatalf("reason must be non-empty")
			}
			for _, want := range tc.wantReason {
				if !strings.Contains(reason, want) {
					t.Errorf("reason %q does not contain %q", reason, want)
				}
			}
		})
	}
}

// TestDecideSubtaskTransitionOnlyVerifiedPasses sweeps every outcome under
// both policies and both windows: passed is produced for verified only, and
// human_review for partially_verified with opt-in only.
func TestDecideSubtaskTransitionOnlyVerifiedPasses(t *testing.T) {
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
				state, _ := decideSubtaskTransition(implementationCompletePayload{TaskID: testTaskID, WorkID: testWorkID, Success: success, Evidence: raw}, policy, window)
				optIn := policy.PartialVerificationOptIn
				want := models.WorkItemStateBlocked
				switch {
				case outcome == evidence.OutcomeVerified:
					want = models.WorkItemStatePassed
				case outcome == evidence.OutcomePartiallyVerified && optIn:
					want = models.WorkItemStateHumanReview
				}
				if state != want {
					t.Errorf("outcome=%s optIn=%v window=%s: state = %q, want %q", outcome, optIn, window, state, want)
				}

				// The same record on a message for another task or work
				// item never passes or reaches human review.
				for name, p := range map[string]implementationCompletePayload{
					"other task":      {TaskID: otherID, WorkID: testWorkID, Success: success, Evidence: raw},
					"other work item": {TaskID: testTaskID, WorkID: otherID, Success: success, Evidence: raw},
					"no ids":          {Success: success, Evidence: raw},
				} {
					state, reason := decideSubtaskTransition(p, policy, window)
					if state != models.WorkItemStateBlocked {
						t.Errorf("%s: outcome=%s optIn=%v window=%s: state = %q, want blocked (reason %q)", name, outcome, optIn, window, state, reason)
					}
				}
			}
		}
	}
}

// fakeStore is an in-memory workItemStore for handler-level target tests.
type fakeStore struct {
	items       map[uuid.UUID]*models.WorkItem
	children    map[uuid.UUID][]*models.WorkItem
	transitions map[uuid.UUID][]*models.WorkItemTransition
	childrenErr error
	transErr    error
	applied     []struct {
		ID      uuid.UUID
		ToState models.WorkItemState
		Reason  string
	}
	// created records CreateWorkItemTx calls (fix subtasks).
	created []*models.WorkItem
	// previous is work_items.previous_state, read under the row lock.
	previous map[uuid.UUID]models.WorkItemState
}

func (f *fakeStore) GetWorkItem(_ context.Context, id uuid.UUID) (*models.WorkItem, error) {
	it, ok := f.items[id]
	if !ok {
		return nil, fmt.Errorf("work item %s not found", id)
	}
	return it, nil
}

func (f *fakeStore) GetChildren(_ context.Context, parentID uuid.UUID) ([]*models.WorkItem, error) {
	if f.childrenErr != nil {
		return nil, f.childrenErr
	}
	return f.children[parentID], nil
}

func (f *fakeStore) GetTransitions(_ context.Context, id uuid.UUID) ([]*models.WorkItemTransition, error) {
	if f.transErr != nil {
		return nil, f.transErr
	}
	return f.transitions[id], nil
}

// newParentFixture builds a parent in the given state with one passed
// subtask whose latest pass carries the marker when gated is true.
func newParentFixture(state models.WorkItemState, gated bool) (*fakeStore, uuid.UUID) {
	parentID := uuid.New()
	childID := uuid.New()
	reason := "implementation complete: outcome verified"
	if gated {
		reason = partialVerificationMarker + ": outcome partially_verified and product has opted in"
	}
	return &fakeStore{
		items: map[uuid.UUID]*models.WorkItem{
			parentID: {ID: parentID, Type: models.WorkItemTypeFeature, State: state, Title: "parent"},
			childID:  {ID: childID, Type: models.WorkItemTypeSubtask, State: models.WorkItemStatePassed, Title: "child", ParentID: &parentID},
		},
		children: map[uuid.UUID][]*models.WorkItem{
			parentID: {{ID: childID, Type: models.WorkItemTypeSubtask, State: models.WorkItemStatePassed, Title: "child", ParentID: &parentID}},
		},
		transitions: map[uuid.UUID][]*models.WorkItemTransition{
			childID: {{ToState: models.WorkItemStatePassed, Reason: reason}},
		},
	}, parentID
}

// TestPostReviewTargetsNeverReleaseGatedParent is the state table for both
// QA entry points. The agent definition disables the human_review step so
// the ungated target is qa — the exact configuration under which a gated
// parent in awaiting_user could previously be released to qa on pr.merged.
func TestPostReviewTargetsNeverReleaseGatedParent(t *testing.T) {
	noHumanReviewStep := &agentregistry.AgentDefinition{
		StepResearch: true, StepPlanning: true, StepImplementation: true,
		StepReview: true, StepHumanReview: false, StepQA: true,
	}
	states := []models.WorkItemState{
		models.WorkItemStateReview,
		models.WorkItemStateHumanReview,
		models.WorkItemStateAwaitingUser,
		models.WorkItemStateImplementation,
	}
	for _, state := range states {
		for _, gated := range []bool{false, true} {
			name := fmt.Sprintf("state=%s gated=%v", state, gated)
			t.Run("pr.merged/"+name, func(t *testing.T) {
				store, parentID := newParentFixture(state, gated)
				got, err := resolvePRMergedTarget(context.Background(), store, parentID, noHumanReviewStep)
				if err != nil {
					t.Fatalf("resolve: %v", err)
				}
				assertTarget(t, got, state, gated, models.WorkItemStateQA)
				if !gated && !got.PublishQAReady {
					t.Errorf("ungated pr.merged to qa must publish qa.ready")
				}
			})
			t.Run("review.approved/"+name, func(t *testing.T) {
				store, parentID := newParentFixture(state, gated)
				// human_review_required is false on both the payload and
				// the config: the weakest ungated configuration.
				got, err := resolveReviewApprovedTarget(context.Background(), store, parentID, noHumanReviewStep, false, false)
				if err != nil {
					t.Fatalf("resolve: %v", err)
				}
				assertTarget(t, got, state, gated, models.WorkItemStateQA)
			})
		}
	}
}

// assertTarget encodes the deny-by-target rule: an ungated parent gets the
// pipeline's next state; a gated parent gets human_review or is held there,
// never qa, whatever its current state.
func assertTarget(t *testing.T, got postReviewTarget, current models.WorkItemState, gated bool, ungatedWant models.WorkItemState) {
	t.Helper()
	if got.Gated != gated {
		t.Fatalf("Gated = %v, want %v (%+v)", got.Gated, gated, got)
	}
	if !gated {
		if got.ToState != ungatedWant || got.Hold {
			t.Fatalf("ungated: target = %s hold=%v, want %s", got.ToState, got.Hold, ungatedWant)
		}
		if got.PublishHumanReviewPrompt {
			t.Errorf("ungated qa target must not publish a human review prompt")
		}
		return
	}
	if isReleaseTarget(got.ToState) {
		t.Fatalf("gated parent in %s handed %s: the gate was bypassed", current, got.ToState)
	}
	if got.PublishQAReady {
		t.Errorf("gated parent must never publish qa.ready")
	}
	if !strings.Contains(got.GateReason, "partially verified") || !strings.Contains(got.Reason, got.GateReason) {
		t.Errorf("gated reason %q must carry the gate reason %q", got.Reason, got.GateReason)
	}
	switch {
	case current == models.WorkItemStateHumanReview || !beforeHumanReview(current):
		if !got.Hold || got.ToState != current || got.PublishHumanReviewPrompt || got.PublishBlockedPrompt {
			t.Fatalf("gated in %s must hold without prompts, got %+v", current, got)
		}
	case workgraph.IsValidTransition(current, models.WorkItemStateHumanReview):
		if got.Hold || got.ToState != models.WorkItemStateHumanReview || !got.PublishHumanReviewPrompt || got.PublishBlockedPrompt {
			t.Fatalf("gated in %s must go to human_review with a prompt, got %+v", current, got)
		}
	case workgraph.IsValidTransition(current, models.WorkItemStateBlocked):
		if got.Hold || got.ToState != models.WorkItemStateBlocked || !got.PublishBlockedPrompt || got.PublishHumanReviewPrompt {
			t.Fatalf("gated in %s (no edge to human_review) must block with a prompt, got %+v", current, got)
		}
		if !strings.Contains(got.Reason, "not reachable from state "+string(current)) {
			t.Errorf("dead-end reason %q must say human review is not reachable from %s", got.Reason, current)
		}
	default:
		if !got.Hold {
			t.Fatalf("gated in terminal state %s must hold, got %+v", current, got)
		}
	}
}

// TestParentCompletionTargetNeverReleasesGatedParent covers the third entry
// point: the parent-completion block in handleImplementationComplete. With
// review, human_review, and QA all disabled the ungated target is passed —
// the configuration under which a gated parent previously went straight to
// passed (and then released).
func TestParentCompletionTargetNeverReleasesGatedParent(t *testing.T) {
	nothingAfterImplementation := &agentregistry.AgentDefinition{
		StepResearch: true, StepPlanning: true, StepImplementation: true,
		StepReview: false, StepHumanReview: false, StepQA: false,
	}
	fullPipeline := (*agentregistry.AgentDefinition)(nil)
	onlyQA := &agentregistry.AgentDefinition{StepImplementation: true, StepQA: true}

	cases := []struct {
		name      string
		def       *agentregistry.AgentDefinition
		state     models.WorkItemState
		gated     bool
		wantState models.WorkItemState
		wantHRP   bool // PublishHumanReviewPrompt
		wantBP    bool // PublishBlockedPrompt
	}{
		{"ungated, full pipeline → review", fullPipeline, models.WorkItemStateImplementation, false, models.WorkItemStateReview, false, false},
		{"gated, full pipeline → review (gate applied when it leaves review)", fullPipeline, models.WorkItemStateImplementation, true, models.WorkItemStateReview, false, false},
		{"ungated, nothing after implementation → passed (pre-existing behaviour)", nothingAfterImplementation, models.WorkItemStateImplementation, false, models.WorkItemStatePassed, false, false},
		{"gated, nothing after implementation → blocked, never passed", nothingAfterImplementation, models.WorkItemStateImplementation, true, models.WorkItemStateBlocked, false, true},
		{"gated, only QA after implementation → blocked, never qa", onlyQA, models.WorkItemStateImplementation, true, models.WorkItemStateBlocked, false, true},
		{"gated, parent already in review (review-agent moved it), nothing after → human_review", nothingAfterImplementation, models.WorkItemStateReview, true, models.WorkItemStateHumanReview, true, false},
		{"gated, parent in awaiting_user, nothing after → human_review", nothingAfterImplementation, models.WorkItemStateAwaitingUser, true, models.WorkItemStateHumanReview, true, false},
		{"gated, parent already in human_review → hold", nothingAfterImplementation, models.WorkItemStateHumanReview, true, models.WorkItemStateHumanReview, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store, parentID := newParentFixture(tc.state, tc.gated)
			got, err := resolveParentCompletionTarget(context.Background(), store, parentID, tc.def)
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			if got.Gated != tc.gated {
				t.Fatalf("Gated = %v, want %v (%+v)", got.Gated, tc.gated, got)
			}
			if got.ToState != tc.wantState {
				t.Fatalf("target = %s, want %s (%+v)", got.ToState, tc.wantState, got)
			}
			if tc.gated && isReleaseTarget(got.ToState) {
				t.Fatalf("gated parent handed release target %s", got.ToState)
			}
			if got.PublishHumanReviewPrompt != tc.wantHRP || got.PublishBlockedPrompt != tc.wantBP {
				t.Errorf("prompts: human_review=%v blocked=%v, want %v/%v", got.PublishHumanReviewPrompt, got.PublishBlockedPrompt, tc.wantHRP, tc.wantBP)
			}
			wantHold := tc.gated && tc.state == models.WorkItemStateHumanReview
			if got.Hold != wantHold {
				t.Errorf("hold = %v, want %v", got.Hold, wantHold)
			}
			if !strings.HasPrefix(got.Reason, "all child tasks complete") {
				t.Errorf("reason %q must start with the completion text", got.Reason)
			}
			if tc.wantBP && !strings.Contains(got.Reason, "not reachable from state "+string(tc.state)) {
				t.Errorf("blocked reason %q must name the unreachable state", got.Reason)
			}
		})
	}
}

// TestGatedDeadEndBlocksWithPrompt pins the dead-end rule at the QA entry
// points: a gated parent in a state with no edge to human_review is blocked
// with a visible reason and the blocked prompt; a terminal state is held.
func TestGatedDeadEndBlocksWithPrompt(t *testing.T) {
	for _, state := range []models.WorkItemState{models.WorkItemStateImplementation, models.WorkItemStatePlanning} {
		store, parentID := newParentFixture(state, true)
		got, err := resolvePRMergedTarget(context.Background(), store, parentID, nil)
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if got.ToState != models.WorkItemStateBlocked || !got.PublishBlockedPrompt || got.Hold || got.PublishHumanReviewPrompt {
			t.Fatalf("gated in %s: got %+v, want blocked with prompt", state, got)
		}
		if !strings.Contains(got.Reason, "mandatory human review required but not reachable from state "+string(state)) {
			t.Errorf("reason %q must explain the dead-end", got.Reason)
		}
	}
	store, parentID := newParentFixture(models.WorkItemStateCancelled, true)
	got, err := resolveReviewApprovedTarget(context.Background(), store, parentID, nil, false, false)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !got.Hold || got.PublishBlockedPrompt || got.PublishHumanReviewPrompt {
		t.Fatalf("gated in cancelled must hold, got %+v", got)
	}
}

// TestPostReviewTargetsFailClosedOnReadError: when the gate cannot be read
// the parent is treated as gated.
func TestPostReviewTargetsFailClosedOnReadError(t *testing.T) {
	for _, tc := range []struct {
		name string
		mut  func(*fakeStore)
	}{
		{"children error", func(f *fakeStore) { f.childrenErr = errors.New("db down") }},
		{"transitions error", func(f *fakeStore) { f.transErr = errors.New("db down") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, parentID := newParentFixture(models.WorkItemStateAwaitingUser, false)
			tc.mut(store)
			got, err := resolvePRMergedTarget(context.Background(), store, parentID, nil)
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			if !got.Gated || got.ToState != models.WorkItemStateHumanReview || got.Hold {
				t.Fatalf("read error must gate into human_review, got %+v", got)
			}
			if !strings.Contains(got.GateReason, "db down") {
				t.Errorf("gate reason %q must name the error", got.GateReason)
			}
		})
	}
}

// TestUngatedReviewApprovedKeepsLegacyBranches pins the pre-existing
// review.complete behaviour for ungated parents.
func TestUngatedReviewApprovedKeepsLegacyBranches(t *testing.T) {
	store, parentID := newParentFixture(models.WorkItemStateReview, false)
	full := (*agentregistry.AgentDefinition)(nil) // nil def = every step enabled

	got, _ := resolveReviewApprovedTarget(context.Background(), store, parentID, full, true, false)
	if got.ToState != models.WorkItemStateHumanReview || !got.PublishHumanReviewPrompt || got.PublishQAReady {
		t.Errorf("human_review_required with full pipeline: %+v", got)
	}
	got, _ = resolveReviewApprovedTarget(context.Background(), store, parentID, full, false, false)
	if got.ToState != models.WorkItemStateHumanReview || got.PublishHumanReviewPrompt || got.PublishQAReady {
		t.Errorf("auto-merge with full pipeline (next step is human_review, no prompt as before): %+v", got)
	}
	onlyQA := &agentregistry.AgentDefinition{StepImplementation: true, StepReview: true, StepQA: true}
	got, _ = resolveReviewApprovedTarget(context.Background(), store, parentID, onlyQA, false, false)
	if got.ToState != models.WorkItemStateQA || !got.PublishQAReady {
		t.Errorf("auto-merge with human_review step disabled: %+v", got)
	}
	none := &agentregistry.AgentDefinition{StepImplementation: true, StepReview: true}
	got, _ = resolveReviewApprovedTarget(context.Background(), store, parentID, none, true, false)
	if got.ToState != models.WorkItemStateHumanReview || !got.PublishHumanReviewPrompt {
		t.Errorf("human_review_required with nothing after review must still gate: %+v", got)
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

func TestPartiallyVerifiedSubtasks(t *testing.T) {
	ts := func(i int) time.Time { return time.Date(2026, 9, 10, 0, i, 0, 0, time.UTC) }
	partial := uuid.New()   // passed on partial evidence
	clean := uuid.New()     // passed on verified evidence
	retried := uuid.New()   // passed partial, blocked, retried, passed clean
	regressed := uuid.New() // passed clean, then re-passed partial after a review fix cycle
	blocked := uuid.New()   // never passed
	epic := uuid.New()      // not a subtask (feature); would match by reason but must be ignored

	children := []*models.WorkItem{
		{ID: partial, Type: models.WorkItemTypeSubtask, Title: "add endpoint", State: models.WorkItemStatePassed},
		{ID: clean, Type: models.WorkItemTypeSubtask, Title: "add tests", State: models.WorkItemStatePassed},
		{ID: retried, Type: models.WorkItemTypeSubtask, Title: "retried", State: models.WorkItemStatePassed},
		{ID: regressed, Type: models.WorkItemTypeSubtask, Title: "regressed", State: models.WorkItemStatePassed},
		{ID: blocked, Type: models.WorkItemTypeSubtask, Title: "blocked", State: models.WorkItemStateBlocked},
		{ID: epic, Type: models.WorkItemTypeFeature, Title: "epic", State: models.WorkItemStatePassed},
		nil,
	}
	marker := partialVerificationMarker + ": outcome partially_verified and product has opted in"
	transitions := map[uuid.UUID][]*models.WorkItemTransition{
		partial: {
			{ToState: models.WorkItemStateImplementation, Reason: "started", Timestamp: ts(1)},
			{ToState: models.WorkItemStatePassed, Reason: marker, Timestamp: ts(2)},
		},
		clean: {
			{ToState: models.WorkItemStatePassed, Reason: "implementation complete: outcome verified", Timestamp: ts(2)},
		},
		retried: {
			{ToState: models.WorkItemStatePassed, Reason: marker, Timestamp: ts(1)},
			{ToState: models.WorkItemStateBlocked, Reason: "operator reset", Timestamp: ts(2)},
			{ToState: models.WorkItemStateImplementation, Reason: "retry", Timestamp: ts(3)},
			{ToState: models.WorkItemStatePassed, Reason: "implementation complete: outcome verified", Timestamp: ts(4)},
		},
		regressed: {
			{ToState: models.WorkItemStatePassed, Reason: "implementation complete: outcome verified", Timestamp: ts(1)},
			{ToState: models.WorkItemStateImplementation, Reason: "review requested changes", Timestamp: ts(2)},
			{ToState: models.WorkItemStatePassed, Reason: marker, Timestamp: ts(3)},
		},
		blocked: {
			{ToState: models.WorkItemStateBlocked, Reason: "implementation blocked: outcome failed", Timestamp: ts(1)},
		},
		epic: {
			{ToState: models.WorkItemStatePassed, Reason: marker, Timestamp: ts(1)},
		},
	}
	lookup := func(id uuid.UUID) []*models.WorkItemTransition { return transitions[id] }

	got := partiallyVerifiedSubtasks(children, lookup)
	var ids []uuid.UUID
	for _, c := range got {
		ids = append(ids, c.ID)
	}
	want := []uuid.UUID{partial, regressed}
	if len(ids) != len(want) {
		t.Fatalf("gated = %v, want %v", ids, want)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("gated[%d] = %s, want %s", i, ids[i], want[i])
		}
	}

	if got := partiallyVerifiedSubtasks(nil, lookup); len(got) != 0 {
		t.Fatalf("no children: gated = %v, want none", got)
	}
	if got := partiallyVerifiedSubtasks(children[1:2], lookup); len(got) != 0 {
		t.Fatalf("only clean child: gated = %v, want none", got)
	}
}
