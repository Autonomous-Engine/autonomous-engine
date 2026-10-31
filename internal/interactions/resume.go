// Package interactions — agent_resumption_state persistence.
// Called by the agent tool loop when it yields on request_user_input
// (persist) and by the agent answer-consumer when resuming (load + delete
// after successful resume).
package interactions

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// PersistResumption upserts the snapshot. If a row already exists for the
// prompt_id, message_history / tool_call_stack / saved_at are overwritten.
// This matters because a re-persist may happen if the agent decides to
// refresh state mid-yield (rare but possible for long tool loops).
func (s *Store) PersistResumption(ctx context.Context, rs *ResumptionState) error {
	_, err := s.db.Exec(ctx, `
		INSERT INTO agent_resumption_state (
			prompt_id, work_item_id, asking_agent, message_history, tool_call_stack
		) VALUES ($1, $2, $3, $4::jsonb, $5::jsonb)
		ON CONFLICT (prompt_id) DO UPDATE SET
			message_history = EXCLUDED.message_history,
			tool_call_stack = EXCLUDED.tool_call_stack,
			saved_at = NOW()
	`, rs.PromptID, rs.WorkItemID, rs.AskingAgent, rs.MessageHistory, rs.ToolCallStack)
	return err
}

// LoadResumption fetches the snapshot. Returns ErrNotFound when no row exists.
func (s *Store) LoadResumption(ctx context.Context, promptID uuid.UUID) (*ResumptionState, error) {
	rs := &ResumptionState{}
	err := s.db.QueryRow(ctx, `
		SELECT prompt_id, work_item_id, asking_agent, message_history, tool_call_stack, saved_at
		FROM agent_resumption_state
		WHERE prompt_id = $1
	`, promptID).Scan(&rs.PromptID, &rs.WorkItemID, &rs.AskingAgent,
		&rs.MessageHistory, &rs.ToolCallStack, &rs.SavedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return rs, err
}

// DeleteResumption removes the row. Safe to call after a resume completes.
// Missing rows don't error — the cascade on user_interactions means the row
// may already be gone if the prompt was revoked in parallel.
func (s *Store) DeleteResumption(ctx context.Context, promptID uuid.UUID) error {
	_, err := s.db.Exec(ctx, `DELETE FROM agent_resumption_state WHERE prompt_id = $1`, promptID)
	return err
}
