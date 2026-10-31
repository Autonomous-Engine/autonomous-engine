package main

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/bus"
	"github.com/autonomousengine/autonomous-engine/internal/interactions"
	"github.com/google/uuid"
)

// fakeInserter — narrow implementation of the consumer's store contract.
type fakeInserter struct {
	mu        sync.Mutex
	inserts   []*interactions.Prompt
	duplicate bool // if set, Insert returns a duplicate-key-style error
}

func (f *fakeInserter) Insert(_ context.Context, p *interactions.Prompt) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.duplicate {
		return errDuplicateKeyForTest
	}
	// Copy so the caller's later mutation doesn't affect our record.
	pp := *p
	f.inserts = append(f.inserts, &pp)
	return nil
}

var errDuplicateKeyForTest = &pgxDupErrStub{}

type pgxDupErrStub struct{}

func (*pgxDupErrStub) Error() string { return "duplicate key" }
func (*pgxDupErrStub) SQLState() string { return "23505" }

// recordingHub — narrow implementation of the consumer's WS push contract.
type recordingHub struct {
	mu     sync.Mutex
	pushes []pushRecord
}
type pushRecord struct {
	event   string
	payload map[string]any
}

func (h *recordingHub) PushPromptEvent(eventType string, payload map[string]any) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.pushes = append(h.pushes, pushRecord{event: eventType, payload: payload})
}

func TestPromptsConsumer_RequestedPersistsAndPushes(t *testing.T) {
	store := &fakeInserter{}
	hub := &recordingHub{}
	c := NewPromptsConsumer(store, hub)

	p := &interactions.Prompt{
		ID:          uuid.New(),
		WorkItemID:  uuid.New(),
		ProductID:   uuid.New(),
		AskingAgent: "implementation-agent",
		Trigger:     interactions.TriggerAgentRequest,
		PromptType:  interactions.PromptTypeClarification,
		Headline:    "Codex lookup failed",
		Question:    "Which did you mean?",
		State:       interactions.StatePending,
		RequestedAt: time.Now().UTC(),
	}
	payload, _ := json.Marshal(p)
	msg := &bus.Message{Topic: bus.TopicUserPromptRequested, Value: payload}

	if err := c.HandleRequested(context.Background(), msg); err != nil {
		t.Fatalf("HandleRequested: %v", err)
	}

	if len(store.inserts) != 1 {
		t.Fatalf("want 1 insert, got %d", len(store.inserts))
	}
	got := store.inserts[0]
	if got.Headline != p.Headline {
		t.Errorf("insert.Headline = %q", got.Headline)
	}
	if len(hub.pushes) != 1 {
		t.Fatalf("want 1 WS push, got %d", len(hub.pushes))
	}
	if hub.pushes[0].event != "prompt_requested" {
		t.Errorf("push event = %q", hub.pushes[0].event)
	}
	if hub.pushes[0].payload["prompt_id"] != p.ID {
		t.Errorf("push payload missing prompt_id")
	}
}

func TestPromptsConsumer_DuplicateInsertStillPushes(t *testing.T) {
	// Producers using interactions.PublishTx write the row in their own
	// transaction before the event arrives, so Insert returns a
	// duplicate-key error. The row exists; the consumer must not fail and
	// must still push the WS event, or the bell never rings for them.
	store := &fakeInserter{duplicate: true}
	hub := &recordingHub{}
	c := NewPromptsConsumer(store, hub)

	p := &interactions.Prompt{ID: uuid.New(), WorkItemID: uuid.New(), ProductID: uuid.New()}
	payload, _ := json.Marshal(p)
	if err := c.HandleRequested(context.Background(), &bus.Message{Value: payload}); err != nil {
		t.Errorf("HandleRequested returned error on duplicate (should be nil): %v", err)
	}
	if len(hub.pushes) != 1 || hub.pushes[0].event != "prompt_requested" || hub.pushes[0].payload["prompt_id"] != p.ID {
		t.Errorf("duplicate insert must still push prompt_requested for the prompt; got %+v", hub.pushes)
	}
}

func TestPromptsConsumer_OtherInsertErrorsAreReturnedWithoutPush(t *testing.T) {
	store := &failingInserter{}
	hub := &recordingHub{}
	c := NewPromptsConsumer(store, hub)
	payload, _ := json.Marshal(&interactions.Prompt{ID: uuid.New(), WorkItemID: uuid.New(), ProductID: uuid.New()})
	if err := c.HandleRequested(context.Background(), &bus.Message{Value: payload}); err == nil {
		t.Error("a non-duplicate insert error must be returned for redelivery")
	}
	if len(hub.pushes) != 0 {
		t.Errorf("failed insert pushed %d event(s)", len(hub.pushes))
	}
}

type failingInserter struct{}

func (failingInserter) Insert(context.Context, *interactions.Prompt) error {
	return errors.New("connection refused")
}

func TestPromptsConsumer_BadPayloadIsSilentlyDropped(t *testing.T) {
	// Unparseable payloads should NOT return error (would cause Kafka
	// redelivery loop). Log and drop.
	store := &fakeInserter{}
	hub := &recordingHub{}
	c := NewPromptsConsumer(store, hub)

	if err := c.HandleRequested(context.Background(), &bus.Message{Value: []byte("not json")}); err != nil {
		t.Errorf("bad payload should not error: %v", err)
	}
	if len(store.inserts) != 0 {
		t.Errorf("no insert should happen on bad payload")
	}
}
