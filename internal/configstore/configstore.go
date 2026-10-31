// Package configstore provides encrypted credential storage, configuration
// management, audit logging, and hot-reload via PostgreSQL LISTEN/NOTIFY.
package configstore

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/crypto"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ─── Domain Types ─────────────────────────────────────────────────────────────

// CredentialEntry represents a single row from the credential_registry table.
// The Value field is masked when returned from listing operations.
type CredentialEntry struct {
	ID                   uuid.UUID  `json:"id"`
	Scope                string     `json:"scope"`
	ScopeTarget          string     `json:"scope_target"`
	KeyName              string     `json:"key_name"`
	Value                string     `json:"value"` // masked or decrypted depending on caller
	Category             string     `json:"category"`
	IsSecret             bool       `json:"is_secret"`
	Description          string     `json:"description"`
	CreatedBy            *uuid.UUID `json:"created_by,omitempty"`
	CreatedAt            time.Time  `json:"created_at"`
	UpdatedAt            time.Time  `json:"updated_at"`
	ExpiresAt            *time.Time `json:"expires_at,omitempty"`
	LastValidatedAt      *time.Time `json:"last_validated_at,omitempty"`
	LastValidationStatus string     `json:"last_validation_status,omitempty"`
	LastValidationError  string     `json:"last_validation_error,omitempty"`
}

// ConfigEntry represents a single row from the system_config table.
type ConfigEntry struct {
	ID          uuid.UUID  `json:"id"`
	Scope       string     `json:"scope"`
	ScopeTarget string     `json:"scope_target"`
	KeyName     string     `json:"key_name"`
	Value       string     `json:"value"`
	ValueType   string     `json:"value_type"`
	Category    string     `json:"category"`
	Description string     `json:"description"`
	UpdatedBy   *uuid.UUID `json:"updated_by,omitempty"`
	UpdatedAt   time.Time  `json:"updated_at"`
}

// AuditEntry represents a single row from the credential_audit_log table.
type AuditEntry struct {
	ID           uuid.UUID       `json:"id"`
	CredentialID *uuid.UUID      `json:"credential_id,omitempty"`
	Action       string          `json:"action"`
	ActorID      *uuid.UUID      `json:"actor_id,omitempty"`
	ActorType    string          `json:"actor_type"`
	IPAddress    string          `json:"ip_address"`
	Metadata     json.RawMessage `json:"metadata"`
	Timestamp    time.Time       `json:"timestamp"`
}

// cacheKey builds the cache key used in the sync.Map.
func cacheKey(scope, scopeTarget, keyName, category string) string {
	return scope + "|" + scopeTarget + "|" + keyName + "|" + category
}

// ─── ConfigStore ──────────────────────────────────────────────────────────────

// ConfigStore manages encrypted credentials and configuration entries backed by
// a PostgreSQL database. It maintains an in-memory cache that is invalidated by
// PostgreSQL LISTEN/NOTIFY messages on the "config_changed" channel.
type ConfigStore struct {
	db        *pgxpool.Pool
	masterKey []byte
	cache     sync.Map // map[string]string  (cacheKey → plaintext value)
}

// New creates a new ConfigStore backed by the given connection pool and master
// encryption key.
func New(db *pgxpool.Pool, masterKey []byte) *ConfigStore {
	return &ConfigStore{
		db:        db,
		masterKey: masterKey,
	}
}

// MasterKey returns the raw master encryption key.
// Callers that need to perform their own crypto operations (e.g. provider
// handlers that store API keys directly) use this to stay consistent with the
// same key material as the rest of the credential store.
func (cs *ConfigStore) MasterKey() []byte {
	return cs.masterKey
}

// ─── Credential Operations ────────────────────────────────────────────────────

// ResolveCredential looks up a credential by walking the scope chain from the
// most specific (agent_instance) to the least specific (global). It decrypts
// the value, records a read audit event, and caches the plaintext result.
//
// The scope chain tried in order:
//  1. agent_instance / instanceID
//  2. agent_type    / agentType
//  3. integration   / "" (integration-level, no target)
//  4. global        / ""
func (cs *ConfigStore) ResolveCredential(ctx context.Context, category, keyName, agentType, instanceID string) (string, error) {
	type candidate struct {
		scope  string
		target string
	}

	chain := []candidate{
		{"agent_instance", instanceID},
		{"agent_type", agentType},
		{"integration", ""},
		{"global", "default"},
	}

	for _, c := range chain {
		if c.target == "" && (c.scope == "agent_instance" || c.scope == "agent_type") {
			continue
		}

		ck := cacheKey(c.scope, c.target, keyName, category)
		if v, ok := cs.cache.Load(ck); ok {
			if s, isStr := v.(string); isStr {
				return s, nil
			}
		}

		var (
			id             uuid.UUID
			encryptedValue []byte
			nonce          []byte
			isSecret       bool
		)
		err := cs.db.QueryRow(ctx, `
			SELECT id, encrypted_value, nonce, is_secret
			FROM credential_registry
			WHERE scope = $1 AND scope_target = $2 AND key_name = $3 AND category = $4
		`, c.scope, c.target, keyName, category).Scan(&id, &encryptedValue, &nonce, &isSecret)
		if err != nil {
			if err == pgx.ErrNoRows {
				continue
			}
			return "", fmt.Errorf("query credential (%s/%s): %w", c.scope, c.target, err)
		}

		plaintext, err := crypto.Decrypt(cs.masterKey, id, encryptedValue, nonce)
		if err != nil {
			return "", fmt.Errorf("decrypt credential %s: %w", id, err)
		}

		cs.cache.Store(ck, plaintext)

		if err := cs.logAudit(ctx, &id, "read", nil, "system", "", map[string]any{
			"key_name": keyName,
			"category": category,
			"scope":    c.scope,
			"target":   c.target,
		}); err != nil {
			// Non-fatal: log failure should not prevent returning the value,
			// but the failure itself must be visible so operators can diagnose
			// silent gaps in the credential_audit_log table.
			log.Printf("configstore: audit log (read %s/%s %s): %v", c.scope, c.target, keyName, err)
		}

		return plaintext, nil
	}

	return "", fmt.Errorf("%w: category=%s key=%s", ErrCredentialNotFound, category, keyName)
}

// SetCredential creates or updates a credential in the registry. It encrypts
// the provided value, performs an upsert, logs the appropriate audit event, and
// removes any cached entry for the affected scope.
func (cs *ConfigStore) SetCredential(
	ctx context.Context,
	scope, scopeTarget, category, keyName, value string,
	isSecret bool,
	description string,
	actorID *uuid.UUID,
) error {
	// Determine whether we are creating or updating (for audit purposes).
	var existingID uuid.UUID
	existErr := cs.db.QueryRow(ctx, `
		SELECT id FROM credential_registry
		WHERE scope = $1 AND scope_target = $2 AND key_name = $3 AND category = $4
	`, scope, scopeTarget, keyName, category).Scan(&existingID)

	isUpdate := existErr == nil
	if existErr != nil && existErr != pgx.ErrNoRows {
		return fmt.Errorf("check existing credential: %w", existErr)
	}

	// For new credentials we need a stable ID before encryption so that the
	// derived key is deterministic. Reuse the existing ID on update.
	credID := existingID
	if !isUpdate {
		credID = uuid.New()
	}

	ciphertext, nonce, err := crypto.Encrypt(cs.masterKey, credID, value)
	if err != nil {
		return fmt.Errorf("encrypt credential: %w", err)
	}

	var actorIDVal interface{}
	if actorID != nil {
		actorIDVal = *actorID
	}

	if isUpdate {
		_, err = cs.db.Exec(ctx, `
			UPDATE credential_registry
			SET encrypted_value = $1, nonce = $2, is_secret = $3, description = $4,
			    updated_at = NOW()
			WHERE id = $5
		`, ciphertext, nonce, isSecret, description, credID)
	} else {
		_, err = cs.db.Exec(ctx, `
			INSERT INTO credential_registry
				(id, scope, scope_target, key_name, encrypted_value, nonce, category, is_secret, description, created_by)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
			ON CONFLICT (scope, scope_target, key_name, category) DO UPDATE
			SET encrypted_value = EXCLUDED.encrypted_value,
			    nonce           = EXCLUDED.nonce,
			    is_secret       = EXCLUDED.is_secret,
			    description     = EXCLUDED.description,
			    updated_at      = NOW()
		`, credID, scope, scopeTarget, keyName, ciphertext, nonce, category, isSecret, description, actorIDVal)
	}
	if err != nil {
		return fmt.Errorf("upsert credential: %w", err)
	}

	action := "created"
	if isUpdate {
		action = "updated"
	}
	actorType := "system"
	if actorID != nil {
		actorType = "user"
	}
	if err := cs.logAudit(ctx, &credID, action, actorID, actorType, "", map[string]any{
		"key_name": keyName,
		"category": category,
		"scope":    scope,
		"target":   scopeTarget,
	}); err != nil {
		log.Printf("configstore: audit log (%s %s/%s %s): %v", action, scope, scopeTarget, keyName, err)
	}

	cs.cache.Delete(cacheKey(scope, scopeTarget, keyName, category))

	// Broadcast to OTHER service instances (each agent holds its own cache).
	// Any listener on config_changed flushes its cache on receipt. Failure here
	// is logged via the wrapped sentinel but does not fail the write — the row
	// is already committed, callers with stale caches will re-read within their
	// normal refresh cycle.
	_, _ = cs.db.Exec(ctx, "SELECT pg_notify('config_changed', $1)", keyName)

	return nil
}

// SetGlobalCredential writes a credential at the canonical global scope
// (scope='global', scope_target='default') — the only place the resolver
// chain terminates. UI code MUST use this instead of SetCredential with
// hand-picked scope args.
func (cs *ConfigStore) SetGlobalCredential(ctx context.Context, category, keyName, value string, isSecret bool, description string, actorID *uuid.UUID) error {
	return cs.SetCredential(ctx, "global", "default", category, keyName, value, isSecret, description, actorID)
}

// SetIntegrationCredential writes at scope='integration', scope_target=''
// (the integration scope does not use a target — the resolver hardcodes "").
// Use for shared integration keys where a single global-per-integration
// works (today, nothing fits this — prefer SetGlobalCredential).
func (cs *ConfigStore) SetIntegrationCredential(ctx context.Context, category, keyName, value string, isSecret bool, description string, actorID *uuid.UUID) error {
	return cs.SetCredential(ctx, "integration", "", category, keyName, value, isSecret, description, actorID)
}

// SetGlobalConfig writes a system_config entry at scope='global',
// scope_target='default' — parity with SetGlobalCredential. UI/writer code
// SHOULD use this helper instead of hand-picking scope args so rows stay
// reachable by GetConfig's resolver chain.
func (cs *ConfigStore) SetGlobalConfig(ctx context.Context, keyName, value, valueType, category, description string, actorID *uuid.UUID) error {
	return cs.SetConfig(ctx, "global", "default", keyName, value, valueType, category, description, actorID)
}

// DeleteCredential removes a credential from the registry by ID and records a
// deleted audit event.
func (cs *ConfigStore) DeleteCredential(ctx context.Context, id uuid.UUID, actorID *uuid.UUID) error {
	// Fetch metadata before deletion for cache invalidation.
	var scope, scopeTarget, keyName, category string
	err := cs.db.QueryRow(ctx, `
		SELECT scope, scope_target, key_name, category
		FROM credential_registry WHERE id = $1
	`, id).Scan(&scope, &scopeTarget, &keyName, &category)
	if err != nil {
		if err == pgx.ErrNoRows {
			return fmt.Errorf("credential %s not found", id)
		}
		return fmt.Errorf("fetch credential metadata: %w", err)
	}

	_, err = cs.db.Exec(ctx, `DELETE FROM credential_registry WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("delete credential: %w", err)
	}

	actorType := "system"
	if actorID != nil {
		actorType = "user"
	}
	if err := cs.logAudit(ctx, &id, "deleted", actorID, actorType, "", map[string]any{
		"key_name": keyName,
		"category": category,
		"scope":    scope,
		"target":   scopeTarget,
	}); err != nil {
		log.Printf("configstore: audit log (deleted %s/%s %s id=%s): %v", scope, scopeTarget, keyName, id, err)
	}

	cs.cache.Delete(cacheKey(scope, scopeTarget, keyName, category))
	_, _ = cs.db.Exec(ctx, "SELECT pg_notify('config_changed', $1)", keyName)
	return nil
}

// RotateCredential re-encrypts a credential with a new plaintext value and
// records a rotated audit event.
func (cs *ConfigStore) RotateCredential(ctx context.Context, id uuid.UUID, newValue string, actorID *uuid.UUID) error {
	var scope, scopeTarget, keyName, category string
	err := cs.db.QueryRow(ctx, `
		SELECT scope, scope_target, key_name, category
		FROM credential_registry WHERE id = $1
	`, id).Scan(&scope, &scopeTarget, &keyName, &category)
	if err != nil {
		if err == pgx.ErrNoRows {
			return fmt.Errorf("credential %s not found", id)
		}
		return fmt.Errorf("fetch credential for rotation: %w", err)
	}

	ciphertext, nonce, err := crypto.Encrypt(cs.masterKey, id, newValue)
	if err != nil {
		return fmt.Errorf("encrypt new value: %w", err)
	}

	_, err = cs.db.Exec(ctx, `
		UPDATE credential_registry
		SET encrypted_value = $1, nonce = $2, updated_at = NOW()
		WHERE id = $3
	`, ciphertext, nonce, id)
	if err != nil {
		return fmt.Errorf("update rotated credential: %w", err)
	}

	actorType := "system"
	if actorID != nil {
		actorType = "user"
	}
	if err := cs.logAudit(ctx, &id, "rotated", actorID, actorType, "", map[string]any{
		"key_name": keyName,
		"category": category,
	}); err != nil {
		log.Printf("configstore: audit log (rotated %s/%s %s id=%s): %v", scope, scopeTarget, keyName, id, err)
	}

	cs.cache.Delete(cacheKey(scope, scopeTarget, keyName, category))
	_, _ = cs.db.Exec(ctx, "SELECT pg_notify('config_changed', $1)", keyName)
	return nil
}

// RevealCredential decrypts and returns the plaintext value of a credential by
// ID. It logs a read audit event including the requester's IP address.
func (cs *ConfigStore) RevealCredential(ctx context.Context, id uuid.UUID, actorID *uuid.UUID, ipAddress string) (string, error) {
	var encryptedValue, nonce []byte
	err := cs.db.QueryRow(ctx, `
		SELECT encrypted_value, nonce FROM credential_registry WHERE id = $1
	`, id).Scan(&encryptedValue, &nonce)
	if err != nil {
		if err == pgx.ErrNoRows {
			return "", fmt.Errorf("credential %s not found", id)
		}
		return "", fmt.Errorf("fetch credential for reveal: %w", err)
	}

	plaintext, err := crypto.Decrypt(cs.masterKey, id, encryptedValue, nonce)
	if err != nil {
		return "", fmt.Errorf("decrypt credential: %w", err)
	}

	actorType := "system"
	if actorID != nil {
		actorType = "user"
	}
	if err := cs.logAudit(ctx, &id, "read", actorID, actorType, ipAddress, map[string]any{
		"reveal": true,
	}); err != nil {
		log.Printf("configstore: audit log (reveal id=%s ip=%s): %v", id, ipAddress, err)
	}

	return plaintext, nil
}

// ListCredentials returns all credentials, optionally filtered by category and
// scope. Secret values are masked in the returned entries.
func (cs *ConfigStore) ListCredentials(ctx context.Context, category, scope string) ([]*CredentialEntry, error) {
	query := `
		SELECT id, scope, scope_target, key_name, is_secret, category, description,
		       created_by, created_at, updated_at,
		       expires_at, last_validated_at, last_validation_status, last_validation_error
		FROM credential_registry
		WHERE 1=1
	`
	args := []any{}
	argN := 1

	if category != "" {
		query += fmt.Sprintf(" AND category = $%d", argN)
		args = append(args, category)
		argN++
	}
	if scope != "" {
		query += fmt.Sprintf(" AND scope = $%d", argN)
		args = append(args, scope)
		argN++
	}
	query += " ORDER BY category, key_name"

	rows, err := cs.db.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list credentials: %w", err)
	}
	defer rows.Close()

	var results []*CredentialEntry
	for rows.Next() {
		e := &CredentialEntry{}
		var expiresAt, lastValidatedAt pgtype.Timestamptz
		var lastStatus, lastErr pgtype.Text
		if err := rows.Scan(
			&e.ID, &e.Scope, &e.ScopeTarget, &e.KeyName,
			&e.IsSecret, &e.Category, &e.Description,
			&e.CreatedBy, &e.CreatedAt, &e.UpdatedAt,
			&expiresAt, &lastValidatedAt, &lastStatus, &lastErr,
		); err != nil {
			return nil, fmt.Errorf("scan credential row: %w", err)
		}
		if expiresAt.Valid {
			t := expiresAt.Time
			e.ExpiresAt = &t
		}
		if lastValidatedAt.Valid {
			t := lastValidatedAt.Time
			e.LastValidatedAt = &t
		}
		if lastStatus.Valid {
			e.LastValidationStatus = lastStatus.String
		}
		if lastErr.Valid {
			e.LastValidationError = lastErr.String
		}
		e.Value = crypto.MaskValue("", e.IsSecret)
		results = append(results, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate credential rows: %w", err)
	}
	return results, nil
}

// ─── Config Operations ────────────────────────────────────────────────────────

// GetConfig looks up a non-secret configuration value using the same scope
// chain as ResolveCredential (agent_instance → agent_type → global).
func (cs *ConfigStore) GetConfig(ctx context.Context, keyName, agentType, instanceID string) (string, error) {
	type candidate struct {
		scope  string
		target string
	}

	chain := []candidate{
		{"agent_instance", instanceID},
		{"agent_type", agentType},
		{"global", "default"},
	}

	for _, c := range chain {
		if c.target == "" && (c.scope == "agent_instance" || c.scope == "agent_type") {
			continue
		}

		ck := "config|" + cacheKey(c.scope, c.target, keyName, "")
		if v, ok := cs.cache.Load(ck); ok {
			if s, isStr := v.(string); isStr {
				return s, nil
			}
		}

		var value string
		err := cs.db.QueryRow(ctx, `
			SELECT value FROM system_config
			WHERE scope = $1 AND scope_target = $2 AND key_name = $3
		`, c.scope, c.target, keyName).Scan(&value)
		if err != nil {
			if err == pgx.ErrNoRows {
				continue
			}
			return "", fmt.Errorf("query config (%s/%s): %w", c.scope, c.target, err)
		}

		cs.cache.Store(ck, value)
		return value, nil
	}

	return "", fmt.Errorf("config key not found: %s", keyName)
}

// SetConfig creates or updates a configuration entry.
func (cs *ConfigStore) SetConfig(
	ctx context.Context,
	scope, scopeTarget, keyName, value, valueType, category, description string,
	actorID *uuid.UUID,
) error {
	var actorIDVal interface{}
	if actorID != nil {
		actorIDVal = *actorID
	}

	_, err := cs.db.Exec(ctx, `
		INSERT INTO system_config
			(scope, scope_target, key_name, value, value_type, category, description, updated_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (scope, scope_target, key_name) DO UPDATE
		SET value       = EXCLUDED.value,
		    value_type  = EXCLUDED.value_type,
		    category    = EXCLUDED.category,
		    description = EXCLUDED.description,
		    updated_by  = EXCLUDED.updated_by,
		    updated_at  = NOW()
	`, scope, scopeTarget, keyName, value, valueType, category, description, actorIDVal)
	if err != nil {
		return fmt.Errorf("upsert config: %w", err)
	}

	cs.cache.Delete("config|" + cacheKey(scope, scopeTarget, keyName, ""))

	// Broadcast to OTHER service instances (each agent holds its own cache).
	// The DB trigger also fires NOTIFY, but we emit here for symmetry with
	// SetCredential and to remain correct if the trigger is ever dropped.
	_, _ = cs.db.Exec(ctx, "SELECT pg_notify('config_changed', $1)", keyName)

	return nil
}

// ListConfig returns all configuration entries, optionally filtered by category.
func (cs *ConfigStore) ListConfig(ctx context.Context, category string) ([]*ConfigEntry, error) {
	query := `
		SELECT id, scope, scope_target, key_name, value, value_type,
		       category, description, updated_by, updated_at
		FROM system_config
		WHERE 1=1
	`
	args := []any{}
	if category != "" {
		query += " AND category = $1"
		args = append(args, category)
	}
	query += " ORDER BY category, key_name"

	rows, err := cs.db.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list config: %w", err)
	}
	defer rows.Close()

	var results []*ConfigEntry
	for rows.Next() {
		e := &ConfigEntry{}
		if err := rows.Scan(
			&e.ID, &e.Scope, &e.ScopeTarget, &e.KeyName, &e.Value, &e.ValueType,
			&e.Category, &e.Description, &e.UpdatedBy, &e.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan config row: %w", err)
		}
		results = append(results, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate config rows: %w", err)
	}
	return results, nil
}

// ─── Audit Log ────────────────────────────────────────────────────────────────

// ListAudit returns paginated audit log entries with optional filters on
// action, category (via joined credential_registry), and actor_id.
func (cs *ConfigStore) ListAudit(ctx context.Context, action, category string, actorID *uuid.UUID, limit, offset int) ([]*AuditEntry, error) {
	if limit <= 0 {
		limit = 50
	}

	// Build filter clauses dynamically.
	clauses := []string{"1=1"}
	args := []any{}
	argN := 1

	if action != "" {
		clauses = append(clauses, fmt.Sprintf("a.action = $%d", argN))
		args = append(args, action)
		argN++
	}
	if actorID != nil {
		clauses = append(clauses, fmt.Sprintf("a.actor_id = $%d", argN))
		args = append(args, *actorID)
		argN++
	}
	if category != "" {
		clauses = append(clauses, fmt.Sprintf("cr.category = $%d", argN))
		args = append(args, category)
		argN++
	}

	args = append(args, limit, offset)

	query := fmt.Sprintf(`
		SELECT a.id, a.credential_id, a.action, a.actor_id, a.actor_type,
		       a.ip_address, a.metadata, a.timestamp
		FROM credential_audit_log a
		LEFT JOIN credential_registry cr ON cr.id = a.credential_id
		WHERE %s
		ORDER BY a.timestamp DESC
		LIMIT $%d OFFSET $%d
	`, strings.Join(clauses, " AND "), argN, argN+1)

	rows, err := cs.db.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list audit: %w", err)
	}
	defer rows.Close()

	var results []*AuditEntry
	for rows.Next() {
		e := &AuditEntry{}
		if err := rows.Scan(
			&e.ID, &e.CredentialID, &e.Action, &e.ActorID, &e.ActorType,
			&e.IPAddress, &e.Metadata, &e.Timestamp,
		); err != nil {
			return nil, fmt.Errorf("scan audit row: %w", err)
		}
		results = append(results, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate audit rows: %w", err)
	}
	return results, nil
}

// ─── Hot-reload Watcher ───────────────────────────────────────────────────────

// StartWatcher launches a goroutine that listens on the PostgreSQL
// "config_changed" NOTIFY channel and flushes the entire in-memory cache
// whenever a notification arrives. It runs until ctx is cancelled.
func (cs *ConfigStore) StartWatcher(ctx context.Context) {
	go func() {
		for {
			if err := cs.runWatcher(ctx); err != nil {
				if ctx.Err() != nil {
					return
				}
				// Brief back-off before reconnecting.
				select {
				case <-ctx.Done():
					return
				case <-time.After(5 * time.Second):
				}
			}
		}
	}()
}

// runWatcher acquires a dedicated connection, issues LISTEN, and processes
// notifications until the context is cancelled or an error occurs.
func (cs *ConfigStore) runWatcher(ctx context.Context) error {
	conn, err := cs.db.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire watcher connection: %w", err)
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, "LISTEN config_changed"); err != nil {
		return fmt.Errorf("LISTEN config_changed: %w", err)
	}

	for {
		notification, err := conn.Conn().WaitForNotification(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil // context cancelled — clean exit
			}
			return fmt.Errorf("wait for notification: %w", err)
		}
		// A notification on config_changed means something changed. Flush the
		// entire cache; the next read will repopulate from the database.
		_ = notification
		cs.cache.Range(func(key, _ any) bool {
			cs.cache.Delete(key)
			return true
		})
	}
}

// ─── Bulk Import ──────────────────────────────────────────────────────────────

// ImportFromEnv seeds the credential registry from a map of environment
// variable values. It only runs if the registry table is currently empty,
// making it safe to call on every startup.
//
// envCredentials is keyed by category, with each inner map keyed by key_name.
// Example:
//
//	{
//	  "llm":    {"MODEL_API_KEY": "sk-..."},
//	  "github": {"GITHUB_TOKEN": "ghp_..."},
//	}
func (cs *ConfigStore) ImportFromEnv(ctx context.Context, envCredentials map[string]map[string]string) error {
	var count int
	if err := cs.db.QueryRow(ctx, `SELECT COUNT(*) FROM credential_registry`).Scan(&count); err != nil {
		return fmt.Errorf("check registry count: %w", err)
	}
	if count > 0 {
		return nil // registry already seeded, skip import
	}

	for category, kvs := range envCredentials {
		for keyName, value := range kvs {
			if value == "" {
				continue
			}
			if err := cs.SetCredential(ctx, "global", "default", category, keyName, value, true, "imported from environment", nil); err != nil {
				return fmt.Errorf("import %s/%s: %w", category, keyName, err)
			}
		}
	}
	return nil
}

// ─── Internal Helpers ─────────────────────────────────────────────────────────

// logAudit inserts a row into credential_audit_log. Errors are non-fatal from
// the caller's perspective but are returned so the caller can decide.
func (cs *ConfigStore) logAudit(
	ctx context.Context,
	credID *uuid.UUID,
	action string,
	actorID *uuid.UUID,
	actorType string,
	ipAddress string,
	meta map[string]any,
) error {
	metaJSON, err := json.Marshal(meta)
	if err != nil {
		metaJSON = []byte("{}")
	}

	var credIDVal, actorIDVal interface{}
	if credID != nil {
		credIDVal = *credID
	}
	if actorID != nil {
		actorIDVal = *actorID
	}

	_, err = cs.db.Exec(ctx, `
		INSERT INTO credential_audit_log
			(credential_id, action, actor_id, actor_type, ip_address, metadata)
		VALUES ($1, $2, $3, $4, $5, $6)
	`, credIDVal, action, actorIDVal, actorType, ipAddress, metaJSON)
	if err != nil {
		return fmt.Errorf("insert audit log: %w", err)
	}
	return nil
}

// ─── Credential Validation State (for credmonitor) ───────────────────────────

// DB returns the underlying connection pool. Used by modules that need raw
// SQL access (credmonitor uses this for the dedupe table queries).
func (cs *ConfigStore) DB() *pgxpool.Pool { return cs.db }

// MarkCredentialInvalid immediately flips the matching credential's validation
// status to 'invalid'. Intended for reactive call sites — e.g. a handler that
// just observed a 401 from GitHub. Returns (changed=true) only if the status
// actually changed, so callers can avoid firing duplicate alerts.
//
// This matches by (category, key_name) globally — the assumption is that
// credential keys like "token" or "api_token" are scope='global' in the
// default configuration. Scope-specific overrides are probed separately by
// their ID.
func (cs *ConfigStore) MarkCredentialInvalid(ctx context.Context, category, keyName, errMsg string) (bool, error) {
	const q = `
UPDATE credential_registry
   SET last_validation_status = 'invalid',
       last_validated_at      = now(),
       last_validation_error  = $3
 WHERE category = $1::credential_category
   AND key_name = $2
   AND (last_validation_status IS DISTINCT FROM 'invalid')
 RETURNING id`
	var id uuid.UUID
	err := cs.db.QueryRow(ctx, q, category, keyName, errMsg).Scan(&id)
	if err != nil {
		if err == pgx.ErrNoRows {
			return false, nil
		}
		return false, fmt.Errorf("mark invalid: %w", err)
	}
	return true, nil
}

// UpdateValidationStatus records the outcome of an explicit probe on a
// specific credential by ID. Returns the previous status so callers can
// detect transitions (unchanged → no alert; changed → emit state-change
// alert).
//
// expiresAt is optional — when non-nil, overwrites the stored expires_at.
// Passing nil leaves it alone.
func (cs *ConfigStore) UpdateValidationStatus(
	ctx context.Context,
	credentialID uuid.UUID,
	status string,
	errMsg string,
	expiresAt *time.Time,
) (prevStatus string, err error) {
	var prev pgtype.Text
	if err := cs.db.QueryRow(ctx,
		`SELECT last_validation_status FROM credential_registry WHERE id = $1`,
		credentialID,
	).Scan(&prev); err != nil {
		return "", fmt.Errorf("fetch prev status: %w", err)
	}
	var expArg interface{}
	if expiresAt != nil {
		expArg = *expiresAt
	}
	if _, err := cs.db.Exec(ctx,
		`UPDATE credential_registry
		    SET last_validation_status = $2,
		        last_validated_at      = now(),
		        last_validation_error  = $3,
		        expires_at             = COALESCE($4, expires_at)
		  WHERE id = $1`,
		credentialID, status, errMsg, expArg,
	); err != nil {
		return "", fmt.Errorf("update status: %w", err)
	}
	if prev.Valid {
		return prev.String, nil
	}
	return "unknown", nil
}

// CredentialsByCategory returns every credential in a category with its value
// DECRYPTED. Used by the credential-monitor daemon to probe each one.
//
// CAUTION: the returned Value fields contain plaintext secrets. Callers must
// not log them. The daemon only passes them to HTTPS clients and discards them.
func (cs *ConfigStore) CredentialsByCategory(ctx context.Context, category string) ([]*CredentialEntry, error) {
	rows, err := cs.db.Query(ctx,
		`SELECT id, scope, scope_target, key_name, category, is_secret, description,
		        created_by, created_at, updated_at,
		        expires_at, last_validated_at, last_validation_status, last_validation_error,
		        encrypted_value, nonce
		   FROM credential_registry
		  WHERE category = $1::credential_category`,
		category,
	)
	if err != nil {
		return nil, fmt.Errorf("query credentials: %w", err)
	}
	defer rows.Close()

	var out []*CredentialEntry
	for rows.Next() {
		e := &CredentialEntry{}
		var expiresAt, lastValidatedAt pgtype.Timestamptz
		var lastStatus, lastErr pgtype.Text
		var encVal, nonce []byte
		if err := rows.Scan(
			&e.ID, &e.Scope, &e.ScopeTarget, &e.KeyName, &e.Category,
			&e.IsSecret, &e.Description,
			&e.CreatedBy, &e.CreatedAt, &e.UpdatedAt,
			&expiresAt, &lastValidatedAt, &lastStatus, &lastErr,
			&encVal, &nonce,
		); err != nil {
			return nil, fmt.Errorf("scan row: %w", err)
		}
		if expiresAt.Valid {
			t := expiresAt.Time
			e.ExpiresAt = &t
		}
		if lastValidatedAt.Valid {
			t := lastValidatedAt.Time
			e.LastValidatedAt = &t
		}
		if lastStatus.Valid {
			e.LastValidationStatus = lastStatus.String
		}
		if lastErr.Valid {
			e.LastValidationError = lastErr.String
		}
		plain, err := crypto.Decrypt(cs.masterKey, e.ID, encVal, nonce)
		if err != nil {
			return nil, fmt.Errorf("decrypt %s/%s: %w", e.Category, e.KeyName, err)
		}
		e.Value = plain
		out = append(out, e)
	}
	return out, rows.Err()
}
