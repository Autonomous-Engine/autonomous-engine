package rollout

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/configsnap"
	"github.com/autonomousengine/autonomous-engine/internal/configstore"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// F2: a guarded credential write commits the credential, its
// credential_audit_log row and the guard's override row in ONE transaction
// under the advisory lock; a failing write rolls the audit rows back.

// guardTx records every statement (and the surface captures, as
// "-- capture") on one fake transaction.
type guardTx struct {
	pgx.Tx
	log       []string
	failOn    string
	existing  *uuid.UUID // credential row the SELECTs find
	commits   int
	rollbacks int
}

func (g *guardTx) Exec(_ context.Context, sql string, _ ...any) (pgconn.CommandTag, error) {
	g.log = append(g.log, strings.TrimSpace(sql))
	if g.failOn != "" && strings.Contains(sql, g.failOn) {
		return pgconn.CommandTag{}, errors.New("injected failure: " + g.failOn)
	}
	return pgconn.NewCommandTag("INSERT 0 1"), nil
}

func (g *guardTx) QueryRow(_ context.Context, sql string, _ ...any) pgx.Row {
	g.log = append(g.log, strings.TrimSpace(sql))
	return guardRow{g: g, sql: sql}
}

func (g *guardTx) Commit(context.Context) error   { g.commits++; return nil }
func (g *guardTx) Rollback(context.Context) error { g.rollbacks++; return nil }

type guardRow struct {
	g   *guardTx
	sql string
}

func (r guardRow) Scan(dest ...any) error {
	if strings.Contains(r.sql, "FROM credential_registry") && r.g.existing != nil {
		if len(dest) == 1 {
			*dest[0].(*uuid.UUID) = *r.g.existing
			return nil
		}
		for i, v := range []string{"global", "default", "model_name", "llm"} {
			*dest[i].(*string) = v
		}
		return nil
	}
	return pgx.ErrNoRows // no credential yet; no approved rollout
}

type guardBeginner struct{ tx *guardTx }

func (b guardBeginner) Begin(context.Context) (pgx.Tx, error) { return b.tx, nil }

// fakeSurface stands in for ProductionSurface: the default entry changes
// once the global-model overlay is in the context (the "after" capture).
func fakeSurface(t *testing.T) {
	t.Helper()
	prev := captureSurface
	captureSurface = func(ctx context.Context, q configsnap.Querier) (Surface, error) {
		q.(*guardTx).log = append(q.(*guardTx).log, "-- capture")
		if _, ok := ctx.Value(overlayKey{}).(GlobalModelOverlay); ok {
			return Surface{SurfaceKeyDefault: "cfg_after"}, nil
		}
		return Surface{SurfaceKeyDefault: "cfg_before"}, nil
	}
	t.Cleanup(func() { captureSurface = prev })
}

func credentialStore() *configstore.ConfigStore {
	return configstore.New(nil, []byte("0123456789abcdef0123456789abcdef"))
}

// index returns the position of the first log entry containing needle.
func index(t *testing.T, log []string, needle string) int {
	t.Helper()
	for i, s := range log {
		if strings.Contains(s, needle) {
			return i
		}
	}
	t.Fatalf("no %q in %q", needle, log)
	return -1
}

func contains(log []string, needle string) bool {
	for _, s := range log {
		if strings.Contains(s, needle) {
			return true
		}
	}
	return false
}

func credentialGuardChange(write func(ctx context.Context, tx pgx.Tx) error) DefaultChange {
	model := "other-model"
	return DefaultChange{
		Action: "credentials.create", Apply: write, Overlay: &GlobalModelOverlay{ModelID: &model},
		OverrideReason: "hotfix", Actor: "admin",
	}
}

func TestApplyGuarded_CredentialWriteCommitsWithTheAudit(t *testing.T) {
	cs := credentialStore()
	for _, tc := range []struct {
		name     string
		existing *uuid.UUID
		write    func(cs *configstore.ConfigStore) func(ctx context.Context, tx pgx.Tx) error
		stmt     string
	}{
		{"create", nil, func(cs *configstore.ConfigStore) func(ctx context.Context, tx pgx.Tx) error {
			return func(ctx context.Context, tx pgx.Tx) error {
				return cs.SetCredentialTx(ctx, tx, "global", "default", "llm", "model_name", "other-model", false, "", nil)
			}
		}, "INSERT INTO credential_registry"},
		{"rotate", func() *uuid.UUID { id := uuid.New(); return &id }(), func(cs *configstore.ConfigStore) func(ctx context.Context, tx pgx.Tx) error {
			return func(ctx context.Context, tx pgx.Tx) error {
				return cs.RotateCredentialTx(ctx, tx, uuid.New(), "other-model", nil)
			}
		}, "UPDATE credential_registry"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fakeSurface(t)
			tx := &guardTx{existing: tc.existing}
			res, err := ApplyGuarded(context.Background(), guardBeginner{tx}, credentialGuardChange(tc.write(cs)))
			if err != nil {
				t.Fatalf("ApplyGuarded: %v", err)
			}
			if !res.Allowed || res.Reason != GuardOverride {
				t.Fatalf("result %+v, want an audited override", res)
			}
			lock := index(t, tx.log, "pg_advisory_xact_lock")
			before := index(t, tx.log, "-- capture")
			write := index(t, tx.log, tc.stmt)
			credAudit := index(t, tx.log, "INSERT INTO credential_audit_log")
			notify := index(t, tx.log, "pg_notify('config_changed'")
			override := index(t, tx.log, "INSERT INTO rollout_overrides")
			after := before + 1 + index(t, tx.log[before+1:], "-- capture")
			if !(lock == 0 && lock < before && before < write && write < credAudit && credAudit < notify && notify < after && after < override) {
				t.Errorf("statement order: lock=%d before=%d write=%d credential_audit=%d notify=%d after=%d override=%d\n%s",
					lock, before, write, credAudit, notify, after, override, strings.Join(tx.log, "\n"))
			}
			if tx.commits != 1 {
				t.Errorf("commits = %d, want the one guard transaction committed", tx.commits)
			}
		})
	}
}

func TestApplyGuarded_FailedCredentialWriteRollsTheAuditBack(t *testing.T) {
	cs := credentialStore()
	for _, failOn := range []string{"INSERT INTO credential_registry", "INSERT INTO credential_audit_log", "pg_notify"} {
		t.Run(failOn, func(t *testing.T) {
			fakeSurface(t)
			tx := &guardTx{failOn: failOn}
			_, err := ApplyGuarded(context.Background(), guardBeginner{tx}, credentialGuardChange(func(ctx context.Context, tx pgx.Tx) error {
				return cs.SetCredentialTx(ctx, tx, "global", "default", "llm", "model_name", "other-model", false, "", nil)
			}))
			if err == nil {
				t.Fatal("ApplyGuarded succeeded although the credential write failed")
			}
			if IsRefused(err) {
				t.Fatalf("err = %v, want the write error", err)
			}
			if tx.commits != 0 || tx.rollbacks == 0 {
				t.Errorf("commits=%d rollbacks=%d, want the guard transaction rolled back", tx.commits, tx.rollbacks)
			}
			if contains(tx.log, "INSERT INTO rollout_overrides") {
				t.Error("the override audit row was written for a credential write that failed")
			}
			if failOn == "INSERT INTO credential_registry" && contains(tx.log, "INSERT INTO credential_audit_log") {
				t.Error("a credential audit row was written for a credential write that failed")
			}
		})
	}
}

// A refused change rolls the credential write back with it.
func TestApplyGuarded_RefusedCredentialWriteRollsBack(t *testing.T) {
	fakeSurface(t)
	cs := credentialStore()
	tx := &guardTx{}
	ch := credentialGuardChange(func(ctx context.Context, tx pgx.Tx) error {
		return cs.SetCredentialTx(ctx, tx, "global", "default", "llm", "model_name", "other-model", false, "", nil)
	})
	ch.OverrideReason = ""
	if _, err := ApplyGuarded(context.Background(), guardBeginner{tx}, ch); !IsRefused(err) {
		t.Fatalf("err = %v, want refused", err)
	}
	if tx.commits != 0 || tx.rollbacks == 0 || contains(tx.log, "INSERT INTO rollout_overrides") {
		t.Errorf("commits=%d rollbacks=%d", tx.commits, tx.rollbacks)
	}
}
