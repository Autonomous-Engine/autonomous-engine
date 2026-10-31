package main

import (
	"context"
	"log"

	"github.com/autonomousengine/autonomous-engine/internal/controller"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

// ─── Implementation controller runs (M3-02) ──────────────────────────────────
//
// The work-item detail shows each subtask's implementation phase and the
// tool calls that proved red and green. Both come from implementation_runs
// and implementation_run_events (migration 052) exactly as the agent
// recorded them; the dashboard infers no phase from anything else.

// RunReader is the narrow controller.Store surface the detail handler uses.
type RunReader interface {
	LatestRuns(ctx context.Context, id uuid.UUID) ([]controller.RunSummary, error)
}

// implementationRuns returns the latest run of every task under id (or of
// id itself when it is a subtask). Failures are logged and yield an empty
// list: the detail page must render without the controller tables.
func (h *Handlers) implementationRuns(ctx context.Context, id uuid.UUID) []controller.RunSummary {
	if h.runs == nil {
		return []controller.RunSummary{}
	}
	runs, err := h.runs.LatestRuns(ctx, id)
	if err != nil {
		log.Printf("handlers: implementation runs for %s: %v", id, err)
		return []controller.RunSummary{}
	}
	if runs == nil {
		return []controller.RunSummary{}
	}
	return runs
}

// consoleToolEvent is one console.stream message. phase is the agent's
// controller state after the call and exit_code is run_command's exit
// status; both are absent for agents without a controller.
type consoleToolEvent struct {
	WorkItemID string `json:"work_item_id"`
	TaskID     string `json:"task_id"`
	AgentType  string `json:"agent_type"`
	Tool       string `json:"tool"`
	Arguments  string `json:"arguments"`
	Result     string `json:"result"`
	IsError    bool   `json:"is_error"`
	Timestamp  string `json:"timestamp"`
	Phase      string `json:"phase,omitempty"`
	ExitCode   *int   `json:"exit_code,omitempty"`
}

// toolEventExecer is the pool surface persistToolEvent needs.
type toolEventExecer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// persistToolEvent writes one console event to tool_events, with its phase
// and exit code (NULL when absent).
func persistToolEvent(ctx context.Context, db toolEventExecer, event consoleToolEvent) error {
	_, err := db.Exec(ctx,
		`INSERT INTO tool_events (work_item_id, task_id, agent_type, tool, arguments, result, is_error, created_at, phase, exit_code)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, COALESCE(NULLIF($8,'')::timestamptz, NOW()), NULLIF($9, ''), $10)`,
		event.WorkItemID, event.TaskID, event.AgentType, event.Tool,
		event.Arguments, event.Result, event.IsError, event.Timestamp, event.Phase, event.ExitCode)
	return err
}
