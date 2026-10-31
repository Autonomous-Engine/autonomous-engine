package models

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// OperatingMode represents the capability tier an LLM model operates in.
type OperatingMode string

const (
	OperatingModeConstrained OperatingMode = "constrained"
	OperatingModeStandard    OperatingMode = "standard"
	OperatingModeAdvanced    OperatingMode = "advanced"
)

// ModelProfile stores capability benchmarks and metadata for a specific LLM model.
// Numeric capability scores are in the range 0–100.
type ModelProfile struct {
	ID                      uuid.UUID     `json:"id" db:"id"`
	Provider                string        `json:"provider" db:"provider"`
	ModelID                 string        `json:"model_id" db:"model_id"`
	EffectiveContextWindow  int           `json:"effective_context_window" db:"effective_context_window"`
	InstructionFollowing    int           `json:"instruction_following" db:"instruction_following"`
	CodeGeneration          int           `json:"code_generation" db:"code_generation"`
	CodeUnderstanding       int           `json:"code_understanding" db:"code_understanding"`
	Reasoning               int           `json:"reasoning" db:"reasoning"`
	Synthesis               int           `json:"synthesis" db:"synthesis"`
	AvgInferenceSpeed       float64       `json:"avg_inference_speed" db:"avg_inference_speed"`
	ConcurrentCapacity      int           `json:"concurrent_capacity" db:"concurrent_capacity"`
	OperatingMode           OperatingMode `json:"operating_mode" db:"operating_mode"`
	EvaluatedAt             time.Time     `json:"evaluated_at" db:"evaluated_at"`
}

// InferenceLog records a single LLM inference call for auditing and performance tracking.
type InferenceLog struct {
	ID           uuid.UUID  `json:"id" db:"id"`
	ModelProvider string    `json:"model_provider" db:"model_provider"`
	ModelID      string     `json:"model_id" db:"model_id"`
	IntentType   string     `json:"intent_type" db:"intent_type"`
	InputTokens  int        `json:"input_tokens" db:"input_tokens"`
	OutputTokens int        `json:"output_tokens" db:"output_tokens"`
	LatencyMs    int64      `json:"latency_ms" db:"latency_ms"`
	Success      bool       `json:"success" db:"success"`
	ErrorMessage string     `json:"error_message" db:"error_message"`
	AgentID      *uuid.UUID `json:"agent_id,omitempty" db:"agent_id"`
	WorkItemID   *uuid.UUID `json:"work_item_id,omitempty" db:"work_item_id"`
	CreatedAt    time.Time  `json:"created_at" db:"created_at"`
	// PromptMessages is the ordered chat-message list sent to the model
	// (role + content). Raw JSON so callers can decode without importing
	// the llm adapter package. Empty array for in-progress rows.
	PromptMessages  json.RawMessage `json:"prompt_messages" db:"prompt_messages"`
	ResponseContent string          `json:"response_content" db:"response_content"`
}
