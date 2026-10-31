package rollout_test

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/autonomousengine/autonomous-engine/internal/configsnap"
	"github.com/autonomousengine/autonomous-engine/internal/db"
	"github.com/autonomousengine/autonomous-engine/internal/outcomes"
	"github.com/autonomousengine/autonomous-engine/internal/rollout"
)

// Migration 055 against a real PostgreSQL (TEST_POSTGRES_DSN; run with -p 1:
// the guard reads the global agent_definitions table): the rollout
// lifecycle and its override path, the default-setting guard (a default
// change is refused without an approved rollout), shadow intake
// validation, production vs replay outcomes, attribution and the
// false-success job.

func pool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		dsn = os.Getenv("TEST_DATABASE_URL")
	}
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN / TEST_DATABASE_URL not set — skipping DB-integration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	p, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	if err := db.Migrate(ctx, p, filepath.Join("..", "..", "migrations")); err != nil {
		p.Close()
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(p.Close)
	return p
}

func mustExec(t *testing.T, p *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := p.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

func count(t *testing.T, p *pgxpool.Pool, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := p.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return n
}

type defs struct{ prod, cand uuid.UUID }

// seedDefinitions isolates the test's view of agent_definitions: every
// existing definition is disabled for the duration (restored afterwards),
// then a default production definition and a disabled candidate with its
// own model are created.
func seedDefinitions(t *testing.T, p *pgxpool.Pool) defs {
	t.Helper()
	ctx := context.Background()
	var enabled []uuid.UUID
	rows, err := p.Query(ctx, `SELECT id FROM agent_definitions WHERE enabled`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id uuid.UUID
		_ = rows.Scan(&id)
		enabled = append(enabled, id)
	}
	rows.Close()
	mustExec(t, p, `UPDATE agent_definitions SET enabled = false WHERE id = ANY($1)`, enabled)
	d := defs{uuid.New(), uuid.New()}
	suffix := d.prod.String()[:8]
	mustExec(t, p, `INSERT INTO agent_definitions (id, name, provider, model_id, enabled, is_default)
		VALUES ($1, $2, 'm406test', 'prod-model', true, true)`, d.prod, "m406-prod-"+suffix)
	mustExec(t, p, `INSERT INTO agent_definitions (id, name, provider, model_id, enabled, is_default)
		VALUES ($1, $2, 'm406test', 'cand-model', false, false)`, d.cand, "m406-cand-"+suffix)
	t.Cleanup(func() {
		_, _ = p.Exec(ctx, `DELETE FROM rollout_overrides WHERE actor = 'm406-test'`)
		_, _ = p.Exec(ctx, `DELETE FROM rollouts WHERE created_by = 'm406-test'`)
		_, _ = p.Exec(ctx, `DELETE FROM agent_definitions WHERE id = ANY($1)`, []uuid.UUID{d.prod, d.cand})
		_, _ = p.Exec(ctx, `UPDATE agent_definitions SET enabled = true WHERE id = ANY($1)`, enabled)
	})
	return d
}

// propose stores the production and candidate units and proposes a rollout.
func propose(t *testing.T, p *pgxpool.Pool, d defs) (*rollout.Rollout, string, string) {
	t.Helper()
	ctx := context.Background()
	snaps := configsnap.NewStore(p)
	baseID, baseDoc, err := rollout.ProductionSnapshot(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := snaps.GetOrCreate(ctx, baseDoc); err != nil {
		t.Fatal(err)
	}
	candID, candDoc, err := rollout.CaptureUnit(ctx, p, &d.cand)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := snaps.GetOrCreate(ctx, candDoc); err != nil {
		t.Fatal(err)
	}
	if candID == baseID {
		t.Fatal("candidate and production units must differ")
	}
	r, err := rollout.NewStore(p).Propose(ctx, rollout.ProposeInput{CandidateSnapshotID: candID, BaselineSnapshotID: baseID, CreatedBy: "m406-test"})
	if err != nil {
		t.Fatal(err)
	}
	return r, candID, baseID
}

func enableAndSetDefault(id uuid.UUID) func(context.Context, pgx.Tx) error {
	return func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE agent_definitions SET is_default = false`); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE agent_definitions SET enabled = true, is_default = true WHERE id = $1`, id)
		return err
	}
}

func setModel(id uuid.UUID, model string) func(context.Context, pgx.Tx) error {
	return func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE agent_definitions SET model_id = $2 WHERE id = $1`, id, model)
		return err
	}
}

func TestIntegration_DefaultChangeRefusedWithoutApproval(t *testing.T) {
	p := pool(t)
	ctx := context.Background()
	d := seedDefinitions(t, p)
	r, candID, _ := propose(t, p, d)
	isDefault := func(id uuid.UUID) bool {
		return count(t, p, `SELECT count(*) FROM agent_definitions WHERE id = $1 AND enabled AND is_default`, id) == 1
	}
	guard := func(action string, apply func(context.Context, pgx.Tx) error, reason string) (rollout.GuardResult, error) {
		return rollout.ApplyGuarded(ctx, p, rollout.DefaultChange{Action: action, Apply: apply, OverrideReason: reason, Actor: "m406-test"})
	}

	// 1. No approved rollout, no override: refused, rolled back, nothing audited.
	_, err := guard("agents.set_default", enableAndSetDefault(d.cand), "")
	var refused *rollout.RefusedError
	if !errors.As(err, &refused) || !rollout.IsRefused(err) {
		t.Fatalf("want a refusal, got %v", err)
	}
	for _, c := range refused.Unapproved {
		if c.After != candID {
			t.Errorf("unapproved change %s → %s, want the candidate unit %s (enabled/is_default must not change the id)", c.Key, c.After, candID)
		}
	}
	if len(refused.Unapproved) != 2 {
		t.Errorf("unapproved = %+v, want the default and the newly enabled definition", refused.Unapproved)
	}
	if isDefault(d.cand) || !isDefault(d.prod) {
		t.Error("a refused change must roll back")
	}
	if n := count(t, p, `SELECT count(*) FROM rollout_overrides WHERE actor = 'm406-test'`); n != 0 {
		t.Errorf("audit rows = %d, want 0", n)
	}

	// 2. Admin override with a reason: applied and audited per change in its transaction.
	res, err := guard("agents.set_default", enableAndSetDefault(d.cand), "incident hotfix")
	if err != nil || res.Reason != rollout.GuardOverride || !isDefault(d.cand) {
		t.Fatalf("override: %+v %v", res, err)
	}
	if n := count(t, p, `SELECT count(*) FROM rollout_overrides WHERE actor = 'm406-test' AND override_reason = 'incident hotfix' AND after_snapshot_id = $1`, candID); n != 2 {
		t.Errorf("override audit rows = %d, want 2", n)
	}
	// Back to production (override), then take the candidate out (not gated).
	if _, err := guard("agents.set_default", enableAndSetDefault(d.prod), "revert"); err != nil {
		t.Fatal(err)
	}
	if res, err := guard("agents.update", func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE agent_definitions SET enabled = false WHERE id = $1`, d.cand)
		return err
	}, ""); err != nil || res.Reason != rollout.GuardUnchanged {
		t.Fatalf("disabling a non-default candidate: %+v %v", res, err)
	}

	// 3. Approve the rollout, then move production away from its baseline:
	// the approval is stale for the default entry.
	store := rollout.NewStore(p)
	if _, err := store.RecordDecision(ctx, r.ID, rollout.Decision{Verdict: rollout.VerdictApproved, Summary: "test"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Approve(ctx, r.ID, "m406-test", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := guard("agents.update", setModel(d.prod, "prod-model-2"), "unrelated hotfix"); err != nil {
		t.Fatal(err)
	}
	_, err = guard("agents.set_default", enableAndSetDefault(d.cand), "")
	if !errors.As(err, &refused) {
		t.Fatalf("stale approval: want a refusal, got %v", err)
	}
	staleSeen := false
	for _, c := range refused.Unapproved {
		if c.Key == rollout.SurfaceKeyDefault && c.Stale {
			staleSeen = true
		}
	}
	if !staleSeen || !strings.Contains(err.Error(), "stale") {
		t.Errorf("the default entry must be refused as stale: %v", err)
	}

	// 4. Production back at the baseline: the approved rollout allows the change.
	if _, err := guard("agents.update", setModel(d.prod, "prod-model"), "undo hotfix"); err != nil {
		t.Fatal(err)
	}
	res, err = guard("agents.set_default", enableAndSetDefault(d.cand), "")
	if err != nil || res.Reason != rollout.GuardApproved || !isDefault(d.cand) {
		t.Fatalf("approved: %+v %v", res, err)
	}
	for _, c := range res.Changes {
		if c.RolloutID == nil || *c.RolloutID != r.ID {
			t.Errorf("change %s not tied to the approved rollout", c.Key)
		}
	}

	// 5. A credential write that sets the global model: an enabled
	// definition without its own model changes with it (overlay), so it is
	// refused; credentials that implementation-agent does not resolve pass.
	nomodel := uuid.New()
	mustExec(t, p, `INSERT INTO agent_definitions (id, name, enabled) VALUES ($1, $2, false)`, nomodel, "m406-nomodel-"+nomodel.String()[:8])
	t.Cleanup(func() { _, _ = p.Exec(ctx, `DELETE FROM agent_definitions WHERE id = $1`, nomodel) })
	if _, err := guard("agents.update", func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE agent_definitions SET enabled = true WHERE id = $1`, nomodel)
		return err
	}, "enable a global-model definition"); err != nil {
		t.Fatal(err)
	}
	shapes, err := rollout.CredentialShapesUnit(ctx, p, "global", "default", "llm", "model_name")
	if err != nil || !shapes {
		t.Fatalf("global/default model_name must shape the unit: %t %v", shapes, err)
	}
	if shapes, _ := rollout.CredentialShapesUnit(ctx, p, "agent_type", "review-agent", "llm", "model_name"); shapes {
		t.Error("a review-agent credential does not shape the implementation-agent unit")
	}
	if shapes, _ := rollout.CredentialShapesUnit(ctx, p, "global", "default", "llm", "api_key"); shapes {
		t.Error("an API key is not part of a snapshot")
	}
	_, err = rollout.ApplyGuarded(ctx, p, rollout.DefaultChange{Action: "credentials.update",
		Overlay: rollout.OverlayForCredential("llm", "model_name", "some-other-model"), Actor: "m406-test"})
	if !errors.As(err, &refused) || len(refused.Unapproved) != 1 || refused.Unapproved[0].Key != "definition:"+nomodel.String() {
		t.Fatalf("global model credential: %v", err)
	}
}

func TestIntegration_RolloutLifecycleAndOverride(t *testing.T) {
	p := pool(t)
	ctx := context.Background()
	d := seedDefinitions(t, p)
	r, _, _ := propose(t, p, d)
	store := rollout.NewStore(p)

	if _, err := store.MarkShadow(ctx, r.ID, "shadow"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RecordDecision(ctx, r.ID, rollout.Decision{Verdict: rollout.VerdictMoreTrials, Summary: "inconclusive"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Approve(ctx, r.ID, "m406-test", ""); !errors.Is(err, rollout.ErrNotApprovable) {
		t.Fatalf("approve MORE_TRIALS without a reason: %v", err)
	}
	got, err := store.Approve(ctx, r.ID, "m406-test", "manual review of the replay outputs")
	if err != nil || got.Status != rollout.StatusApproved || got.DecidedBy == nil || *got.DecidedBy != "m406-test" {
		t.Fatalf("override approve: %+v %v", got, err)
	}
	if n := count(t, p, `SELECT count(*) FROM rollout_overrides WHERE rollout_id = $1 AND action = 'approve_rollout'
		AND details->>'verdict' = 'MORE_TRIALS'`, r.ID); n != 1 {
		t.Errorf("approve override audit rows = %d, want 1", n)
	}
	if _, err := store.Rollback(ctx, r.ID, "m406-test", "regression in production"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Approve(ctx, r.ID, "m406-test", "again"); !errors.Is(err, rollout.ErrBadTransition) {
		t.Fatalf("approve after rollback: %v", err)
	}

	// A BLOCKED evaluation blocks the rollout.
	r2, err := store.Propose(ctx, rollout.ProposeInput{CandidateSnapshotID: *r.CandidateSnapshotID, BaselineSnapshotID: *r.BaselineSnapshotID, CreatedBy: "m406-test"})
	if err != nil {
		t.Fatal(err)
	}
	got, err = store.RecordDecision(ctx, r2.ID, rollout.Decision{Verdict: rollout.VerdictBlocked, Summary: "sealed false success"})
	if err != nil || got.Status != rollout.StatusBlocked || got.Verdict() != rollout.VerdictBlocked {
		t.Fatalf("blocked decision: %+v %v", got, err)
	}
	if _, err := store.Approve(ctx, r2.ID, "m406-test", "override"); !errors.Is(err, rollout.ErrBadTransition) {
		t.Fatalf("a blocked rollout stays blocked: %v", err)
	}
	if ok, err := rollout.Approved(ctx, p, *r.CandidateSnapshotID, rollout.ScopeGlobal, nil); err != nil || ok != nil {
		t.Errorf("no approved rollout should remain: %+v %v", ok, err)
	}
	if _, err := store.Propose(ctx, rollout.ProposeInput{CandidateSnapshotID: "x", BaselineSnapshotID: "x"}); !errors.Is(err, rollout.ErrSameSnapshot) {
		t.Errorf("same snapshot: %v", err)
	}
}

func TestIntegration_ShadowIntakeOutcomesAndAttribution(t *testing.T) {
	p := pool(t)
	ctx := context.Background()
	d := seedDefinitions(t, p)
	r, candID, baseID := propose(t, p, d)

	source, scratch := uuid.New(), uuid.New()
	mustExec(t, p, `INSERT INTO products (id, name) VALUES ($1, $2), ($3, $4)`, source, "m406-src-"+source.String()[:8], scratch, "m406-scratch-"+scratch.String()[:8])
	orig, replayItem, prodItem := uuid.New(), uuid.New(), uuid.New()
	mustExec(t, p, `INSERT INTO work_items (id, type, state, title, product_id) VALUES ($1, 'feature', 'released', 'M4-06 original', $2)`, orig, source)
	t.Cleanup(func() {
		_, _ = p.Exec(ctx, `DELETE FROM work_items WHERE id = ANY($1)`, []uuid.UUID{replayItem, prodItem, orig})
		_, _ = p.Exec(ctx, `DELETE FROM products WHERE id = ANY($1)`, []uuid.UUID{source, scratch})
	})

	replayMeta := map[string]any{"replay": true, "replay_of": orig.String(), "config_snapshot_id": candID, "rollout_id": r.ID.String()}
	// Replay of a product that has not opted in: 403.
	_, err := rollout.ResolveShadowIntake(ctx, p, "rollout-replay", replayMeta, scratch)
	var ie *rollout.IntakeError
	if !errors.As(err, &ie) || ie.Status != http.StatusForbidden {
		t.Fatalf("replay without opt-in: %v", err)
	}
	mustExec(t, p, `UPDATE products SET allow_replay = true WHERE id = $1`, source)
	// Replay into the source product itself: 400.
	if _, err := rollout.ResolveShadowIntake(ctx, p, "rollout-replay", replayMeta, source); !errors.As(err, &ie) || ie.Code != "replay_needs_scratch_product" {
		t.Fatalf("replay into the source product: %v", err)
	}
	// Replay into a product that is not a benchmark scratch product: 400.
	if _, err := rollout.ResolveShadowIntake(ctx, p, "rollout-replay", replayMeta, scratch); !errors.As(err, &ie) || ie.Code != "replay_needs_scratch_product" {
		t.Fatalf("replay into a non-scratch product: %v", err)
	}
	mustExec(t, p, `UPDATE products SET benchmark_scratch = true WHERE id = $1`, scratch)
	s, err := rollout.ResolveShadowIntake(ctx, p, "rollout-replay", replayMeta, scratch)
	if err != nil || s.SnapshotID != candID || s.AgentDefinitionID == nil || *s.AgentDefinitionID != d.cand || !s.Replay {
		t.Fatalf("replay intake: %+v %v", s, err)
	}
	// A snapshot id needs the rollout, a snapshot of that rollout, a shadow
	// status and a scratch product — "source: benchmark" grants nothing.
	refusals := []struct {
		meta    map[string]any
		product uuid.UUID
		code    string
	}{
		{map[string]any{"config_snapshot_id": candID}, scratch, "snapshot_needs_rollout"},
		{map[string]any{"config_snapshot_id": candID, "rollout_id": r.ID.String()}, source, "snapshot_needs_scratch_product"},
		{map[string]any{"config_snapshot_id": "0000000000000000000000000000000000000000000000000000000000000000", "rollout_id": r.ID.String()}, scratch, "snapshot_not_in_rollout"},
	}
	for _, rc := range refusals {
		if _, err := rollout.ResolveShadowIntake(ctx, p, "benchmark", rc.meta, rc.product); !errors.As(err, &ie) || ie.Code != rc.code || ie.Status != http.StatusBadRequest {
			t.Errorf("%v on %s: %v, want %s", rc.meta, rc.product, err, rc.code)
		}
	}
	for _, snap := range []string{candID, baseID} {
		ok, err := rollout.ResolveShadowIntake(ctx, p, "anything", map[string]any{"config_snapshot_id": snap, "rollout_id": r.ID.String()}, scratch)
		if err != nil || ok.SnapshotID != snap || ok.RolloutID == nil {
			t.Errorf("benchmark shadow intake for %s: %+v %v", snap[:8], ok, err)
		}
	}
	// A plain intake is untouched.
	if s, err := rollout.ResolveShadowIntake(ctx, p, "benchmark", map[string]any{"benchmark": map[string]any{}}, source); err != nil || s != nil {
		t.Errorf("plain intake: %+v %v", s, err)
	}

	// The replay item is created through ApplyTx; a production item runs under the baseline.
	mustExec(t, p, `INSERT INTO work_items (id, type, state, title, product_id, config_snapshot_id) VALUES ($1, 'feature', 'review', 'M4-06 replay', $2, $3)`, replayItem, scratch, candID)
	tx, err := p.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ApplyTx(ctx, tx, replayItem); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if ok, err := rollout.IsShadowTerminal(ctx, p, replayItem); err != nil || !ok {
		t.Fatalf("IsShadowTerminal(replay): %t %v", ok, err)
	}
	mustExec(t, p, `INSERT INTO work_items (id, type, state, title, product_id, config_snapshot_id) VALUES ($1, 'feature', 'released', 'M4-06 prod', $2, $3)`, prodItem, source, baseID)
	merged := time.Now().Add(-48 * time.Hour)
	mustExec(t, p, `INSERT INTO work_item_outcomes (work_item_id, config_snapshot_id, release_decision, merged_at, reverted_at, review_defects)
		VALUES ($1, $2, 'release', $3, $4, '{"critical": 0, "major": 1, "minor": 2}')`, prodItem, baseID, merged, merged.Add(24*time.Hour))
	mustExec(t, p, `INSERT INTO work_item_outcomes (work_item_id, config_snapshot_id, release_decision) VALUES ($1, $2, 'release')`, replayItem, candID)

	// Nightly false-success job: the production item was verified and reverted a day later.
	if n, err := outcomes.MarkFalseSuccess(ctx, p); err != nil || n < 1 {
		t.Fatalf("MarkFalseSuccess: %d %v", n, err)
	}
	if n, err := outcomes.MarkFalseSuccess(ctx, p); err != nil || n != 0 {
		t.Errorf("second run marked %d (%v), want 0 (idempotent)", n, err)
	}

	base, err := rollout.ProductionOutcomes(ctx, p, baseID)
	if err != nil || base.Items != 1 || base.Released != 1 || base.FalseSuccess != 1 || base.DefectDataItems != 1 || base.ItemsWithMajorDefect != 1 {
		t.Fatalf("baseline outcomes: %+v %v", base, err)
	}
	cand, err := rollout.CandidateOutcomes(ctx, p, candID, r.ID)
	if err != nil || cand.Items != 1 || cand.Released != 1 || cand.Source != "replay" || cand.FalseSuccess != 0 {
		t.Fatalf("candidate outcomes: %+v %v", cand, err)
	}

	// Benchmark shadow items of the rollout feed the defect gates, split by
	// the side they requested (C1b), and stay out of production rollups.
	candShadow, baseShadow, rogue := uuid.New(), uuid.New(), uuid.New()
	t.Cleanup(func() {
		_, _ = p.Exec(ctx, `DELETE FROM work_items WHERE id = ANY($1)`, []uuid.UUID{candShadow, baseShadow, rogue})
	})
	for _, it := range []struct {
		id      uuid.UUID
		snap    string
		defects string
	}{{candShadow, candID, `{"critical": 0, "major": 2, "minor": 0}`}, {baseShadow, baseID, `{"critical": 0, "major": 0, "minor": 1}`}} {
		mustExec(t, p, `INSERT INTO work_items (id, type, state, title, product_id, config_snapshot_id, shadow_rollout_id, requested_config_snapshot_id)
			VALUES ($1, 'feature', 'review', 'M4-06 shadow', $2, $3, $4, $3)`, it.id, scratch, it.snap, r.ID)
		mustExec(t, p, `INSERT INTO work_item_outcomes (work_item_id, config_snapshot_id, release_decision, review_defects) VALUES ($1, $2, 'release', $3::jsonb)`, it.id, it.snap, it.defects)
	}
	cand, err = rollout.CandidateOutcomes(ctx, p, candID, r.ID)
	if err != nil || cand.Items != 2 || cand.DefectDataItems != 1 || cand.ItemsWithMajorDefect != 1 || cand.Source != "shadow+replay" {
		t.Fatalf("candidate outcomes with shadow: %+v %v", cand, err)
	}
	baseAll, err := rollout.BaselineOutcomes(ctx, p, baseID, r.ID)
	if err != nil || baseAll.Items != 2 || baseAll.DefectDataItems != 2 || baseAll.ItemsWithMajorDefect != 1 || baseAll.Source != "production+shadow" {
		t.Fatalf("baseline outcomes with shadow: %+v %v", baseAll, err)
	}
	if prod, _ := rollout.ProductionOutcomes(ctx, p, baseID); prod.Items != 1 {
		t.Errorf("a shadow item counted as production: %+v", prod)
	}
	// Shadow items on a scratch product open PRs normally; a requested
	// snapshot on a non-scratch product without an approval gets replay
	// semantics (no PR).
	if term, err := rollout.IsShadowTerminal(ctx, p, candShadow); err != nil || term {
		t.Errorf("scratch shadow item terminal=%t %v", term, err)
	}
	mustExec(t, p, `INSERT INTO work_items (id, type, state, title, product_id, requested_config_snapshot_id) VALUES ($1, 'feature', 'intake', 'M4-06 rogue', $2, $3)`, rogue, source, candID)
	if term, err := rollout.IsShadowTerminal(ctx, p, rogue); err != nil || !term {
		t.Errorf("non-scratch requested snapshot terminal=%t %v", term, err)
	}
	if prod, _ := rollout.ProductionOutcomes(ctx, p, candID); prod.Items != 0 {
		t.Errorf("a replay item counted as production: %+v", prod)
	}

	// Attribution: a pipeline snapshot of the candidate's definition and
	// the candidate unit itself are attributed; the baseline is not.
	pipeID, err := configsnap.NewStore(p).CaptureAndStore(ctx, configsnap.Input{AgentType: configsnap.PipelineAgentType, AgentDefinitionID: &d.cand})
	if err != nil {
		t.Fatal(err)
	}
	a, err := rollout.Attribute(ctx, p, candID, baseID, []rollout.RecordRef{
		{TaskID: "t1", Side: "candidate", SnapshotID: candID},
		{TaskID: "t2", Side: "candidate", SnapshotID: pipeID},
		{TaskID: "t3", Side: "candidate", SnapshotID: baseID},
		{TaskID: "t4", Side: "candidate", SnapshotID: ""},
		{TaskID: "t1", Side: "baseline", SnapshotID: baseID},
	})
	if err != nil {
		t.Fatal(err)
	}
	if a.CandidateRecords != 4 || a.CandidateAttributed != 2 || a.BaselineRecords != 1 || a.BaselineAttributed != 1 || len(a.Problems) != 2 {
		t.Errorf("attribution: %+v", a)
	}
}
