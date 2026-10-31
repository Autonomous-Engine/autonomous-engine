package configstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/autonomousengine/autonomous-engine/internal/crypto"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// F2: transactional credential writes for callers that must commit the
// credential with other rows (the M4-06 configuration guard commits the
// write, its credential_audit_log row and the guard's own audit row in one
// transaction under its advisory lock). The value is encrypted in Go before
// anything is written; every statement runs on the caller's transaction,
// including the audit row and pg_notify (delivered only if it commits).
// Unlike the plain SetCredential/RotateCredential, an audit or notify
// failure is returned: in a transaction it has aborted the write anyway, and
// the caller rolls everything back.
//
// The process-local cache entry is dropped immediately, and the caller drops
// it again after the commit (InvalidateCached); the commit's config_changed
// notification flushes every other process's cache. A rollback leaves the
// old value, which a re-read simply caches again.

// SetCredentialTx is SetCredential on tx.
func (cs *ConfigStore) SetCredentialTx(
	ctx context.Context,
	tx pgx.Tx,
	scope, scopeTarget, category, keyName, value string,
	isSecret bool,
	description string,
	actorID *uuid.UUID,
) error {
	if tx == nil {
		return errors.New("configstore: set credential: nil transaction")
	}
	var existingID uuid.UUID
	existErr := tx.QueryRow(ctx, `
		SELECT id FROM credential_registry
		WHERE scope = $1 AND scope_target = $2 AND key_name = $3 AND category = $4
	`, scope, scopeTarget, keyName, category).Scan(&existingID)
	isUpdate := existErr == nil
	if existErr != nil && !errors.Is(existErr, pgx.ErrNoRows) {
		return fmt.Errorf("check existing credential: %w", existErr)
	}
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
		_, err = tx.Exec(ctx, `
			UPDATE credential_registry
			SET encrypted_value = $1, nonce = $2, is_secret = $3, description = $4,
			    updated_at = NOW()
			WHERE id = $5
		`, ciphertext, nonce, isSecret, description, credID)
	} else {
		// No ON CONFLICT here: the plain path's upsert would keep the
		// conflicting row's id while the value was encrypted under credID,
		// and in a transaction a concurrent insert of the same key is a
		// unique violation the caller reports (nothing is written).
		_, err = tx.Exec(ctx, `
			INSERT INTO credential_registry
				(id, scope, scope_target, key_name, encrypted_value, nonce, category, is_secret, description, created_by)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		`, credID, scope, scopeTarget, keyName, ciphertext, nonce, category, isSecret, description, actorIDVal)
	}
	if err != nil {
		return fmt.Errorf("upsert credential: %w", err)
	}

	action := "created"
	if isUpdate {
		action = "updated"
	}
	if err := logAuditTx(ctx, tx, &credID, action, actorID, map[string]any{
		"key_name": keyName,
		"category": category,
		"scope":    scope,
		"target":   scopeTarget,
	}); err != nil {
		return err
	}
	return cs.notifyTx(ctx, tx, scope, scopeTarget, keyName, category)
}

// ErrCredentialNotFound is returned (wrapped) by RotateCredentialTx for an
// unknown id and by ResolveCredential when no scope in the chain has the key.
var ErrCredentialNotFound = errors.New("credential not found")

// RotateCredentialTx is RotateCredential on tx.
func (cs *ConfigStore) RotateCredentialTx(ctx context.Context, tx pgx.Tx, id uuid.UUID, newValue string, actorID *uuid.UUID) error {
	if tx == nil {
		return errors.New("configstore: rotate credential: nil transaction")
	}
	var scope, scopeTarget, keyName, category string
	err := tx.QueryRow(ctx, `
		SELECT scope, scope_target, key_name, category
		FROM credential_registry WHERE id = $1
		FOR UPDATE
	`, id).Scan(&scope, &scopeTarget, &keyName, &category)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("credential %s: %w", id, ErrCredentialNotFound)
		}
		return fmt.Errorf("fetch credential for rotation: %w", err)
	}

	ciphertext, nonce, err := crypto.Encrypt(cs.masterKey, id, newValue)
	if err != nil {
		return fmt.Errorf("encrypt new value: %w", err)
	}

	if _, err := tx.Exec(ctx, `
		UPDATE credential_registry
		SET encrypted_value = $1, nonce = $2, updated_at = NOW()
		WHERE id = $3
	`, ciphertext, nonce, id); err != nil {
		return fmt.Errorf("update rotated credential: %w", err)
	}
	if err := logAuditTx(ctx, tx, &id, "rotated", actorID, map[string]any{
		"key_name": keyName,
		"category": category,
	}); err != nil {
		return err
	}
	return cs.notifyTx(ctx, tx, scope, scopeTarget, keyName, category)
}

// logAuditTx is logAudit on tx for a credential write (no IP address).
func logAuditTx(ctx context.Context, tx pgx.Tx, credID *uuid.UUID, action string, actorID *uuid.UUID, meta map[string]any) error {
	metaJSON, err := json.Marshal(meta)
	if err != nil {
		metaJSON = []byte("{}")
	}
	var credIDVal, actorIDVal interface{}
	if credID != nil {
		credIDVal = *credID
	}
	actorType := "system"
	if actorID != nil {
		actorIDVal = *actorID
		actorType = "user"
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO credential_audit_log
			(credential_id, action, actor_id, actor_type, ip_address, metadata)
		VALUES ($1, $2, $3, $4, $5, $6)
	`, credIDVal, action, actorIDVal, actorType, "", metaJSON); err != nil {
		return fmt.Errorf("insert audit log: %w", err)
	}
	return nil
}

// notifyTx queues config_changed on tx (sent on commit) and drops the local
// cache entry.
func (cs *ConfigStore) notifyTx(ctx context.Context, tx pgx.Tx, scope, scopeTarget, keyName, category string) error {
	if _, err := tx.Exec(ctx, "SELECT pg_notify('config_changed', $1)", keyName); err != nil {
		return fmt.Errorf("notify config_changed: %w", err)
	}
	cs.cache.Delete(cacheKey(scope, scopeTarget, keyName, category))
	return nil
}

// InvalidateCached drops the process-local cache entry for one credential.
// A caller that committed a *Tx write calls it after the commit: a read
// between the drop inside the transaction and the commit caches the old
// value again, and the config_changed notification is lost while the LISTEN
// connection is down.
func (cs *ConfigStore) InvalidateCached(scope, scopeTarget, keyName, category string) {
	cs.cache.Delete(cacheKey(scope, scopeTarget, keyName, category))
}
