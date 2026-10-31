package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/interactions"
	"github.com/google/uuid"
)

// inMemStore implements PromptsStore (+ Insert for test fixtures).
type inMemStore struct {
	mu   sync.Mutex
	rows map[uuid.UUID]*interactions.Prompt
}

func newInMemStore() *inMemStore { return &inMemStore{rows: map[uuid.UUID]*interactions.Prompt{}} }

func (s *inMemStore) mustInsert(p *interactions.Prompt) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rows[p.ID] = p
}

func (s *inMemStore) GetByID(_ context.Context, id uuid.UUID) (*interactions.Prompt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p, ok := s.rows[id]; ok {
		return p, nil
	}
	return nil, interactions.ErrNotFound
}

func (s *inMemStore) ListPending(_ context.Context, productID *uuid.UUID, limit int) ([]*interactions.Prompt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []*interactions.Prompt{}
	for _, p := range s.rows {
		if p.State != interactions.StatePending {
			continue
		}
		if productID != nil && p.ProductID != *productID {
			continue
		}
		out = append(out, p)
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

func (s *inMemStore) MarkAnswered(_ context.Context, id uuid.UUID, ans interactions.AnswerInput) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.rows[id]
	if !ok || p.State != interactions.StatePending {
		return interactions.ErrNotFound
	}
	p.State = interactions.StateAnswered
	now := time.Now().UTC()
	p.AnsweredAt = &now
	p.AnsweredBy = ans.AnsweredBy
	p.AnsweredVia = ans.AnsweredVia
	p.ActionChosen = ans.ActionChosen
	p.AnswerText = ans.AnswerText
	p.SaveAsOverride = ans.SaveAsOverride
	return nil
}

func (s *inMemStore) MarkRevoked(_ context.Context, id uuid.UUID, by, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.rows[id]
	if !ok {
		return interactions.ErrNotFound
	}
	if p.State != interactions.StatePending && p.State != interactions.StateAnswered {
		return interactions.ErrNotFound
	}
	p.State = interactions.StateRevoked
	now := time.Now().UTC()
	p.RevokedAt = &now
	p.RevokedBy = by
	p.RevocationReason = reason
	return nil
}

func (s *inMemStore) FindByCodexEntry(_ context.Context, codexEntryID uuid.UUID, table string) (uuid.UUID, uuid.UUID, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.rows {
		if p.CodexEntryID != nil && *p.CodexEntryID == codexEntryID {
			return p.ID, p.ProductID, nil
		}
	}
	return uuid.Nil, uuid.Nil, nil
}

// recordingPub captures Answered + Revoked publishes.
type recordingPub struct {
	answered []interactions.AnsweredPayload
	revoked  []interactions.RevokedPayload
}

func (p *recordingPub) PublishAnswered(_ context.Context, a *interactions.AnsweredPayload) error {
	p.answered = append(p.answered, *a)
	return nil
}
func (p *recordingPub) PublishRevoked(_ context.Context, r *interactions.RevokedPayload) error {
	p.revoked = append(p.revoked, *r)
	return nil
}

func TestPromptsHandler_AnswerMarksAnsweredAndPublishes(t *testing.T) {
	store := newInMemStore()
	pub := &recordingPub{}
	h := NewPromptsHandlers(store, pub, "admin@localhost")

	id := uuid.New()
	store.mustInsert(&interactions.Prompt{
		ID:          id,
		WorkItemID:  uuid.New(),
		ProductID:   uuid.New(),
		AskingAgent: "implementation-agent",
		Trigger:     interactions.TriggerAgentRequest,
		PromptType:  interactions.PromptTypeClarification,
		Headline:    "h",
		Question:    "q",
		State:       interactions.StatePending,
		RequestedAt: time.Now().UTC(),
	})

	body, _ := json.Marshal(map[string]any{
		"action_chosen":    "use_sendmail",
		"answer_text":      "yeah use sendMail",
		"save_as_override": "codex_fact",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/prompts/"+id.String()+"/answer", bytes.NewReader(body))
	req.SetPathValue("id", id.String())
	rec := httptest.NewRecorder()
	h.AnswerPrompt(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	got, _ := store.GetByID(context.Background(), id)
	if got.State != interactions.StateAnswered {
		t.Errorf("state = %q, want answered", got.State)
	}
	if len(pub.answered) != 1 {
		t.Fatalf("want 1 answered publish, got %d", len(pub.answered))
	}
	if pub.answered[0].ActionChosen != "use_sendmail" {
		t.Errorf("publish.action_chosen = %q", pub.answered[0].ActionChosen)
	}
}

func TestPromptsHandler_AnswerTwice_Returns409(t *testing.T) {
	store := newInMemStore()
	pub := &recordingPub{}
	h := NewPromptsHandlers(store, pub, "admin@localhost")

	id := uuid.New()
	store.mustInsert(&interactions.Prompt{
		ID:    id,
		State: interactions.StatePending,
	})

	// First answer — 200.
	req1 := httptest.NewRequest(http.MethodPost, "/api/prompts/"+id.String()+"/answer", bytes.NewReader([]byte(`{}`)))
	req1.SetPathValue("id", id.String())
	rec1 := httptest.NewRecorder()
	h.AnswerPrompt(rec1, req1)
	if rec1.Code != http.StatusOK {
		t.Fatalf("first answer: %d %s", rec1.Code, rec1.Body.String())
	}

	// Second answer — CAS fails, 409.
	req2 := httptest.NewRequest(http.MethodPost, "/api/prompts/"+id.String()+"/answer", bytes.NewReader([]byte(`{}`)))
	req2.SetPathValue("id", id.String())
	rec2 := httptest.NewRecorder()
	h.AnswerPrompt(rec2, req2)
	if rec2.Code != http.StatusConflict {
		t.Errorf("second answer status = %d, want 409", rec2.Code)
	}
}

func TestPromptsHandler_RevokePublishesWithCodexBackLink(t *testing.T) {
	store := newInMemStore()
	pub := &recordingPub{}
	h := NewPromptsHandlers(store, pub, "admin@localhost")

	id := uuid.New()
	codexID := uuid.New()
	store.mustInsert(&interactions.Prompt{
		ID:           id,
		WorkItemID:   uuid.New(),
		State:        interactions.StateAnswered,
		CodexEntryID: &codexID,
	})

	body, _ := json.Marshal(map[string]any{"reason": "wrong answer"})
	req := httptest.NewRequest(http.MethodPost, "/api/prompts/"+id.String()+"/revoke", bytes.NewReader(body))
	req.SetPathValue("id", id.String())
	rec := httptest.NewRecorder()
	h.RevokePrompt(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d %s", rec.Code, rec.Body.String())
	}
	if len(pub.revoked) != 1 {
		t.Fatalf("want 1 revoke publish, got %d", len(pub.revoked))
	}
	r := pub.revoked[0]
	if r.CodexEntryIDToDeprecate == nil || *r.CodexEntryIDToDeprecate != codexID {
		t.Errorf("CodexEntryIDToDeprecate missing or wrong: %+v", r.CodexEntryIDToDeprecate)
	}
	if r.RevocationReason != "wrong answer" {
		t.Errorf("reason = %q", r.RevocationReason)
	}
}

func TestPromptsHandler_GetNotFound(t *testing.T) {
	store := newInMemStore()
	h := NewPromptsHandlers(store, &recordingPub{}, "admin@localhost")

	id := uuid.New()
	req := httptest.NewRequest(http.MethodGet, "/api/prompts/"+id.String(), nil)
	req.SetPathValue("id", id.String())
	rec := httptest.NewRecorder()
	h.GetPrompt(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestRevokeCodexEntry_ValidatesTableWhitelist(t *testing.T) {
	h := NewPromptsHandlers(newInMemStore(), &recordingPub{}, "admin@localhost")

	body := strings.NewReader(`{"id":"` + uuid.New().String() + `","table":"; DROP TABLE users"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/codex/entries/revoke", body)
	rec := httptest.NewRecorder()
	h.RevokeCodexEntry(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for bad table name", rec.Code)
	}
}

func TestRevokeCodexEntry_RejectsInvalidJSON(t *testing.T) {
	h := NewPromptsHandlers(newInMemStore(), &recordingPub{}, "admin@localhost")
	req := httptest.NewRequest(http.MethodPost, "/api/codex/entries/revoke", strings.NewReader(`not json`))
	rec := httptest.NewRecorder()
	h.RevokeCodexEntry(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

// Dummy reference so "errors" isn't an unused import if error paths don't use it.
var _ = errors.New
