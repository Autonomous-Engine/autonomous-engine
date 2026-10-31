package models

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// CodexSectionType represents the category of a codex section.
type CodexSectionType string

const (
	CodexSectionTypeArchitecture CodexSectionType = "architecture"
	CodexSectionTypeModule       CodexSectionType = "module"
	CodexSectionTypePattern      CodexSectionType = "pattern"
	CodexSectionTypeAntiPattern  CodexSectionType = "anti_pattern"
	CodexSectionTypeTesting      CodexSectionType = "testing"
	CodexSectionTypeStyle        CodexSectionType = "style"
	CodexSectionTypeProcess      CodexSectionType = "process"
	CodexSectionTypeDomain       CodexSectionType = "domain"
)

// CodexChangeSource represents the origin of a codex change.
type CodexChangeSource string

const (
	CodexChangeSourceOnboarding          CodexChangeSource = "onboarding"
	CodexChangeSourceExternalChange      CodexChangeSource = "external_change"
	CodexChangeSourceHumanUpdate         CodexChangeSource = "human_update"
	CodexChangeSourceAutomatedReanalysis CodexChangeSource = "automated_reanalysis"
)

// CodexSection represents a versioned section of the project codex.
// Branch defaults to "main" (the migration sets NOT NULL DEFAULT 'main' and
// every pre-021 row was backfilled to 'main'). Callers that leave Branch empty
// on an Upsert write to the main partition; the codex.normalizeBranch helper
// applies the same rule on every read path.
type CodexSection struct {
	ID           uuid.UUID        `json:"id" db:"id"`
	SectionType  CodexSectionType `json:"section_type" db:"section_type"`
	Name         string           `json:"name" db:"name"`
	Content      json.RawMessage  `json:"content" db:"content"`
	Version      int              `json:"version" db:"version"`
	UpdatedAt    time.Time        `json:"updated_at" db:"updated_at"`
	ProductID    *uuid.UUID       `json:"product_id,omitempty" db:"product_id"`
	RepositoryID *uuid.UUID       `json:"repository_id,omitempty" db:"repository_id"`
	Branch       string           `json:"branch,omitempty" db:"branch"`
}

// CodexVersion records a versioned snapshot of codex changes.
type CodexVersion struct {
	ID              uuid.UUID         `json:"id" db:"id"`
	VersionNumber   int               `json:"version_number" db:"version_number"`
	SectionsChanged []string          `json:"sections_changed" db:"sections_changed"`
	ChangeSummary   string            `json:"change_summary" db:"change_summary"`
	ChangeSource    CodexChangeSource `json:"change_source" db:"change_source"`
	Diff            json.RawMessage   `json:"diff" db:"diff"`
	CreatedAt       time.Time         `json:"created_at" db:"created_at"`
	ProductID       *uuid.UUID        `json:"product_id,omitempty" db:"product_id"`
	Branch          string            `json:"branch,omitempty" db:"branch"`
	// ProductVersion numbers the versions of (ProductID, Branch) from 1
	// (migration 060); 0 for a legacy row without a product.
	ProductVersion int `json:"product_version" db:"product_version"`
}

// CodexPattern describes an approved implementation pattern in the codebase.
type CodexPattern struct {
	ID             uuid.UUID  `json:"id" db:"id"`
	Name           string     `json:"name" db:"name"`
	Category       string     `json:"category" db:"category"`
	WhenToUse      string     `json:"when_to_use" db:"when_to_use"`
	ReferenceFiles []string   `json:"reference_files" db:"reference_files"`
	Rationale      string     `json:"rationale" db:"rationale"`
	KeyRules       []string   `json:"key_rules" db:"key_rules"`
	CommonMistakes []string   `json:"common_mistakes" db:"common_mistakes"`
	ProductID      *uuid.UUID `json:"product_id,omitempty" db:"product_id"`
	RepositoryID   *uuid.UUID `json:"repository_id,omitempty" db:"repository_id"`
	Branch         string     `json:"branch,omitempty" db:"branch"`
}

// CodexAntiPattern describes a known problematic pattern to avoid.
type CodexAntiPattern struct {
	ID                uuid.UUID  `json:"id" db:"id"`
	Name              string     `json:"name" db:"name"`
	WhyWrong          string     `json:"why_wrong" db:"why_wrong"`
	WhatInstead       string     `json:"what_instead" db:"what_instead"`
	HistoricalContext string     `json:"historical_context" db:"historical_context"`
	ProductID         *uuid.UUID `json:"product_id,omitempty" db:"product_id"`
	RepositoryID      *uuid.UUID `json:"repository_id,omitempty" db:"repository_id"`
	Branch            string     `json:"branch,omitempty" db:"branch"`
}

// CodexModule describes a module within the codebase as tracked by the codex.
type CodexModule struct {
	ID                uuid.UUID       `json:"id" db:"id"`
	Name              string          `json:"name" db:"name"`
	Purpose           string          `json:"purpose" db:"purpose"`
	PublicInterface   json.RawMessage `json:"public_interface" db:"public_interface"`
	Dependencies      []string        `json:"dependencies" db:"dependencies"`
	Dependents        []string        `json:"dependents" db:"dependents"`
	InternalPatterns  string          `json:"internal_patterns" db:"internal_patterns"`
	OwnerDomain       string          `json:"owner_domain" db:"owner_domain"`
	RiskLevel         string          `json:"risk_level" db:"risk_level"`
	TestCoverageNotes string          `json:"test_coverage_notes" db:"test_coverage_notes"`
	ProductID         *uuid.UUID      `json:"product_id,omitempty" db:"product_id"`
	RepositoryID      *uuid.UUID      `json:"repository_id,omitempty" db:"repository_id"`
	Branch            string          `json:"branch,omitempty" db:"branch"`
}
