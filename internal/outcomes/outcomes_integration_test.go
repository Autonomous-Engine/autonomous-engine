package outcomes_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/configsnap"
	"github.com/autonomousengine/autonomous-engine/internal/controller"
	"github.com/autonomousengine/autonomous-engine/internal/db"
	"github.com/autonomousengine/autonomous-engine/internal/outcomes"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Migration 053 against a real PostgreSQL: snapshots round-trip through
// JSONB with a stable id; the outcome upsert, the price view and the revert
// heuristic behave as the spec says. Skips without TEST_POSTGRES_DSN /
// TEST_DATABASE_URL.

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

type fixture struct {
	product, parent, sub uuid.UUID
}

func seed(t *testing.T, p *pgxpool.Pool) fixture {
	t.Helper()
	ctx := context.Background()
	f := fixture{uuid.New(), uuid.New(), uuid.New()}
	mustExec(t, p, `INSERT INTO products (id, name) VALUES ($1, $2)`, f.product, "m405-test-"+f.product.String()[:8])
	mustExec(t, p, `INSERT INTO work_items (id, type, state, title, product_id) VALUES ($1, 'feature', 'review', 'M4-05 parent', $2)`, f.parent, f.product)
	mustExec(t, p, `INSERT INTO work_items (id, type, state, title, product_id, parent_id) VALUES ($1, 'subtask', 'passed', 'M4-05 sub', $2, $3)`, f.sub, f.product, f.parent)
	t.Cleanup(func() {
		_, _ = p.Exec(ctx, `DELETE FROM work_items WHERE id = ANY($1)`, []uuid.UUID{f.sub, f.parent})
		_, _ = p.Exec(ctx, `DELETE FROM products WHERE id = $1`, f.product)
		_, _ = p.Exec(ctx, `DELETE FROM model_prices WHERE provider = 'm405test'`)
	})
	return f
}

func mustExec(t *testing.T, p *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := p.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

func upsert(t *testing.T, p *pgxpool.Pool, root uuid.UUID, f outcomes.Facts) {
	t.Helper()
	ctx := context.Background()
	tx, err := p.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if err := outcomes.UpsertTx(ctx, tx, root, f); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

type row struct {
	snapshot, verified, reviewed, decision, qa, mergeSHA *string
	defects                                              []byte
	humanReview                                          bool
	humanEdits                                           *bool
	tokensIn, tokensOut                                  int64
	cost                                                 *float64
	reverted                                             *time.Time
	detection                                            *string
}

func read(t *testing.T, p *pgxpool.Pool, id uuid.UUID) row {
	t.Helper()
	var r row
	err := p.QueryRow(context.Background(), `SELECT config_snapshot_id, verified_head_sha, reviewed_head_sha, release_decision, qa_result, merge_sha,
		review_defects, human_review_required, human_edits_after_review, tokens_in, tokens_out, cost_usd::float8, reverted_at, revert_detection
		FROM work_item_outcomes WHERE work_item_id = $1`, id).Scan(&r.snapshot, &r.verified, &r.reviewed, &r.decision, &r.qa, &r.mergeSHA,
		&r.defects, &r.humanReview, &r.humanEdits, &r.tokensIn, &r.tokensOut, &r.cost, &r.reverted, &r.detection)
	if err != nil {
		t.Fatalf("read outcome: %v", err)
	}
	return r
}

func str(p *string) string {
	if p == nil {
		return "<nil>"
	}
	return *p
}

func TestIntegration_SnapshotRoundTripAndLinkage(t *testing.T) {
	p := pool(t)
	f := seed(t, p)
	ctx := context.Background()
	store := configsnap.NewStore(p)
	in := configsnap.Input{WorkItemID: f.parent, AgentType: "implementation-agent",
		Model: configsnap.Model{Provider: "m405test", ModelID: "model-a", Endpoint: "https://u:p@example.test/v1", Source: configsnap.SourceGlobalDefault}}
	id1, err := store.CaptureAndStore(ctx, in)
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	// A fresh store (no cache) captures the same id; the row is not rewritten.
	id2, err := configsnap.NewStore(p).CaptureAndStore(ctx, in)
	if err != nil || id2 != id1 {
		t.Fatalf("second capture = %s, %v; want %s", id2, err, id1)
	}
	s, err := store.Get(ctx, id1)
	if err != nil {
		t.Fatal(err)
	}
	// The stored JSONB re-canonicalises to the same hash.
	if again, _, _ := configsnap.Hash(s.Document); again != id1 {
		t.Fatalf("JSONB round trip changed the hash: %s != %s", again, id1)
	}
	if ep := s.Document["model"].(map[string]any)["endpoint"]; ep != "https://example.test/v1" {
		t.Fatalf("endpoint stored with credentials: %v", ep)
	}
	// A different model is a different snapshot; the diff names it.
	in.Model.ModelID = "model-b"
	id3, err := store.CaptureAndStore(ctx, in)
	if err != nil || id3 == id1 {
		t.Fatalf("changed model kept id %s (%v)", id3, err)
	}
	s3, _ := store.Get(ctx, id3)
	changes, _ := configsnap.Diff(s.Document, s3.Document)
	if len(changes) != 1 || changes[0].Path != "model.model_id" {
		t.Fatalf("diff = %+v", changes)
	}
	// FK linkage: work item, inference log.
	mustExec(t, p, `UPDATE work_items SET config_snapshot_id = $2 WHERE id = $1`, f.parent, id1)
	mustExec(t, p, `INSERT INTO inference_logs (model_provider, model_id, intent_type, work_item_id, role, agent_type, config_snapshot_id)
		VALUES ('m405test', 'model-a', 'tool_loop', $1, 'author', 'implementation-agent', $2)`, f.parent, id1)
	var n int
	if err := p.QueryRow(ctx, `SELECT COUNT(*) FROM inference_logs WHERE config_snapshot_id = $1 AND role = 'author'`, id1).Scan(&n); err != nil || n != 1 {
		t.Fatalf("tagged inference rows = %d, %v", n, err)
	}
	// The attempt's run records its snapshot (controller seam).
	runs := controller.NewStore(p)
	started, err := runs.StartRun(ctx, f.parent, f.sub.String())
	if err != nil {
		t.Fatal(err)
	}
	if err := runs.SetConfigSnapshot(ctx, started.ID, id3); err != nil {
		t.Fatal(err)
	}
	var runSnap string
	if err := p.QueryRow(ctx, `SELECT config_snapshot_id FROM implementation_runs WHERE id = $1`, started.ID).Scan(&runSnap); err != nil || runSnap != id3 {
		t.Fatalf("run snapshot = %q, %v", runSnap, err)
	}
	// The (unscheduled) prune helper never removes a referenced snapshot.
	if _, err := store.PruneUnreferenced(ctx, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	var stillThere bool
	if err := p.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM config_snapshots WHERE id = $1)`, id3).Scan(&stillThere); err != nil || !stillThere {
		t.Fatalf("prune removed a referenced snapshot: %v %v", stillThere, err)
	}
	if _, err := p.Exec(ctx, `INSERT INTO inference_logs (model_provider, intent_type, role) VALUES ('x', 'y', 'boss')`); err == nil {
		t.Fatal("an unknown role was accepted")
	}
	_, _ = p.Exec(ctx, `DELETE FROM inference_logs WHERE work_item_id = $1`, f.parent)
}

func TestIntegration_OutcomeUpsert(t *testing.T) {
	p := pool(t)
	f := seed(t, p)
	ctx := context.Background()

	// Two calls with tokens, no prices yet: cost NULL, tokens summed over
	// the parent and its subtask.
	mustExec(t, p, `INSERT INTO inference_logs (model_provider, model_id, intent_type, input_tokens, output_tokens, work_item_id, created_at)
		VALUES ('m405test', 'priced', 'x', 1000000, 500000, $1, NOW() - interval '1 hour'),
		       ('m405test/priced', '', 'tool_loop', 2000000, 0, $2, NOW() - interval '1 hour')`, f.parent, f.sub)
	t.Cleanup(func() {
		_, _ = p.Exec(ctx, `DELETE FROM inference_logs WHERE work_item_id = ANY($1)`, []uuid.UUID{f.parent, f.sub})
	})

	upsert(t, p, f.parent, outcomes.Facts{TaskID: f.sub.String(), Decision: outcomes.DecisionRelease})
	r := read(t, p, f.parent)
	if str(r.decision) != outcomes.DecisionRelease || r.tokensIn != 3000000 || r.tokensOut != 500000 || r.cost != nil {
		t.Fatalf("first upsert: decision=%s tokens=%d/%d cost=%v", str(r.decision), r.tokensIn, r.tokensOut, r.cost)
	}

	// A placeholder price row (NULL) keeps cost unknown; a real price
	// prices both rows, including the legacy "provider/model" one.
	mustExec(t, p, `INSERT INTO model_prices (provider, model_id) VALUES ('m405test', 'priced')`)
	upsert(t, p, f.parent, outcomes.Facts{})
	if r := read(t, p, f.parent); r.cost != nil {
		t.Fatalf("placeholder price produced cost %v", *r.cost)
	}
	mustExec(t, p, `UPDATE model_prices SET input_per_1m = 3, output_per_1m = 15 WHERE provider = 'm405test'`)

	// Review artifact with structured defects and a head; evidence;
	// human_review transition; a second subtask decision (worst wins).
	mustExec(t, p, `INSERT INTO work_item_artifacts (work_item_id, artifact_type, content)
		VALUES ($1, 'review_comments', '{"decision":"approved","head_sha":"aaa111","defects":{"critical":0,"major":1,"minor":2}}')`, f.parent)
	mustExec(t, p, `INSERT INTO work_item_transitions (work_item_id, from_state, to_state, reason) VALUES ($1, 'review', 'human_review', 'gate')`, f.parent)
	other := uuid.NewString()
	upsert(t, p, f.parent, outcomes.Facts{TaskID: other, Decision: outcomes.DecisionHumanReview})
	r = read(t, p, f.parent)
	wantCost := 3.0*3 + 0.5*15 // 3M input @ $3, 0.5M output @ $15
	if r.cost == nil || *r.cost != wantCost {
		t.Fatalf("cost = %v, want %v", r.cost, wantCost)
	}
	// Reviewed is not verified: without verified evidence the verified head
	// stays NULL even though the review recorded a head.
	if r.verified != nil || str(r.reviewed) != "aaa111" || !r.humanReview || str(r.decision) != outcomes.DecisionHumanReview || string(r.defects) == "" {
		t.Fatalf("after review: verified=%s reviewed=%s hr=%t decision=%s defects=%s", str(r.verified), str(r.reviewed), r.humanReview, str(r.decision), r.defects)
	}
	// Only verified evidence sets verified_head_sha; unverified evidence never does.
	mustExec(t, p, `INSERT INTO implementation_verifications (work_item_id, task_id, schema_version, head_sha, outcome, evidence, created_at)
		VALUES ($1, $2, 1, 'fff000', 'failed', '{}', NOW() - interval '2 minutes'),
		       ($1, $2, 1, 'eee555', 'verified', '{}', NOW() - interval '1 minute'),
		       ($1, $2, 1, 'ddd444', 'partially_verified', '{}', NOW())`, f.parent, f.sub.String())
	upsert(t, p, f.parent, outcomes.Facts{})
	if r := read(t, p, f.parent); str(r.verified) != "eee555" || str(r.reviewed) != "aaa111" {
		t.Fatalf("verified=%s reviewed=%s, want eee555/aaa111", str(r.verified), str(r.reviewed))
	}

	// QA fail then pass: latest wins; a factless upsert keeps it.
	upsert(t, p, f.parent, outcomes.Facts{QAResult: outcomes.QAFail})
	upsert(t, p, f.parent, outcomes.Facts{QAResult: outcomes.QAPass})
	upsert(t, p, f.parent, outcomes.Facts{})
	if r := read(t, p, f.parent); str(r.qa) != outcomes.QAPass {
		t.Fatalf("qa = %s", str(r.qa))
	}

	// Merge with a head that differs from the reviewed head: human edits.
	now := time.Now().UTC().Truncate(time.Second)
	upsert(t, p, f.parent, outcomes.Facts{Merge: &outcomes.Merge{PRNumber: 41, Repository: "acme/m405", HeadSHA: "bbb222", MergeSHA: "mmm333", MergedAt: &now}})
	r = read(t, p, f.parent)
	if r.humanEdits == nil || !*r.humanEdits || str(r.mergeSHA) != "mmm333" {
		t.Fatalf("merge: edits=%v merge=%s", r.humanEdits, str(r.mergeSHA))
	}

	// Revert heuristic: a later PR reverting #41 marks it, once.
	tx, err := p.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	n, err := outcomes.MarkRevertedTx(ctx, tx, "acme/m405", outcomes.RevertedPRNumbers(`Revert "M4-05 parent"`, "Reverts acme/m405#41", 42), now)
	if err != nil || n != 1 {
		t.Fatalf("mark reverted = %d, %v", n, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	r = read(t, p, f.parent)
	if r.reverted == nil || str(r.detection) != outcomes.RevertDetectionHeuristic {
		t.Fatalf("reverted = %v detection = %s", r.reverted, str(r.detection))
	}
	tx2, _ := p.Begin(ctx)
	if n, _ := outcomes.MarkRevertedTx(ctx, tx2, "acme/m405", []int{41}, now.Add(time.Hour)); n != 0 {
		t.Fatalf("second revert marked %d rows; first detection must win", n)
	}
	_ = tx2.Commit(ctx)

	// Outcome rows cascade with the work item.
	var exists bool
	mustExec(t, p, `DELETE FROM work_items WHERE id = ANY($1)`, []uuid.UUID{f.sub, f.parent})
	if err := p.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM work_item_outcomes WHERE work_item_id = $1)`, f.parent).Scan(&exists); err != nil || exists {
		t.Fatalf("outcome survived its work item: %v %v", exists, err)
	}
}
