package verifier

import (
	"context"
	"errors"
	"log"
	"strings"

	"github.com/autonomousengine/autonomous-engine/internal/configstore"
	"github.com/autonomousengine/autonomous-engine/internal/llm/adapter"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// AuthorModel is the model the implementation (author) ran on.
type AuthorModel struct {
	Provider string
	ModelID  string
}

// LoadAuthorModel reads the author's model from the newest inference_logs
// row tagged role "author" (M4-05) of the work item or its subtasks. ok is
// false when none is recorded.
func LoadAuthorModel(ctx context.Context, q Querier, workID uuid.UUID) (AuthorModel, bool) {
	if q == nil {
		return AuthorModel{}, false
	}
	var m AuthorModel
	err := q.QueryRow(ctx, `SELECT model_provider, model_id FROM inference_logs
		WHERE role = 'author' AND model_id <> ''
		  AND (work_item_id = $1 OR work_item_id IN (SELECT id FROM work_items WHERE parent_id = $1))
		ORDER BY created_at DESC LIMIT 1`, workID).Scan(&m.Provider, &m.ModelID)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			log.Printf("verifier: work_id=%s: read author model: %v", workID, err)
		}
		return AuthorModel{}, false
	}
	return m, true
}

// ResolveModel resolves the verifier's model (resolveModel) and sets
// SharedWithAuthor against the author's model: an equal (provider, model)
// is shared, whichever branch resolved it. When the author's model is
// unknown (nil), the review agent's model fallback is assumed shared (it is
// the author's whenever the work item names an agent definition) and a
// dedicated verifier model is not.
func ResolveModel(ctx context.Context, q Querier, cs *configstore.ConfigStore, shared Shared, author *AuthorModel) (adapter.ModelAdapter, ModelInfo) {
	a, info := resolveModel(ctx, q, cs, shared)
	return a, info.WithAuthor(author)
}

// WithAuthor sets SharedWithAuthor and AuthorModel (see ResolveModel).
func (m ModelInfo) WithAuthor(author *AuthorModel) ModelInfo {
	if author == nil {
		m.AuthorModel = ""
		m.SharedWithAuthor = m.Source == SourceSharedWithReviewer
		return m
	}
	m.AuthorModel = author.Provider + "/" + author.ModelID
	m.SharedWithAuthor = strings.EqualFold(strings.TrimSpace(m.Provider), strings.TrimSpace(author.Provider)) &&
		strings.EqualFold(strings.TrimSpace(m.ModelID), strings.TrimSpace(author.ModelID))
	return m
}
