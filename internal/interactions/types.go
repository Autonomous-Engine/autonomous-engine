// Package interactions defines the domain types for the unified interaction
// surface — the layer that lets agents pause for user input, captures user
// answers as permanent institutional knowledge (codex + qdrant), and offers
// programmatic action buttons (retry/continue/cancel/etc.) across dashboard,
// Slack, email, and future channels.
//
// Reference:
package interactions

import (
	"time"

	"github.com/google/uuid"
)

// PromptType distinguishes the class of action required from the user.
type PromptType string

const (
	PromptTypeApproval      PromptType = "approval"
	PromptTypeClarification PromptType = "clarification"
	PromptTypeEscalation    PromptType = "escalation"
	PromptTypeBlockQuestion PromptType = "block_question"
)

// Trigger identifies what caused a prompt to be published.
type Trigger string

const (
	TriggerAgentRequest             Trigger = "agent_request"
	TriggerHeuristicStuck           Trigger = "heuristic:stuck"
	TriggerHeuristicDiminishing     Trigger = "heuristic:diminishing"
	TriggerHeuristicRetrievalContra Trigger = "heuristic:retrieval_contradiction"
	TriggerApprovalPRD              Trigger = "approval:prd"
	TriggerApprovalHumanReview      Trigger = "approval:human_review"
	TriggerApprovalPRGate           Trigger = "approval:pr_gate"
	TriggerPreflightBrokenBuild     Trigger = "preflight:broken_build"
	TriggerBlocked                  Trigger = "blocked"
	// TriggerCodexReplan (codex coherence Part 1): the codex changed under
	// an in-flight item that may not be re-planned automatically (the
	// re-plan cap or its budget is reached). Actions: replan, continue,
	// cancel; status-orchestrator acts on the answer.
	TriggerCodexReplan Trigger = "codex:replan"
)

// State is the lifecycle state of a prompt row.
type State string

const (
	StatePending   State = "pending"
	StateAnswered  State = "answered"
	StateRevoked   State = "revoked"
	StateCancelled State = "cancelled"
)

// SaveAsOverride is the user-supplied override for codex promotion.
// Strictly enumerated in v1 — no custom tags.
type SaveAsOverride string

const (
	SaveAsCodexFact SaveAsOverride = "codex_fact"
	SaveAsEphemeral SaveAsOverride = "ephemeral"
)

// Semantic categorises an action button so UIs across channels can render
// consistent styling and report consistent event semantics upstream.
type Semantic string

const (
	SemanticRetry    Semantic = "retry"
	SemanticRestart  Semantic = "restart"
	SemanticContinue Semantic = "continue"
	SemanticCancel   Semantic = "cancel"
	SemanticStop     Semantic = "stop"
	SemanticOK       Semantic = "ok"
	SemanticCustom   Semantic = "custom"
)

// Action is a single button rendered alongside a prompt.
type Action struct {
	ID       string   `json:"id"`
	Label    string   `json:"label"`
	Semantic Semantic `json:"semantic"`
}

// ClassifiedAs is the triage output — enumerates the knowledge kinds an
// answer can promote to.
type ClassifiedAs string

const (
	ClassifiedCodexModuleCorrection ClassifiedAs = "codex_module_correction"
	ClassifiedCodexPattern          ClassifiedAs = "codex_pattern"
	ClassifiedCodexAntipattern      ClassifiedAs = "codex_antipattern"
	ClassifiedCodexDomainFact       ClassifiedAs = "codex_domain_fact"
	ClassifiedCodexStyleRule        ClassifiedAs = "codex_style_rule"
	ClassifiedAdHoc                 ClassifiedAs = "ad_hoc"
	ClassifiedEphemeral             ClassifiedAs = "ephemeral"
)

// IsCodex reports whether the classification promotes to a codex entry.
func (c ClassifiedAs) IsCodex() bool {
	switch c {
	case ClassifiedCodexModuleCorrection,
		ClassifiedCodexPattern,
		ClassifiedCodexAntipattern,
		ClassifiedCodexDomainFact,
		ClassifiedCodexStyleRule:
		return true
	}
	return false
}

// CodexTable returns the Postgres table name where a codex entry for this
// classification should be written. Non-codex classifications return "".
// domain_fact + style_rule both use codex_sections for v1 because those
// are general-purpose typed rows keyed by section name rather than having
// their own tables; if they grow their own tables later, update this map.
func (c ClassifiedAs) CodexTable() string {
	switch c {
	case ClassifiedCodexModuleCorrection:
		return "codex_modules"
	case ClassifiedCodexPattern:
		return "codex_patterns"
	case ClassifiedCodexAntipattern:
		return "codex_anti_patterns"
	case ClassifiedCodexDomainFact, ClassifiedCodexStyleRule:
		return "codex_sections"
	}
	return ""
}

// Prompt mirrors the user_interactions row. Used for DB reads/writes and
// Kafka payload marshaling. All JSON field names match the Postgres column
// names so the struct round-trips over the wire without a separate DTO.
type Prompt struct {
	ID             uuid.UUID      `json:"id"`
	WorkItemID     uuid.UUID      `json:"work_item_id"`
	ProductID      uuid.UUID      `json:"product_id"`
	AskingAgent    string         `json:"asking_agent"`
	Trigger        Trigger        `json:"trigger"`
	PromptType     PromptType     `json:"prompt_type"`
	Headline       string         `json:"headline"`
	Question       string         `json:"question"`
	ContextLink    string         `json:"context_link,omitempty"`
	ContextPayload map[string]any `json:"context_payload,omitempty"`
	Actions        []Action       `json:"actions"`
	State          State          `json:"state"`
	RequestedAt    time.Time      `json:"requested_at"`
	AnsweredAt     *time.Time     `json:"answered_at,omitempty"`
	PromptedVia    []string       `json:"prompted_via,omitempty"`

	// Answer fields — populated after user responds.
	AnsweredBy     string          `json:"answered_by,omitempty"`
	AnsweredVia    string          `json:"answered_via,omitempty"`
	ActionChosen   string          `json:"action_chosen,omitempty"`
	AnswerText     string          `json:"answer_text,omitempty"`
	SaveAsOverride *SaveAsOverride `json:"save_as_override,omitempty"`

	// Write-back outcome — populated by knowledge-indexer after triage.
	ClassifiedAs             ClassifiedAs `json:"classified_as,omitempty"`
	ClassificationConfidence *float64     `json:"classification_confidence,omitempty"`
	CodexEntryID             *uuid.UUID   `json:"codex_entry_id,omitempty"`
	CodexEntryTable          string       `json:"codex_entry_table,omitempty"` // 'codex_modules' | 'codex_patterns' | 'codex_anti_patterns' | 'codex_sections'
	QdrantPointID            string       `json:"qdrant_point_id,omitempty"`

	// Revocation — populated on revoke.
	RevokedAt        *time.Time `json:"revoked_at,omitempty"`
	RevokedBy        string     `json:"revoked_by,omitempty"`
	RevocationReason string     `json:"revocation_reason,omitempty"`
}

// Decision is a lightweight past-decision record returned from qdrant
// semantic recall (the past_decisions_query tool).
type Decision struct {
	Question         string       `json:"question"`
	Answer           string       `json:"answer"`
	ClassifiedAs     ClassifiedAs `json:"classified_as"`
	SourceWorkItemID uuid.UUID    `json:"source_work_item_id"`
	AnsweredAt       time.Time    `json:"answered_at"`
}

// ResumptionState is the serialized agent tool-loop snapshot persisted while
// a work item is awaiting a user answer. message_history + tool_call_stack
// are opaque JSON blobs to this package; each agent service encodes its own
// shape and decodes on resume.
type ResumptionState struct {
	PromptID       uuid.UUID `json:"prompt_id"`
	WorkItemID     uuid.UUID `json:"work_item_id"`
	AskingAgent    string    `json:"asking_agent"`
	MessageHistory []byte    `json:"message_history"`
	ToolCallStack  []byte    `json:"tool_call_stack"`
	SavedAt        time.Time `json:"saved_at"`
}
