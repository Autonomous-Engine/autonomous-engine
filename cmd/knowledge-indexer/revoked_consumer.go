// cmd/knowledge-indexer/revoked_consumer.go — on user_prompt.revoked:
//
//	(1) set linked codex entry's state to 'revoked' in the per-section table
//	    identified by user_interactions.codex_entry_table,
//	(2) bump codex version so in-flight work items re-plan,
//	(3) flip qdrant point's payload.revoked to "true" so past_decisions_query
//	    filters it out of future retrievals.
//
// Nothing is deleted — audit trail + recoverability.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"

	"github.com/autonomousengine/autonomous-engine/internal/bus"
	"github.com/autonomousengine/autonomous-engine/internal/codex"
	"github.com/autonomousengine/autonomous-engine/internal/interactions"
	"github.com/autonomousengine/autonomous-engine/internal/models"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// allowedCodexTables is the guard against SQL injection when substituting
// the table name into the UPDATE. Any value outside this set is rejected.
var allowedCodexTables = map[string]struct{}{
	"codex_modules":       {},
	"codex_patterns":      {},
	"codex_anti_patterns": {},
	"codex_sections":      {},
}

type RevokedConsumer struct {
	db        *pgxpool.Pool
	qdrantURL string
	codex     *codex.Codex
	http      *http.Client
}

func NewRevokedConsumer(db *pgxpool.Pool, qdrantURL string) *RevokedConsumer {
	c := &RevokedConsumer{db: db, qdrantURL: qdrantURL, http: http.DefaultClient}
	if db != nil {
		c.codex = codex.New(db)
	}
	return c
}

// Start subscribes to user_prompt.revoked.
func (c *RevokedConsumer) Start(_ context.Context, msgBus bus.MessageBus) error {
	return msgBus.Subscribe(
		bus.TopicUserPromptRevoked,
		"knowledge-indexer-revoked",
		func(ctx context.Context, m *bus.Message) error { return c.Handle(ctx, m) },
	)
}

func (c *RevokedConsumer) Handle(ctx context.Context, m *bus.Message) error {
	var r interactions.RevokedPayload
	if err := json.Unmarshal(m.Value, &r); err != nil {
		log.Printf("revoked_consumer: bad payload: %v", err)
		return nil
	}

	// Look up the interaction row for codex_entry_table + qdrant_point_id + product_id.
	info, err := c.loadInteractionInfo(ctx, r.PromptID)
	if err != nil {
		// If the interaction row is gone (cascaded by a work_item delete),
		// nothing to do. Log and move on.
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return fmt.Errorf("load interaction: %w", err)
	}

	// Step 1 — set codex entry state='revoked' if one exists.
	if r.CodexEntryIDToDeprecate != nil && info.CodexEntryTable != "" {
		if _, ok := allowedCodexTables[info.CodexEntryTable]; !ok {
			log.Printf("revoked_consumer: suspicious codex_entry_table=%q — skipping codex update", info.CodexEntryTable)
		} else {
			name, err := c.markCodexRevoked(ctx, info.CodexEntryTable, *r.CodexEntryIDToDeprecate)
			if err != nil {
				log.Printf("revoked_consumer: mark codex revoked: %v", err)
			}
			// Step 2 — bump codex version, typed (migration 060): the
			// revoked module or section by name, or patterns /
			// anti_patterns. A row that could not be read back (no name)
			// still bumps the version, with no entry.
			if c.codex != nil {
				var sections []string
				if entry, ok := codex.TableChange(info.CodexEntryTable, name); ok {
					sections = []string{entry}
				}
				summary := fmt.Sprintf("revoke user interaction %s", r.PromptID)
				if _, err := c.codex.RecordVersion(ctx, codex.NewVersion{
					ProductID: info.ProductID, SectionsChanged: sections, Summary: summary,
					Source: models.CodexChangeSourceHumanUpdate, Trigger: codex.TriggerInteractionRevoked,
				}); err != nil {
					log.Printf("revoked_consumer: version bump: %v", err)
				}
			}
		}
	}

	// Step 3 — flip qdrant payload.revoked=true. Soft failure.
	if info.QdrantPointID != "" {
		collection := collectionNameFor(info.ProductID)
		if err := c.setQdrantRevoked(ctx, collection, info.QdrantPointID); err != nil {
			log.Printf("revoked_consumer: qdrant flip: %v", err)
		}
	}
	return nil
}

type interactionInfo struct {
	ProductID       uuid.UUID
	CodexEntryTable string
	QdrantPointID   string
}

func (c *RevokedConsumer) loadInteractionInfo(ctx context.Context, promptID uuid.UUID) (*interactionInfo, error) {
	info := &interactionInfo{}
	err := c.db.QueryRow(ctx, `
		SELECT product_id, COALESCE(codex_entry_table, ''), COALESCE(qdrant_point_id, '')
		FROM user_interactions WHERE id = $1
	`, promptID).Scan(&info.ProductID, &info.CodexEntryTable, &info.QdrantPointID)
	if err != nil {
		return nil, err
	}
	return info, nil
}

// markCodexRevoked sets the entry's state to revoked and returns its name
// ("" when no row matched).
func (c *RevokedConsumer) markCodexRevoked(ctx context.Context, table string, entryID uuid.UUID) (string, error) {
	// Table name is whitelisted above — safe to interpolate.
	sql := fmt.Sprintf(`UPDATE %s SET state = 'revoked' WHERE id = $1 RETURNING name`, table)
	var name string
	err := c.db.QueryRow(ctx, sql, entryID).Scan(&name)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return name, err
}

func (c *RevokedConsumer) setQdrantRevoked(ctx context.Context, collection, pointID string) error {
	url := fmt.Sprintf("%s/collections/%s/points/payload", c.qdrantURL, collection)
	body, _ := json.Marshal(map[string]any{
		"payload": map[string]any{"revoked": "true"},
		"points":  []string{pointID},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("qdrant set payload: status %d", resp.StatusCode)
	}
	return nil
}
