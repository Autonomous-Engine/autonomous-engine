// cmd/knowledge-indexer/answered_consumer_test.go
package main

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/bus"
	"github.com/autonomousengine/autonomous-engine/internal/interactions"
	"github.com/google/uuid"
)

// --- fakes ---

type fakeStore struct {
	mu                 sync.Mutex
	lastClassification *interactions.ClassificationResult
	lastPromptID       uuid.UUID
}

func (s *fakeStore) ApplyClassification(_ context.Context, id uuid.UUID, r interactions.ClassificationResult) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastPromptID = id
	rr := r
	s.lastClassification = &rr
	return nil
}

type fakeVec struct {
	mu      sync.Mutex
	upserts []fakeUpsertCall
	err     error
	pointID string
}

type fakeUpsertCall struct {
	productID uuid.UUID
	point     UserDecisionPoint
}

func (v *fakeVec) UpsertUserDecision(_ context.Context, productID uuid.UUID, p UserDecisionPoint) (string, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.upserts = append(v.upserts, fakeUpsertCall{productID: productID, point: p})
	if v.err != nil {
		return "", v.err
	}
	if v.pointID == "" {
		return "qdrant-fake-point", nil
	}
	return v.pointID, nil
}

// fakeCodexPub is the codexPromoter: in production it writes the
// classification (with the codex back-link) and enqueues the event in one
// transaction, so it records both.
type fakeCodexPub struct {
	mu                 sync.Mutex
	published          []CodexUpdateFromInteraction
	lastClassification *interactions.ClassificationResult
	lastWorkItemID     uuid.UUID
	err                error
}

func (c *fakeCodexPub) Promote(_ context.Context, _ uuid.UUID, workItemID uuid.UUID, cls interactions.ClassificationResult, p CodexUpdateFromInteraction) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return c.err
	}
	c.published = append(c.published, p)
	cc := cls
	c.lastClassification = &cc
	c.lastWorkItemID = workItemID
	return nil
}

type fakeTriage struct {
	result *interactions.TriageResult
	err    error
	called bool
}

func (t *fakeTriage) ClassifyWithOverride(_ context.Context, _, _ string, _ *interactions.SaveAsOverride) (*interactions.TriageResult, error) {
	t.called = true
	return t.result, t.err
}

// helper
func msgFor(a *interactions.AnsweredPayload) *bus.Message {
	b, _ := json.Marshal(a)
	return &bus.Message{Topic: bus.TopicUserPromptAnswered, Value: b}
}

// --- tests ---

func TestAnsweredConsumer_PromotesToCodexAndUpsertsQdrant(t *testing.T) {
	store := &fakeStore{}
	vec := &fakeVec{pointID: "qpt-1"}
	codex := &fakeCodexPub{}
	triage := &fakeTriage{result: &interactions.TriageResult{
		ClassifiedAs:  interactions.ClassifiedCodexAntipattern,
		Confidence:    0.91,
		ShouldPromote: true,
	}}

	c := NewAnsweredConsumer(store, vec, codex, triage)

	promptID := uuid.New()
	productID := uuid.New()
	workItemID := uuid.New()
	if err := c.Handle(context.Background(), msgFor(&interactions.AnsweredPayload{
		PromptID:    promptID,
		WorkItemID:  workItemID,
		ProductID:   productID,
		AskingAgent: "impl",
		AnsweredBy:  "admin@localhost",
		AnsweredVia: "dashboard",
		AnswerText:  "don't use int IDs, use GUIDs",
		Question:    "what id type?",
	})); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	if len(vec.upserts) != 1 {
		t.Fatalf("qdrant upsert count = %d, want 1", len(vec.upserts))
	}
	if vec.upserts[0].productID != productID {
		t.Errorf("qdrant upsert productID mismatch")
	}
	if vec.upserts[0].point.Question != "what id type?" {
		t.Errorf("qdrant upsert question = %q", vec.upserts[0].point.Question)
	}
	if len(codex.published) != 1 {
		t.Fatalf("codex publish count = %d, want 1 (ShouldPromote=true)", len(codex.published))
	}
	if codex.published[0].Section != "codex_anti_patterns" {
		t.Errorf("codex section = %q, want codex_anti_patterns", codex.published[0].Section)
	}
	if codex.published[0].Source != "user_interaction" {
		t.Errorf("codex source = %q", codex.published[0].Source)
	}
	// A promoted answer's classification is written by the promoter, in
	// the same transaction as the event, not by the pooled store.
	if store.lastClassification != nil {
		t.Errorf("pooled store was written for a promoted answer: %+v", store.lastClassification)
	}
	cls := codex.lastClassification
	if cls == nil || cls.ClassifiedAs != interactions.ClassifiedCodexAntipattern {
		t.Fatalf("promoter classification not applied: %+v", cls)
	}
	if cls.QdrantPointID != "qpt-1" {
		t.Errorf("qdrant point id = %q, want qpt-1", cls.QdrantPointID)
	}
	if cls.CodexEntryTable != "codex_anti_patterns" {
		t.Errorf("codex_entry_table = %q", cls.CodexEntryTable)
	}
	if cls.CodexEntryID == nil || *cls.CodexEntryID != codex.published[0].CodexEntryID {
		t.Errorf("back-link %v does not match the published entry id %s", cls.CodexEntryID, codex.published[0].CodexEntryID)
	}
	if codex.lastWorkItemID != workItemID {
		t.Errorf("promoter work item = %s, want %s", codex.lastWorkItemID, workItemID)
	}
}

func TestAnsweredConsumer_PromotionFailureStillClassifiesWithoutBackLink(t *testing.T) {
	store := &fakeStore{}
	codex := &fakeCodexPub{err: errors.New("db down")}
	triage := &fakeTriage{result: &interactions.TriageResult{
		ClassifiedAs:  interactions.ClassifiedCodexPattern,
		ShouldPromote: true,
	}}
	c := NewAnsweredConsumer(store, &fakeVec{}, codex, triage)
	if err := c.Handle(context.Background(), msgFor(&interactions.AnsweredPayload{
		PromptID: uuid.New(), WorkItemID: uuid.New(), ProductID: uuid.New(),
		AnswerText: "x", Question: "y",
	})); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if store.lastClassification == nil || store.lastClassification.ClassifiedAs != interactions.ClassifiedCodexPattern {
		t.Fatalf("classification not written after a failed promotion: %+v", store.lastClassification)
	}
	if store.lastClassification.CodexEntryID != nil || store.lastClassification.CodexEntryTable != "" {
		t.Errorf("back-link written for an entry that was never enqueued: %+v", store.lastClassification)
	}
}

func TestAnsweredConsumer_AdHocOnlyQdrants(t *testing.T) {
	store := &fakeStore{}
	vec := &fakeVec{}
	codex := &fakeCodexPub{}
	triage := &fakeTriage{result: &interactions.TriageResult{
		ClassifiedAs:  interactions.ClassifiedAdHoc,
		Confidence:    0.4,
		ShouldPromote: false,
	}}
	c := NewAnsweredConsumer(store, vec, codex, triage)

	if err := c.Handle(context.Background(), msgFor(&interactions.AnsweredPayload{
		PromptID: uuid.New(), WorkItemID: uuid.New(), ProductID: uuid.New(),
		AnswerText: "sure whatever", Question: "name?",
	})); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(vec.upserts) != 1 {
		t.Errorf("qdrant upsert must still happen for ad_hoc, got %d", len(vec.upserts))
	}
	if len(codex.published) != 0 {
		t.Errorf("no codex publish for ad_hoc, got %d", len(codex.published))
	}
	if store.lastClassification == nil || store.lastClassification.ClassifiedAs != interactions.ClassifiedAdHoc {
		t.Errorf("store classification = %+v", store.lastClassification)
	}
}

func TestAnsweredConsumer_EphemeralOverrideSkipsCodex(t *testing.T) {
	store := &fakeStore{}
	vec := &fakeVec{}
	codex := &fakeCodexPub{}
	// Triage returns ephemeral because ClassifyWithOverride short-circuits
	// ephemeral-override before calling the LLM (per Task 10 logic).
	triage := &fakeTriage{result: &interactions.TriageResult{
		ClassifiedAs:  interactions.ClassifiedEphemeral,
		ShouldPromote: false,
	}}
	c := NewAnsweredConsumer(store, vec, codex, triage)

	override := interactions.SaveAsEphemeral
	if err := c.Handle(context.Background(), msgFor(&interactions.AnsweredPayload{
		PromptID: uuid.New(), WorkItemID: uuid.New(), ProductID: uuid.New(),
		AnswerText: "nothing to save", Question: "q",
		SaveAsOverride: &override,
	})); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(codex.published) != 0 {
		t.Errorf("ephemeral override must not publish codex, got %d", len(codex.published))
	}
	if len(vec.upserts) != 1 {
		t.Errorf("ephemeral still gets qdrant upsert, got %d", len(vec.upserts))
	}
}

func TestAnsweredConsumer_QdrantFailureStillClassifies(t *testing.T) {
	// Qdrant upsert failure should NOT block classification write-back.
	// The answer is still recorded; observability comes from logs.
	store := &fakeStore{}
	vec := &fakeVec{err: errors.New("qdrant down")}
	codex := &fakeCodexPub{}
	triage := &fakeTriage{result: &interactions.TriageResult{
		ClassifiedAs:  interactions.ClassifiedAdHoc,
		ShouldPromote: false,
	}}
	c := NewAnsweredConsumer(store, vec, codex, triage)

	if err := c.Handle(context.Background(), msgFor(&interactions.AnsweredPayload{
		PromptID: uuid.New(), WorkItemID: uuid.New(), ProductID: uuid.New(),
		AnswerText: "x", Question: "y",
	})); err != nil {
		t.Errorf("qdrant failure should degrade gracefully, got error: %v", err)
	}
	if store.lastClassification == nil {
		t.Error("classification still must be applied even when qdrant fails")
	}
	if store.lastClassification.QdrantPointID != "" {
		t.Errorf("QdrantPointID should be empty on qdrant failure, got %q", store.lastClassification.QdrantPointID)
	}
}

func TestAnsweredConsumer_BadPayloadSilentlyDropped(t *testing.T) {
	store := &fakeStore{}
	vec := &fakeVec{}
	codex := &fakeCodexPub{}
	triage := &fakeTriage{}
	c := NewAnsweredConsumer(store, vec, codex, triage)

	if err := c.Handle(context.Background(), &bus.Message{Value: []byte("not json")}); err != nil {
		t.Errorf("bad payload must not error (would cause Kafka redelivery): %v", err)
	}
	if triage.called {
		t.Error("triage should not run on unparseable payload")
	}
}
