// Package main defines the dashboard-backend service.
//
// prompts_consumer.go — Kafka consumer for user_prompt.requested. Persists
// the prompt into Postgres and pushes a "prompt_requested" event on the prompts
// WebSocket channel so connected clients update their bell badge + sheet
// immediately.
//
// Duplicate rows: producers converted to the transactional outbox (M1-04)
// write the user_interactions row themselves, with interactions.PublishTx, in
// the same transaction as the event, so by the time this consumer sees the
// event the row usually exists already. On a Postgres unique-violation
// (SQLSTATE 23505) the row is therefore treated as already persisted and the
// WS push still happens — otherwise the bell would never ring for those
// prompts. Without a ledger a redelivered event pushes the same prompt twice
// (clients key prompts by prompt_id); with one (WithLedger, M1-05) it is
// acknowledged without a second push.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"log"

	"github.com/autonomousengine/autonomous-engine/internal/bus"
	"github.com/autonomousengine/autonomous-engine/internal/idempotency"
	"github.com/autonomousengine/autonomous-engine/internal/interactions"
	"github.com/jackc/pgx/v5/pgconn"
)

type promptInserter interface {
	Insert(ctx context.Context, p *interactions.Prompt) error
}

type promptPusher interface {
	PushPromptEvent(eventType string, payload map[string]any)
}

type PromptsConsumer struct {
	store  promptInserter
	hub    promptPusher
	ledger *idempotency.Ledger
}

func NewPromptsConsumer(s promptInserter, h promptPusher) *PromptsConsumer {
	return &PromptsConsumer{store: s, hub: h}
}

// WithLedger deduplicates deliveries on the processed-event ledger (M1-05)
// as consumer "dashboard-backend". The insert is a pooled write (and already
// tolerates an existing row), so the ledger acknowledges a recorded
// duplicate before it runs and records the event after it succeeds: a
// redelivered event no longer pushes the same prompt to the WebSocket twice.
func (c *PromptsConsumer) WithLedger(l *idempotency.Ledger) *PromptsConsumer {
	c.ledger = l
	return c
}

func (c *PromptsConsumer) Start(ctx context.Context, msgBus bus.MessageBus) error {
	handler := func(ctx context.Context, m *bus.Message) error {
		return c.HandleRequested(ctx, m)
	}
	if c.ledger != nil {
		handler = c.ledger.Handler("dashboard-backend", handler)
	}
	return msgBus.Subscribe(bus.TopicUserPromptRequested, "dashboard-backend-prompts", handler)
}

func (c *PromptsConsumer) HandleRequested(ctx context.Context, m *bus.Message) error {
	var p interactions.Prompt
	if err := json.Unmarshal(m.Value, &p); err != nil {
		log.Printf("prompts_consumer: bad payload: %v", err)
		return nil // unparseable — don't redeliver
	}
	if err := c.store.Insert(ctx, &p); err != nil && !isDuplicateKey(err) {
		return err
	}
	// Inserted now, or already written by the producer's transaction
	// (PublishTx) or an earlier delivery: either way, notify.
	c.hub.PushPromptEvent("prompt_requested", map[string]any{
		"prompt_id":    p.ID,
		"prompt_type":  p.PromptType,
		"headline":     p.Headline,
		"work_item_id": p.WorkItemID,
		"product_id":   p.ProductID,
	})
	return nil
}

// isDuplicateKey returns true for Postgres unique_violation (SQLSTATE 23505).
// Also accepts any error type that exposes a SQLState() string method,
// which lets the test double plug in without importing pgconn.
func isDuplicateKey(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "23505"
	}
	type sqlStater interface{ SQLState() string }
	var s sqlStater
	if errors.As(err, &s) {
		return s.SQLState() == "23505"
	}
	return false
}
