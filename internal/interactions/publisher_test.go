package interactions_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/bus"
	"github.com/autonomousengine/autonomous-engine/internal/interactions"
	"github.com/google/uuid"
)

// fakeBus records every publish for assertion.
type fakeBus struct{ pubs []bus.Message }

func (f *fakeBus) Publish(_ context.Context, topic, key string, payload []byte) error {
	f.pubs = append(f.pubs, bus.Message{Topic: topic, Key: key, Value: payload})
	return nil
}
func (f *fakeBus) PublishMessage(_ context.Context, m *bus.Message) error {
	f.pubs = append(f.pubs, *m)
	return nil
}
func (f *fakeBus) Subscribe(string, string, bus.Handler) error           { return nil }
func (f *fakeBus) SubscribeMultiple([]string, string, bus.Handler) error { return nil }
func (f *fakeBus) Close() error                                          { return nil }

func TestPublisher_Requested(t *testing.T) {
	bs := &fakeBus{}
	pub := interactions.NewPublisher(bs)

	workItemID := uuid.New()
	p := &interactions.Prompt{
		ID:          uuid.New(),
		WorkItemID:  workItemID,
		ProductID:   uuid.New(),
		AskingAgent: "implementation-agent",
		Trigger:     interactions.TriggerAgentRequest,
		PromptType:  interactions.PromptTypeClarification,
		Headline:    "Codex lookup failed",
		Question:    "Which did you mean?",
		State:       interactions.StatePending,
		RequestedAt: time.Now().UTC(),
	}
	if err := pub.PublishRequested(context.Background(), p); err != nil {
		t.Fatalf("PublishRequested: %v", err)
	}
	if len(bs.pubs) != 1 {
		t.Fatalf("want 1 publish, got %d", len(bs.pubs))
	}
	got := bs.pubs[0]
	if got.Topic != bus.TopicUserPromptRequested {
		t.Errorf("topic = %q, want %q", got.Topic, bus.TopicUserPromptRequested)
	}
	if got.Key != workItemID.String() {
		t.Errorf("partition key = %q, want %s", got.Key, workItemID)
	}
	var payload interactions.Prompt
	if err := json.Unmarshal(got.Value, &payload); err != nil {
		t.Fatalf("payload json: %v", err)
	}
	if payload.Headline != p.Headline {
		t.Errorf("payload.Headline = %q", payload.Headline)
	}
}

func TestPublisher_Answered(t *testing.T) {
	bs := &fakeBus{}
	pub := interactions.NewPublisher(bs)

	workItemID := uuid.New()
	a := &interactions.AnsweredPayload{
		PromptID:     uuid.New(),
		WorkItemID:   workItemID,
		ProductID:    uuid.New(),
		AskingAgent:  "implementation-agent",
		AnsweredBy:   "admin@localhost",
		AnsweredVia:  "dashboard",
		ActionChosen: "use_sendmail",
		AnswerText:   "sendMail at app/lib/mail.ts",
		AnsweredAt:   time.Now().UTC().Format(time.RFC3339),
		Question:     "Which did you mean?",
	}
	if err := pub.PublishAnswered(context.Background(), a); err != nil {
		t.Fatalf("PublishAnswered: %v", err)
	}
	if len(bs.pubs) != 1 {
		t.Fatalf("want 1 publish, got %d", len(bs.pubs))
	}
	got := bs.pubs[0]
	if got.Topic != bus.TopicUserPromptAnswered {
		t.Errorf("topic = %q", got.Topic)
	}
	if got.Key != workItemID.String() {
		t.Errorf("partition key = %q, want %s", got.Key, workItemID)
	}
}

func TestPublisher_Revoked(t *testing.T) {
	bs := &fakeBus{}
	pub := interactions.NewPublisher(bs)

	workItemID := uuid.New()
	codexID := uuid.New()
	r := &interactions.RevokedPayload{
		PromptID:                uuid.New(),
		WorkItemID:              workItemID,
		CodexEntryIDToDeprecate: &codexID,
		RevokedBy:               "admin@localhost",
		RevocationReason:        "wrong answer",
		RevokedAt:               time.Now().UTC().Format(time.RFC3339),
	}
	if err := pub.PublishRevoked(context.Background(), r); err != nil {
		t.Fatalf("PublishRevoked: %v", err)
	}
	if len(bs.pubs) != 1 {
		t.Fatalf("want 1 publish, got %d", len(bs.pubs))
	}
	if bs.pubs[0].Topic != bus.TopicUserPromptRevoked {
		t.Errorf("topic = %q", bs.pubs[0].Topic)
	}
	if bs.pubs[0].Key != workItemID.String() {
		t.Errorf("partition key = %q, want %s", bs.pubs[0].Key, workItemID)
	}
}
