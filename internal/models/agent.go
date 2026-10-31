package models

import (
	"time"

	"github.com/google/uuid"
)

// AgentType represents the role of an agent in the pipeline.
type AgentType string

const (
	AgentTypeProduct                AgentType = "product"
	AgentTypeResearch               AgentType = "research"
	AgentTypeResearchCoordinator    AgentType = "research_coordinator"
	AgentTypeImplementation         AgentType = "implementation"
	AgentTypeImplementationPlanner  AgentType = "implementation_planner"
	AgentTypePRCoordinator          AgentType = "pr_coordinator"
	AgentTypeReview                 AgentType = "review"
	AgentTypeQA                     AgentType = "qa"
)

// AssignmentStatus represents the lifecycle status of an agent assignment.
type AssignmentStatus string

const (
	AssignmentStatusActive    AssignmentStatus = "active"
	AssignmentStatusCompleted AssignmentStatus = "completed"
	AssignmentStatusFailed    AssignmentStatus = "failed"
	AssignmentStatusCancelled AssignmentStatus = "cancelled"
)

// AgentAssignment records the assignment of an agent instance to a work item.
type AgentAssignment struct {
	ID              uuid.UUID        `json:"id" db:"id"`
	WorkItemID      uuid.UUID        `json:"work_item_id" db:"work_item_id"`
	AgentType       AgentType        `json:"agent_type" db:"agent_type"`
	AgentInstanceID string           `json:"agent_instance_id" db:"agent_instance_id"`
	AssignedAt      time.Time        `json:"assigned_at" db:"assigned_at"`
	CompletedAt     *time.Time       `json:"completed_at,omitempty" db:"completed_at"`
	Status          AssignmentStatus `json:"status" db:"status"`
}
