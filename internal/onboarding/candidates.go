// Package onboarding holds shared types and helpers for the proactive
// onboarding pre-fill flow. The indexer produces Candidates, stores them via
// Store, and the onboarding service reads them back for the wizard to confirm.
package onboarding

import (
	"time"

	"github.com/google/uuid"
)

// Confidence is the coarse bucket we report to the user. No 0-100 score —
// we don't have ML-grade precision and pretending we do violates the app's
// Trust Through Honesty principle.
type Confidence string

const (
	ConfidenceHigh        Confidence = "high"
	ConfidenceMedium      Confidence = "medium"
	ConfidenceLow         Confidence = "low"
	ConfidenceNotDetected Confidence = "not_detected"
)

// UserStatus tracks the reviewer's action on a candidate.
type UserStatus string

const (
	StatusPending   UserStatus = "pending"
	StatusConfirmed UserStatus = "confirmed"
	StatusEdited    UserStatus = "edited"
	StatusRejected  UserStatus = "rejected"
)

// QuestionKey enumerates every onboarding question the pre-fill stage answers.
// These must match cmd/onboarding/main.go:orderedQuestions keys exactly.
const (
	QArchitectureOverview = "architecture_overview"
	QTechStack            = "tech_stack"
	QPatternsDiscovered   = "patterns_discovered"
	QAntiPatterns         = "anti_patterns"
	QBranchingStrategy    = "branching_strategy"
	QPRProcess            = "pr_process"
	QDeploymentProcess    = "deployment_process"
	QDomainKnowledge      = "domain_knowledge"
	QCriticalConstraints  = "critical_constraints"
	QTestingPhilosophy    = "testing_philosophy"
	QTestConventions      = "test_conventions"
	QAutonomousScope      = "autonomous_scope"
	QReviewTriggers       = "review_triggers"
	QRiskZones            = "risk_zones"
	QCrossRepoImports     = "cross_repo_imports" // MapDependenciesStep suggestion panel only
)

// Evidence is one justification item attached to a Candidate. Rendered by the
// UI under "How we got this". Stored as JSONB in onboarding_candidates.evidence.
type Evidence struct {
	Kind   string `json:"kind"`             // "file", "module", "branch", "workflow", "import", "error"
	Path   string `json:"path,omitempty"`   // file path relative to repo root
	Lines  string `json:"lines,omitempty"`  // "1-42" or "17"
	Name   string `json:"name,omitempty"`   // module or branch or pattern name
	Detail string `json:"detail,omitempty"` // free-form note (e.g. error message)
}

// Candidate is a single pre-fill answer. Produced by an extractor, written via
// Store.Upsert, read back by the wizard via GET /api/onboarding/candidates.
type Candidate struct {
	ID           uuid.UUID  `json:"id"`
	ProductID    uuid.UUID  `json:"product_id"`
	RepositoryID *uuid.UUID `json:"repository_id,omitempty"`
	QuestionKey  string     `json:"question_key"`
	Value        string     `json:"value"`
	Confidence   Confidence `json:"confidence"`
	Evidence     []Evidence `json:"evidence"`
	Extractor    string     `json:"extractor"`
	UserStatus   UserStatus `json:"user_status"`
	UserValue    *string    `json:"user_value,omitempty"`
	GeneratedAt  time.Time  `json:"generated_at"`
	ReviewedAt   *time.Time `json:"reviewed_at,omitempty"`
}

// EffectiveValue returns UserValue when the user edited the candidate, else
// Value. Callers (approve-codex, LLM prompt builder) should always use this
// instead of reading Value directly.
func (c Candidate) EffectiveValue() string {
	if c.UserValue != nil && *c.UserValue != "" {
		return *c.UserValue
	}
	return c.Value
}

// NotDetected builds a Candidate with confidence=not_detected and an error
// evidence item. Used by the orchestrator when an extractor panics or returns
// nothing — we never silently skip a question.
func NotDetected(productID uuid.UUID, repoID *uuid.UUID, questionKey, extractor, reason string) Candidate {
	return Candidate{
		ProductID:    productID,
		RepositoryID: repoID,
		QuestionKey:  questionKey,
		Value:        "",
		Confidence:   ConfidenceNotDetected,
		Evidence:     []Evidence{{Kind: "error", Detail: reason}},
		Extractor:    extractor,
		UserStatus:   StatusPending,
	}
}
