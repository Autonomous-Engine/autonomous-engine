package models

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// WorkItemType represents the type of a work item.
type WorkItemType string

const (
	WorkItemTypeFeature  WorkItemType = "feature"
	WorkItemTypeBug      WorkItemType = "bug"
	WorkItemTypeTask     WorkItemType = "task"
	WorkItemTypeSubtask  WorkItemType = "subtask"
	WorkItemTypeResearch WorkItemType = "research"
	WorkItemTypeQA       WorkItemType = "qa"
)

// WorkItemState represents the current state of a work item in the pipeline.
type WorkItemState string

const (
	WorkItemStateIntake          WorkItemState = "intake"
	WorkItemStatePRD             WorkItemState = "prd"
	// PRDApproval is the human-in-the-loop gate between PRD generation and
	// research dispatch. Nothing proceeds past this state until an admin
	// clicks Approve in the dashboard. Catches hallucinated PRDs before
	// they burn 30+ minutes of research-phase LLM time.
	WorkItemStatePRDApproval     WorkItemState = "prd_approval"
	WorkItemStateResearch        WorkItemState = "research"
	WorkItemStatePlanning        WorkItemState = "planning"
	WorkItemStateImplementation  WorkItemState = "implementation"
	// Verification (R14, migration 059) is the parent's state between
	// implementation and review: every subtask is implemented and the
	// independent verifier judges each subtask's head before any PR or
	// pending change exists.
	WorkItemStateVerification    WorkItemState = "verification"
	WorkItemStateReview          WorkItemState = "review"
	WorkItemStateHumanReview     WorkItemState = "human_review"
	WorkItemStateQA              WorkItemState = "qa"
	WorkItemStatePassed          WorkItemState = "passed"
	WorkItemStateFailed          WorkItemState = "failed"
	WorkItemStateAwaitingUser    WorkItemState = "awaiting_user"
	WorkItemStateBlocked         WorkItemState = "blocked"
	WorkItemStateRejected        WorkItemState = "rejected"
	WorkItemStateReleased        WorkItemState = "released"
	WorkItemStateCancelled       WorkItemState = "cancelled"
)

// Priority represents the priority level of a work item.
type Priority string

const (
	PriorityCritical Priority = "critical"
	PriorityHigh     Priority = "high"
	PriorityMedium   Priority = "medium"
	PriorityLow      Priority = "low"
)

// ArtifactType represents the type of an artifact associated with a work item.
type ArtifactType string

const (
	ArtifactTypePRD              ArtifactType = "prd"
	ArtifactTypeTechnicalBrief   ArtifactType = "technical_brief"
	ArtifactTypeResearchFinding  ArtifactType = "research_finding"
	ArtifactTypeQAReport         ArtifactType = "qa_report"
	ArtifactTypeReviewComments   ArtifactType = "review_comments"
)

// WorkItem represents a unit of work tracked through the autonomous pipeline.
type WorkItem struct {
	ID           uuid.UUID     `json:"id" db:"id"`
	ExternalRef  string        `json:"external_ref" db:"external_ref"`
	Type         WorkItemType  `json:"type" db:"type"`
	State        WorkItemState `json:"state" db:"state"`
	Title        string        `json:"title" db:"title"`
	Description  string        `json:"description" db:"description"`
	Priority     Priority      `json:"priority" db:"priority"`
	CodexVersion int           `json:"codex_version" db:"codex_version"`
	ParentID     *uuid.UUID    `json:"parent_id,omitempty" db:"parent_id"`
	ProductID    *uuid.UUID    `json:"product_id,omitempty" db:"product_id"`
	RepositoryID *uuid.UUID    `json:"repository_id,omitempty" db:"repository_id"`
	// Branch is the git branch this work item is targeting. NULL / empty
	// means "use main" (the historical default); a concrete value means
	// the pipeline should resolve codex grounding for that branch and fall
	// back to main if no branch-specific codex has been indexed yet.
	Branch    *string   `json:"branch,omitempty" db:"branch"`
	CreatedAt time.Time `json:"created_at" db:"created_at"`
	UpdatedAt time.Time `json:"updated_at" db:"updated_at"`
}

// WorkItemTransition records a state change for a work item.
type WorkItemTransition struct {
	ID         uuid.UUID     `json:"id" db:"id"`
	WorkItemID uuid.UUID     `json:"work_item_id" db:"work_item_id"`
	FromState  WorkItemState `json:"from_state" db:"from_state"`
	ToState    WorkItemState `json:"to_state" db:"to_state"`
	AgentID    string        `json:"agent_id" db:"agent_id"`
	Reason     string        `json:"reason" db:"reason"`
	Timestamp  time.Time     `json:"timestamp" db:"timestamp"`
}

// WorkItemArtifact represents an artifact produced during processing of a work item.
type WorkItemArtifact struct {
	ID         uuid.UUID       `json:"id" db:"id"`
	WorkItemID uuid.UUID       `json:"work_item_id" db:"work_item_id"`
	Type       ArtifactType    `json:"artifact_type" db:"artifact_type"`
	Content    json.RawMessage `json:"content" db:"content"`
	CreatedAt  time.Time       `json:"created_at" db:"created_at"`
	AgentID    string          `json:"agent_id" db:"agent_id"`
}
