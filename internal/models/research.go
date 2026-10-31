package models

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// FindingType classifies the nature of a research finding.
type FindingType string

const (
	FindingTypeFact       FindingType = "fact"
	FindingTypeInference  FindingType = "inference"
	FindingTypeConcern    FindingType = "concern"
	FindingTypeSuggestion FindingType = "suggestion"
)

// Confidence represents the confidence level assigned to a finding or assessment.
type Confidence string

const (
	ConfidenceHigh   Confidence = "high"
	ConfidenceMedium Confidence = "medium"
	ConfidenceLow    Confidence = "low"
)

// ResearchTaskType describes the kind of research operation performed.
type ResearchTaskType string

const (
	ResearchTaskTypeCodeTrace         ResearchTaskType = "code_trace"
	ResearchTaskTypeHistoryAnalysis   ResearchTaskType = "history_analysis"
	ResearchTaskTypePatternMatch      ResearchTaskType = "pattern_match"
	ResearchTaskTypeImpactAssessment  ResearchTaskType = "impact_assessment"
	ResearchTaskTypeBugArchaeology    ResearchTaskType = "bug_archaeology"
)

// ResearchTask represents a unit of research work assigned to a research agent.
type ResearchTask struct {
	ID              uuid.UUID        `json:"id" db:"id"`
	WorkID          uuid.UUID        `json:"work_id" db:"work_id"`
	Type            ResearchTaskType `json:"type" db:"type"`
	StartingContext string           `json:"starting_context" db:"starting_context"`
	CodexSections   []string         `json:"codex_sections" db:"codex_sections"`
	CreatedAt       time.Time        `json:"created_at" db:"created_at"`
}

// ResearchFinding records a specific insight discovered during a research task.
type ResearchFinding struct {
	ID                  uuid.UUID       `json:"id" db:"id"`
	TaskID              uuid.UUID       `json:"task_id" db:"task_id"`
	WorkID              uuid.UUID       `json:"work_id" db:"work_id"`
	FindingType         FindingType     `json:"finding_type" db:"finding_type"`
	Summary             string          `json:"summary" db:"summary"`
	Evidence            json.RawMessage `json:"evidence" db:"evidence"`
	Confidence          Confidence      `json:"confidence" db:"confidence"`
	FollowupSuggestions []string        `json:"followup_suggestions" db:"followup_suggestions"`
	CreatedAt           time.Time       `json:"created_at" db:"created_at"`
}

// FileChange describes a single file modification as part of an implementation plan.
type FileChange struct {
	Path        string `json:"path" db:"path"`
	Description string `json:"description" db:"description"`
}

// TechnicalBrief is the structured output produced by the research phase for a work item.
type TechnicalBrief struct {
	WorkID           uuid.UUID    `json:"work_id" db:"work_id"`
	PRDReference     uuid.UUID    `json:"prd_reference" db:"prd_reference"`
	CodexVersion     int          `json:"codex_version" db:"codex_version"`
	Confidence       string       `json:"confidence" db:"confidence"`
	Complexity       string       `json:"complexity" db:"complexity"`
	ExecutiveSummary string       `json:"executive_summary" db:"executive_summary"`
	CurrentState     string       `json:"current_state" db:"current_state"`
	RootCause        string       `json:"root_cause" db:"root_cause"`
	ProposedApproach string       `json:"proposed_approach" db:"proposed_approach"`
	FilesToModify    []FileChange `json:"files_to_modify" db:"files_to_modify"`
	FilesToCreate    []FileChange `json:"files_to_create" db:"files_to_create"`
	PatternsToFollow []string     `json:"patterns_to_follow" db:"patterns_to_follow"`
	AntiPatterns     []string     `json:"anti_patterns" db:"anti_patterns"`
	EdgeCases        []string     `json:"edge_cases" db:"edge_cases"`
	OpenQuestions    []string     `json:"open_questions" db:"open_questions"`
	TestStrategy     string       `json:"test_strategy" db:"test_strategy"`
}
