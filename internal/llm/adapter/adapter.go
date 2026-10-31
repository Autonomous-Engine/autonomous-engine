package adapter

import (
	"context"
	"encoding/json"
)

// Role represents the role of a message participant in a conversation.
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// ChatMessage is a single turn in a conversation.
type ChatMessage struct {
	Role       Role       `json:"role"`
	Content    string     `json:"content"`
	ToolCallID string     `json:"tool_call_id,omitempty"` // for role=tool messages
	Name       string     `json:"name,omitempty"`          // tool name for role=tool
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`    // for role=assistant messages that invoked tools
}

// Tool defines a tool the LLM can call.
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"` // JSON Schema
}

// ToolCall represents the LLM requesting a tool invocation.
type ToolCall struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"` // JSON string
}

// ToolResult is the result of executing a tool call.
type ToolResult struct {
	ToolCallID string `json:"tool_call_id"`
	Content    string `json:"content"`
	IsError    bool   `json:"is_error"`

	// Structured fields set by run_command only (M3-01). Content is
	// unchanged — the model still sees the "[exit code: …]" text — so
	// prompts do not shift; these are for observers (the executor
	// callback, the implementation controller) and are never sent to a
	// provider. ExitCode is nil when the command did not run to an exit
	// status (rejected, failed to start, killed by its timeout).
	ExitCode   *int   `json:"-"`
	DurationMs int64  `json:"-"`
	Command    string `json:"-"`
}

// CompletionRequest is the input to a completion call.
type CompletionRequest struct {
	Messages    []ChatMessage `json:"messages"`
	MaxTokens   int           `json:"max_tokens,omitempty"`
	Temperature float64       `json:"temperature,omitempty"`
	TopP        float64       `json:"top_p,omitempty"`
	JSONMode    bool          `json:"json_mode,omitempty"`
	Stop        []string      `json:"stop,omitempty"`
	Tools       []Tool        `json:"tools,omitempty"`        // available tools
	ToolChoice  string        `json:"tool_choice,omitempty"`  // "auto", "none", "required"
	ToolResults []ToolResult  `json:"tool_results,omitempty"` // results from previous tool calls
}

// CompletionResponse is the output of a completion call.
type CompletionResponse struct {
	Content      string     `json:"content"`
	InputTokens  int        `json:"input_tokens"`
	OutputTokens int        `json:"output_tokens"`
	Model        string     `json:"model"`
	FinishReason string     `json:"finish_reason"` // "stop", "tool_calls", "length"
	ToolCalls    []ToolCall `json:"tool_calls,omitempty"`
}

// StreamChunk is a single chunk in a streaming response.
type StreamChunk struct {
	Content      string `json:"content"`
	Done         bool   `json:"done"`
	InputTokens  int    `json:"input_tokens,omitempty"`
	OutputTokens int    `json:"output_tokens,omitempty"`
}

// ModelCapabilities describes what a model/adapter supports.
type ModelCapabilities struct {
	MaxContextTokens  int  `json:"max_context_tokens"`
	SupportsJSON      bool `json:"supports_json"`
	SupportsStreaming bool `json:"supports_streaming"`
	SupportsFunctions bool `json:"supports_functions"`
}

// ModelAdapter is the common interface all LLM backends must implement.
type ModelAdapter interface {
	// Complete sends a synchronous completion request and returns the full response.
	Complete(ctx context.Context, req CompletionRequest) (*CompletionResponse, error)

	// Stream sends a streaming completion request and returns a channel of chunks.
	// The channel is closed when the stream ends or an error occurs.
	Stream(ctx context.Context, req CompletionRequest) (<-chan StreamChunk, error)

	// Capabilities returns the static capability description of this adapter.
	Capabilities() ModelCapabilities

	// HealthCheck performs a lightweight liveness probe.
	HealthCheck(ctx context.Context) error

	// Name returns a human-readable identifier for the adapter (e.g. "openai/gpt-4o").
	Name() string
}
