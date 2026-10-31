package onboarding

import (
	"testing"

	"github.com/google/uuid"
)

func TestDetectDrift_NoDriftWhenValuesEqual(t *testing.T) {
	pid := uuid.New()
	stored := []Candidate{
		{ProductID: pid, QuestionKey: QBranchingStrategy, Value: "github-flow", UserStatus: StatusConfirmed},
	}
	fresh := []Candidate{
		{ProductID: pid, QuestionKey: QBranchingStrategy, Value: "github-flow", UserStatus: StatusPending},
	}
	drifts := DetectDrift(stored, fresh)
	if len(drifts) != 0 {
		t.Fatalf("expected no drift, got %v", drifts)
	}
}

func TestDetectDrift_NoDriftWhenValuesEqualAfterNormalisation(t *testing.T) {
	pid := uuid.New()
	stored := []Candidate{
		{ProductID: pid, QuestionKey: QBranchingStrategy, Value: "  GitHub-Flow  ", UserStatus: StatusConfirmed},
	}
	fresh := []Candidate{
		{ProductID: pid, QuestionKey: QBranchingStrategy, Value: "github-flow", UserStatus: StatusPending},
	}
	drifts := DetectDrift(stored, fresh)
	if len(drifts) != 0 {
		t.Fatalf("expected no drift after trim/case normalisation, got %v", drifts)
	}
}

func TestDetectDrift_DriftWhenConfirmedValueChanged(t *testing.T) {
	pid := uuid.New()
	stored := []Candidate{
		{ProductID: pid, QuestionKey: QBranchingStrategy, Value: "github-flow", UserStatus: StatusConfirmed},
	}
	fresh := []Candidate{
		{ProductID: pid, QuestionKey: QBranchingStrategy, Value: "gitflow", UserStatus: StatusPending, Extractor: "branching"},
	}
	drifts := DetectDrift(stored, fresh)
	if len(drifts) != 1 {
		t.Fatalf("expected 1 drift, got %d", len(drifts))
	}
	if drifts[0].QuestionKey != QBranchingStrategy {
		t.Fatalf("unexpected key: %q", drifts[0].QuestionKey)
	}
	if drifts[0].OldValue != "github-flow" || drifts[0].NewValue != "gitflow" {
		t.Fatalf("wrong diff: %+v", drifts[0])
	}
	if drifts[0].Extractor != "branching" {
		t.Fatalf("expected extractor carried through, got %q", drifts[0].Extractor)
	}
}

func TestDetectDrift_IgnoresPendingAndRejected(t *testing.T) {
	pid := uuid.New()
	stored := []Candidate{
		{ProductID: pid, QuestionKey: QBranchingStrategy, Value: "x", UserStatus: StatusPending},
		{ProductID: pid, QuestionKey: QTechStack, Value: "x", UserStatus: StatusRejected},
	}
	fresh := []Candidate{
		{ProductID: pid, QuestionKey: QBranchingStrategy, Value: "y"},
		{ProductID: pid, QuestionKey: QTechStack, Value: "y"},
	}
	drifts := DetectDrift(stored, fresh)
	if len(drifts) != 0 {
		t.Fatalf("expected 0 drifts (pending/rejected not watched), got %v", drifts)
	}
}

func TestDetectDrift_EditedRowComparesAgainstUserValue(t *testing.T) {
	pid := uuid.New()
	uv := "trunk-based"
	stored := []Candidate{
		{ProductID: pid, QuestionKey: QBranchingStrategy, Value: "github-flow", UserValue: &uv, UserStatus: StatusEdited},
	}
	fresh := []Candidate{
		{ProductID: pid, QuestionKey: QBranchingStrategy, Value: "trunk-based"},
	}
	drifts := DetectDrift(stored, fresh)
	if len(drifts) != 0 {
		t.Fatalf("expected no drift (user's edit matches fresh detection), got %v", drifts)
	}
}

func TestDetectDrift_EditedRowFlagsWhenUserValueDisagrees(t *testing.T) {
	pid := uuid.New()
	uv := "trunk-based"
	stored := []Candidate{
		{ProductID: pid, QuestionKey: QBranchingStrategy, Value: "github-flow", UserValue: &uv, UserStatus: StatusEdited},
	}
	fresh := []Candidate{
		{ProductID: pid, QuestionKey: QBranchingStrategy, Value: "gitflow"},
	}
	drifts := DetectDrift(stored, fresh)
	if len(drifts) != 1 {
		t.Fatalf("expected 1 drift against user_value, got %d", len(drifts))
	}
	if drifts[0].OldValue != "trunk-based" {
		t.Fatalf("expected OldValue from user edit, got %q", drifts[0].OldValue)
	}
}
