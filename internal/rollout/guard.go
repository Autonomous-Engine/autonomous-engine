package rollout

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/autonomousengine/autonomous-engine/internal/configsnap"
)

// Guard results.
const (
	GuardUnchanged = "unchanged"
	GuardApproved  = "approved"
	GuardOverride  = "override"
)

// SurfaceKeyDefault is the production default's key in a Surface.
const SurfaceKeyDefault = "default"

// guardLockKey is the pg_advisory_xact_lock key that serialises every
// guarded configuration change: the before/after capture, the audit row
// and the write commit as one unit, with no other guarded change between.
const guardLockKey int64 = 0x4d34303601 // "M406" + 1

// Surface is the production configuration surface: the rollout unit of
// the default definition (SurfaceKeyDefault) and of every enabled
// definition ("definition:<id>"). Tag/complexity routing
// (agentregistry.ResolveForWorkItem) can send production work to any
// enabled definition, not only the default, so every enabled definition is
// production. A candidate is therefore created disabled; enabling it (or
// making it the default) is the rollout. `enabled` and `is_default` are not
// part of a snapshot, so doing either keeps the candidate's id.
type Surface map[string]string

// SurfaceChange is one entry of the surface that a change adds or alters.
type SurfaceChange struct {
	Key       string     `json:"key"`
	Before    string     `json:"before_snapshot_id"`
	After     string     `json:"after_snapshot_id"`
	RolloutID *uuid.UUID `json:"rollout_id,omitempty"`
	// Stale is set when an approved rollout exists for After but was
	// measured against a different baseline than the current default
	// (Before): its evidence does not cover this change.
	Stale bool `json:"stale,omitempty"`
}

// ProductionSurface captures the surface on q (which may be a transaction
// holding an uncommitted change).
func ProductionSurface(ctx context.Context, q configsnap.Querier) (Surface, error) {
	s := Surface{}
	def, err := DefaultDefinitionID(ctx, q)
	if err != nil {
		return nil, err
	}
	id, _, err := CaptureUnit(ctx, q, def)
	if err != nil {
		return nil, err
	}
	s[SurfaceKeyDefault] = id
	ids, err := enabledDefinitions(ctx, q)
	if err != nil {
		return nil, err
	}
	for _, defID := range ids {
		defID := defID
		id, _, err := CaptureUnit(ctx, q, &defID)
		if err != nil {
			return nil, err
		}
		s["definition:"+defID.String()] = id
	}
	return s, nil
}

func enabledDefinitions(ctx context.Context, q configsnap.Querier) ([]uuid.UUID, error) {
	rq, ok := q.(interface {
		Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	})
	if !ok {
		return nil, fmt.Errorf("rollout: surface needs a queryable database")
	}
	rows, err := rq.Query(ctx, `SELECT id FROM agent_definitions WHERE enabled ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("rollout: enabled definitions: %w", err)
	}
	defer rows.Close()
	var out []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// Changes lists the entries of after that are new or differ from s,
// sorted by key. Removals (a definition disabled or deleted) are not
// changes the guard gates: they take configurations out of production.
func (s Surface) Changes(after Surface) []SurfaceChange {
	var out []SurfaceChange
	for k, a := range after {
		if b := s[k]; b != a {
			out = append(out, SurfaceChange{Key: k, Before: b, After: a})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// GuardResult says why a configuration change was allowed.
type GuardResult struct {
	Allowed bool            `json:"allowed"`
	Reason  string          `json:"reason"`
	Changes []SurfaceChange `json:"changes"`
}

// RefusedError is ErrRolloutRequired with the unapproved changes.
type RefusedError struct {
	Action     string
	Unapproved []SurfaceChange
}

func (e *RefusedError) Error() string {
	parts := make([]string, 0, len(e.Unapproved))
	for _, c := range e.Unapproved {
		p := fmt.Sprintf("%s %s → %s", c.Key, orNone(c.Before), short(c.After))
		if c.Stale {
			p += " (an approved rollout exists for this snapshot but was measured against a different baseline: stale; re-evaluate against the current default)"
		}
		parts = append(parts, p)
	}
	return fmt.Sprintf("%v (action %s: %s)", ErrRolloutRequired, e.Action, strings.Join(parts, "; "))
}

// Unwrap lets errors.Is match ErrRolloutRequired.
func (e *RefusedError) Unwrap() error { return ErrRolloutRequired }

// IsRefused reports whether err is the guard's refusal.
func IsRefused(err error) bool { return errors.Is(err, ErrRolloutRequired) }

// DefaultChange describes one admin change that may alter the production
// configuration surface.
type DefaultChange struct {
	// Action names the change ("agents.set_default", "providers.update", …).
	Action string
	// Apply performs the real change on tx. It commits only when the guard
	// allows it, in the same transaction as any audit row.
	Apply func(ctx context.Context, tx pgx.Tx) error
	// Overlay, when set, is applied to the "after" capture as well as
	// Apply: the global-model effect of a credential write, whose
	// encrypted value the capture cannot read. Apply is then the
	// credential write itself (configstore SetCredentialTx /
	// RotateCredentialTx, F2), so the credential, its credential_audit_log
	// row and any override row commit together or not at all.
	Overlay *GlobalModelOverlay
	// OverrideReason, when non-empty, allows a change without an approved
	// rollout; each unapproved change is written to rollout_overrides with
	// Actor in the change's transaction.
	OverrideReason string
	Actor          string
	Details        map[string]any
}

// captureSurface is ProductionSurface; tests replace it to drive
// ApplyGuarded on a fake transaction.
var captureSurface = ProductionSurface

// Beginner starts a transaction (*pgxpool.Pool).
type Beginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// ApplyGuarded is the default-setting guard (D7 enforcement). In one
// transaction under pg_advisory_xact_lock it captures the production
// surface, applies the change, captures it again and decides: every entry
// the change adds or alters must be a snapshot with an approved global
// rollout — for the default entry, one measured against the current
// default (its baseline is Before) — or an override reason is required and
// audited in the same transaction. The change, the audit rows and nothing
// else commit together; a refusal rolls the change back and returns a
// *RefusedError (IsRefused). Product-scoped rollouts never satisfy it.
func ApplyGuarded(ctx context.Context, db Beginner, ch DefaultChange) (GuardResult, error) {
	var res GuardResult
	tx, err := db.Begin(ctx)
	if err != nil {
		return res, fmt.Errorf("rollout: guard begin: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, guardLockKey); err != nil {
		return res, fmt.Errorf("rollout: guard lock: %w", err)
	}
	before, err := captureSurface(ctx, tx)
	if err != nil {
		return res, fmt.Errorf("rollout: guard capture before: %w", err)
	}
	if ch.Apply != nil {
		if err := ch.Apply(ctx, tx); err != nil {
			return res, err
		}
	}
	afterCtx := ctx
	if ch.Overlay != nil {
		afterCtx = WithGlobalModelOverlay(ctx, *ch.Overlay)
	}
	after, err := captureSurface(afterCtx, tx)
	if err != nil {
		return res, fmt.Errorf("rollout: guard capture after: %w", err)
	}
	changes := before.Changes(after)
	res.Changes = changes
	var unapproved []SurfaceChange
	for i := range changes {
		c := &changes[i]
		baseline := ""
		if c.Key == SurfaceKeyDefault {
			baseline = c.Before
		}
		approved, stale, err := approvedFor(ctx, tx, c.After, baseline)
		if err != nil {
			return res, err
		}
		if approved != nil {
			id := approved.ID
			c.RolloutID = &id
			continue
		}
		c.Stale = stale
		unapproved = append(unapproved, *c)
	}
	res.Changes = changes
	switch {
	case len(changes) == 0:
		res.Reason = GuardUnchanged
	case len(unapproved) == 0:
		res.Reason = GuardApproved
	case strings.TrimSpace(ch.OverrideReason) == "":
		return res, &RefusedError{Action: ch.Action, Unapproved: unapproved}
	default:
		for _, c := range unapproved {
			details := map[string]any{"surface_key": c.Key}
			if c.Stale {
				details["stale_approval"] = true
			}
			for k, v := range ch.Details {
				details[k] = v
			}
			if err := RecordOverride(ctx, tx, Override{
				Action: ch.Action, Scope: ScopeGlobal, Before: c.Before, After: c.After,
				Reason: ch.OverrideReason, Actor: ch.Actor, Details: details,
			}); err != nil {
				return res, err
			}
		}
		res.Reason = GuardOverride
	}
	if err := tx.Commit(ctx); err != nil {
		return res, fmt.Errorf("rollout: guard commit: %w", err)
	}
	res.Allowed = true
	return res, nil
}

// approvedFor returns an approved global rollout for snapshotID. When
// baseline is set (the default entry), the rollout's baseline must equal
// it; stale reports that an approval exists but against another baseline.
func approvedFor(ctx context.Context, q DB, snapshotID, baseline string) (*Rollout, bool, error) {
	if baseline == "" {
		r, err := Approved(ctx, q, snapshotID, ScopeGlobal, nil)
		return r, false, err
	}
	row := q.QueryRow(ctx, `SELECT `+rolloutColumns+` FROM rollouts
		WHERE candidate_snapshot_id = $1 AND status = 'approved' AND scope = 'global' AND baseline_snapshot_id = $2
		ORDER BY decided_at DESC NULLS LAST LIMIT 1`, snapshotID, baseline)
	r, err := scanRollout(row)
	if err == nil {
		return r, false, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, false, fmt.Errorf("rollout: approved lookup: %w", err)
	}
	any, err := Approved(ctx, q, snapshotID, ScopeGlobal, nil)
	if err != nil {
		return nil, false, err
	}
	return nil, any != nil, nil
}

func orNone(id string) string {
	if id == "" {
		return "(new)"
	}
	return short(id)
}
