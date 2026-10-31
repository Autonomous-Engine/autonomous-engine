// Package interactions defines the domain types for the unified interaction
// surface — the layer that lets agents pause for user input, captures user
// answers as permanent institutional knowledge (codex + qdrant), and offers
// programmatic action buttons (retry/continue/cancel/etc.) across dashboard,
// Slack, email, and future channels.
package interactions

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/bus"
	"github.com/autonomousengine/autonomous-engine/internal/outbox"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Publisher handles Kafka publish operations for the three interaction topics.
// Partition key is always work_item_id so events for one work item land on one
// partition and preserve per-work-item ordering across requested → answered → revoked.
type Publisher struct {
	bus bus.MessageBus
}

// NewPublisher creates a new Publisher wrapping the given MessageBus.
func NewPublisher(b bus.MessageBus) *Publisher { return &Publisher{bus: b} }

// PublishRequested publishes a prompt.requested event when a prompt is created.
func (p *Publisher) PublishRequested(ctx context.Context, prompt *Prompt) error {
	payload, err := json.Marshal(prompt)
	if err != nil {
		return err
	}
	return p.bus.Publish(ctx, bus.TopicUserPromptRequested, prompt.WorkItemID.String(), payload)
}

// PublishTx records prompt and its user_prompt.requested event inside the
// caller's transaction: it inserts the user_interactions row and enqueues the
// event through the outbox. Nothing reaches the bus until the transaction
// commits and the service's outbox dispatcher picks the row up, so a state
// change, the prompt row and its event commit together or not at all.
//
// aggregateID is the work item the event is ordered under: the parent work
// item when the prompt belongs to a subtask, otherwise prompt.WorkItemID
// itself. The Kafka key and the payload are unchanged from PublishRequested
// (key = prompt.WorkItemID, payload = the prompt plus the additive event_id),
// so partitioning and consumers behave as before.
//
// It does not use the Publisher's bus; NewPublisher(nil) is fine for a caller
// that only uses PublishTx. PublishRequested stays for callers with no
// transaction.
//
// A zero ID or RequestedAt is filled in on prompt before the write, so the row
// and the event agree. Any error must roll the caller's transaction back.
//
// The row now exists before dashboard-backend's user_prompt.requested
// consumer sees the event, so that consumer's insert finds it already there
// and must still push the websocket notification on a duplicate key.
func (p *Publisher) PublishTx(ctx context.Context, tx pgx.Tx, aggregateID uuid.UUID, prompt *Prompt) error {
	if tx == nil {
		return errors.New("interactions: PublishTx requires the caller's transaction")
	}
	if aggregateID == uuid.Nil {
		return errors.New("interactions: PublishTx requires an aggregate id (the parent work item, or the prompt's own)")
	}
	if prompt == nil {
		return errors.New("interactions: PublishTx requires a prompt")
	}
	if prompt.WorkItemID == uuid.Nil {
		return errors.New("interactions: PublishTx requires the prompt's work item id")
	}
	if prompt.ID == uuid.Nil {
		prompt.ID = uuid.New()
	}
	if prompt.RequestedAt.IsZero() {
		prompt.RequestedAt = time.Now().UTC()
	}
	if err := insertPrompt(ctx, tx, prompt); err != nil {
		return fmt.Errorf("insert user interaction %s: %w", prompt.ID, err)
	}
	if err := outbox.EnqueueJSON(ctx, tx, aggregateID, bus.TopicUserPromptRequested, prompt.WorkItemID.String(), prompt); err != nil {
		return fmt.Errorf("enqueue %s for prompt %s: %w", bus.TopicUserPromptRequested, prompt.ID, err)
	}
	return nil
}

// AnsweredPayload is the wire-level shape of a user answer. Echoes the
// question so triage doesn't need a separate DB read to classify.
type AnsweredPayload struct {
	PromptID       uuid.UUID       `json:"prompt_id"`
	WorkItemID     uuid.UUID       `json:"work_item_id"`
	ProductID      uuid.UUID       `json:"product_id"`
	AskingAgent    string          `json:"asking_agent"`
	AnsweredBy     string          `json:"answered_by"`
	AnsweredVia    string          `json:"answered_via"`
	ActionChosen   string          `json:"action_chosen"`
	AnswerText     string          `json:"answer_text"`
	SaveAsOverride *SaveAsOverride `json:"save_as_override,omitempty"`
	AnsweredAt     string          `json:"answered_at"` // RFC3339
	Question       string          `json:"question"`    // echoed for triage convenience
}

// PublishAnswered publishes a prompt.answered event when a user provides an answer.
func (p *Publisher) PublishAnswered(ctx context.Context, a *AnsweredPayload) error {
	payload, err := json.Marshal(a)
	if err != nil {
		return err
	}
	return p.bus.Publish(ctx, bus.TopicUserPromptAnswered, a.WorkItemID.String(), payload)
}

// RevokedPayload tells knowledge-indexer to deprecate a codex entry (if any)
// and flip the qdrant point to revoked=true. CodexEntryIDToDeprecate is nil
// when the prompt was revoked before triage wrote a codex entry.
type RevokedPayload struct {
	PromptID                uuid.UUID  `json:"prompt_id"`
	WorkItemID              uuid.UUID  `json:"work_item_id"`
	CodexEntryIDToDeprecate *uuid.UUID `json:"codex_entry_id_to_deprecate,omitempty"`
	RevokedBy               string     `json:"revoked_by"`
	RevocationReason        string     `json:"revocation_reason"`
	RevokedAt               string     `json:"revoked_at"` // RFC3339
}

// PublishRevoked publishes a prompt.revoked event when a user revokes an answer.
func (p *Publisher) PublishRevoked(ctx context.Context, r *RevokedPayload) error {
	payload, err := json.Marshal(r)
	if err != nil {
		return err
	}
	return p.bus.Publish(ctx, bus.TopicUserPromptRevoked, r.WorkItemID.String(), payload)
}
