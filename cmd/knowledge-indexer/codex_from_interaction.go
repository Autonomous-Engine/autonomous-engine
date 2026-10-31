// cmd/knowledge-indexer/codex_from_interaction.go — consumes
// codex.update.from_interaction events and writes the typed codex entry
// directly into the correct per-section table. Bumps the codex version
// afterward so in-flight work items tagged against the prior version are
// flagged for re-planning (existing codex-version-change machinery).
//
// We write directly with SQL instead of going through the codex package's
// UpsertPattern/UpsertModule/UpsertSection helpers because:
//
//	(a) codex_anti_patterns has no matching Upsert helper yet,
//	(b) the source / source_interaction_id / state columns (added by
//	    migration 024) aren't exposed through the existing helpers, and
//	(c) we need to honor the caller-supplied primary key (CodexEntryID) so
//	    the back-link from user_interactions.codex_entry_id stays valid.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"

	"github.com/autonomousengine/autonomous-engine/internal/bus"
	"github.com/autonomousengine/autonomous-engine/internal/codex"
	"github.com/autonomousengine/autonomous-engine/internal/interactions"
	"github.com/autonomousengine/autonomous-engine/internal/models"
	"github.com/jackc/pgx/v5/pgxpool"
)

// CodexFromInteractionHandler writes a codex entry originating from a user
// answer and bumps the codex version.
type CodexFromInteractionHandler struct {
	db    *pgxpool.Pool
	codex *codex.Codex
}

func NewCodexFromInteractionHandler(db *pgxpool.Pool) *CodexFromInteractionHandler {
	return &CodexFromInteractionHandler{db: db, codex: codex.New(db)}
}

// Start subscribes to codex.update.from_interaction on the given bus.
func (h *CodexFromInteractionHandler) Start(_ context.Context, msgBus bus.MessageBus) error {
	return msgBus.Subscribe(
		bus.TopicCodexUpdateFromInteraction,
		"knowledge-indexer-codex-from-interaction",
		func(ctx context.Context, m *bus.Message) error { return h.Handle(ctx, m) },
	)
}

func (h *CodexFromInteractionHandler) Handle(ctx context.Context, m *bus.Message) error {
	var p CodexUpdateFromInteraction
	if err := json.Unmarshal(m.Value, &p); err != nil {
		log.Printf("codex_from_interaction: bad payload: %v", err)
		return nil // unparseable — don't redeliver
	}

	name := stringFrom(p.EntryPayload, "name")
	if name == "" {
		// Fallback: synthesize a name so the NOT NULL constraint is satisfied.
		// Uses the interaction id so dedup against a later re-classification
		// works. Keep it terse.
		name = "user-" + p.InteractionID.String()[:8]
	}

	var err error
	switch p.Section {
	case "codex_modules":
		err = h.writeModule(ctx, p, name)
	case "codex_patterns":
		err = h.writePattern(ctx, p, name)
	case "codex_anti_patterns":
		err = h.writeAntiPattern(ctx, p, name)
	case "codex_sections":
		err = h.writeSection(ctx, p, name, sectionTypeFor(p.EntryType))
	default:
		log.Printf("codex_from_interaction: unknown section %q", p.Section)
		return nil
	}
	if err != nil {
		return fmt.Errorf("write codex entry: %w", err)
	}

	// Bump codex version (on the product's default branch), typed:
	// module:<name>, section:<name>, patterns or anti_patterns (migration
	// 060); its codex.indexed event commits with it.
	entry, _ := codex.TableChange(p.Section, name)
	summary := fmt.Sprintf("user interaction %s → %s", p.InteractionID, p.Section)
	if _, err := h.codex.RecordVersion(ctx, codex.NewVersion{
		ProductID: p.ProductID, SectionsChanged: []string{entry}, Summary: summary,
		Source: models.CodexChangeSourceHumanUpdate, Trigger: codex.TriggerInteractionAnswer,
	}); err != nil {
		// Don't fail the handler — the entry landed. Log and move on.
		log.Printf("codex_from_interaction: version bump failed product=%s: %v", p.ProductID, err)
	}
	return nil
}

func (h *CodexFromInteractionHandler) writeModule(ctx context.Context, p CodexUpdateFromInteraction, name string) error {
	_, err := h.db.Exec(ctx, `
		INSERT INTO codex_modules (id, product_id, name, source, source_interaction_id, state, purpose)
		VALUES ($1, $2, $3, 'user_interaction', $4, 'active', $5)
	`, p.CodexEntryID, p.ProductID, name, p.InteractionID, stringFrom(p.EntryPayload, "purpose"))
	return err
}

func (h *CodexFromInteractionHandler) writePattern(ctx context.Context, p CodexUpdateFromInteraction, name string) error {
	_, err := h.db.Exec(ctx, `
		INSERT INTO codex_patterns (id, product_id, name, source, source_interaction_id, state,
			rationale, when_to_use)
		VALUES ($1, $2, $3, 'user_interaction', $4, 'active', $5, $6)
	`, p.CodexEntryID, p.ProductID, name, p.InteractionID,
		stringFrom(p.EntryPayload, "rationale"), stringFrom(p.EntryPayload, "when_to_use"))
	return err
}

func (h *CodexFromInteractionHandler) writeAntiPattern(ctx context.Context, p CodexUpdateFromInteraction, name string) error {
	_, err := h.db.Exec(ctx, `
		INSERT INTO codex_anti_patterns (id, product_id, name, source, source_interaction_id, state,
			why_wrong, what_instead)
		VALUES ($1, $2, $3, 'user_interaction', $4, 'active', $5, $6)
	`, p.CodexEntryID, p.ProductID, name, p.InteractionID,
		stringFrom(p.EntryPayload, "why_wrong"), stringFrom(p.EntryPayload, "what_instead"))
	return err
}

func (h *CodexFromInteractionHandler) writeSection(ctx context.Context, p CodexUpdateFromInteraction, name, sectionType string) error {
	contentJSON, _ := json.Marshal(p.EntryPayload)
	if contentJSON == nil {
		contentJSON = []byte("{}")
	}
	_, err := h.db.Exec(ctx, `
		INSERT INTO codex_sections (id, product_id, section_type, name, content, source, source_interaction_id, state)
		VALUES ($1, $2, $3::codex_section_type, $4, $5::jsonb, 'user_interaction', $6, 'active')
	`, p.CodexEntryID, p.ProductID, sectionType, name, contentJSON, p.InteractionID)
	return err
}

// sectionTypeFor maps a ClassifiedAs value to a codex_sections.section_type
// enum value. The enum has 8 values: architecture, module, pattern,
// anti_pattern, testing, style, process, domain. Domain + style are the
// two classifications that land in codex_sections (per Prompt.CodexTable).
func sectionTypeFor(classifiedAs string) string {
	switch classifiedAs {
	case string(interactions.ClassifiedCodexDomainFact):
		return "domain"
	case string(interactions.ClassifiedCodexStyleRule):
		return "style"
	}
	// Unknown — bail to "domain" as a safe default; caller has no way to
	// specify otherwise in the current schema.
	return "domain"
}

func stringFrom(m map[string]any, key string) string {
	if v, ok := m[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}
