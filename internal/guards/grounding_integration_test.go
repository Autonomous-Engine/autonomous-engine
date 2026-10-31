package guards

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/bus"
	"github.com/autonomousengine/autonomous-engine/internal/codex"
	"github.com/autonomousengine/autonomous-engine/internal/db"
	"github.com/autonomousengine/autonomous-engine/internal/models"
	"github.com/autonomousengine/autonomous-engine/internal/workgraph"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Codex coherence Part 3 against a real PostgreSQL (TEST_POSTGRES_DSN, a
// disposable database: the tests migrate it).

func integrationPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN not set — skipping DB-integration test")
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

// groundingProduct inserts a product whose one enabled repository has
// defaultBranch as its base branch.
func groundingProduct(t *testing.T, pool *pgxpool.Pool, defaultBranch string) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	pid, rid := uuid.New(), uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO products (id, name) VALUES ($1, $2)`, pid, "p3-"+pid.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO repositories (id, product_id, name, github_url, base_branch, enabled) VALUES ($1, $2, 'r', $3, $4, true)`,
		rid, pid, "o/p3-"+rid.String(), defaultBranch); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = pool.Exec(ctx, `DELETE FROM outbox_events WHERE aggregate_id IN (SELECT id FROM work_items WHERE product_id = $1)`, pid)
		_, _ = pool.Exec(ctx, `DELETE FROM work_items WHERE product_id = $1`, pid)
		_, _ = pool.Exec(ctx, `DELETE FROM codex_modules WHERE product_id = $1`, pid)
		_, _ = pool.Exec(ctx, `DELETE FROM codex_patterns WHERE product_id = $1`, pid)
		_, _ = pool.Exec(ctx, `DELETE FROM codex_anti_patterns WHERE product_id = $1`, pid)
		_, _ = pool.Exec(ctx, `DELETE FROM repositories WHERE product_id = $1`, pid)
		_, _ = pool.Exec(ctx, `DELETE FROM products WHERE id = $1`, pid)
	})
	return pid
}

func addModule(t *testing.T, pool *pgxpool.Pool, pid uuid.UUID, branch, name string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO codex_modules (id, name, product_id, branch) VALUES ($1, $2, $3, $4)`,
		uuid.New(), name+"-"+uuid.NewString()[:8], pid, branch); err != nil {
		t.Fatal(err)
	}
}

func addPattern(t *testing.T, pool *pgxpool.Pool, pid uuid.UUID, branch string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO codex_patterns (id, name, product_id, branch) VALUES ($1, $2, $3, $4)`,
		uuid.New(), "pat-"+uuid.NewString()[:8], pid, branch); err != nil {
		t.Fatal(err)
	}
}

func contextModules(t *testing.T, pool *pgxpool.Pool, pid uuid.UUID, branch string) int {
	t.Helper()
	raw, err := codex.New(pool).BuildContextForIntentBranch(context.Background(), pid, branch)
	if err != nil {
		t.Fatalf("BuildContextForIntentBranch(%q): %v", branch, err)
	}
	var payload struct {
		Modules []json.RawMessage `json:"modules"`
	}
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		t.Fatal(err)
	}
	return len(payload.Modules)
}

// Patterns and anti-patterns alone are not grounding.
func TestIntegration_CheckCodexGrounded_PatternsOnlyIsNotGrounded(t *testing.T) {
	pool := integrationPool(t)
	pid := groundingProduct(t, pool, "main")
	addPattern(t, pool, pid, "main")
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO codex_anti_patterns (id, name, product_id, branch) VALUES ($1, $2, $3, 'main')`,
		uuid.New(), "anti-"+uuid.NewString()[:8], pid); err != nil {
		t.Fatal(err)
	}
	got := CheckCodexGrounded(context.Background(), pool, pid, "")
	if got.Valid {
		t.Fatal("a product with patterns and anti-patterns but no module must not be grounded")
	}
	if len(got.Missing) != 1 || got.Missing[0] != MissingCodexGrounding {
		t.Errorf("Missing = %v, want [%s]", got.Missing, MissingCodexGrounding)
	}
	addModule(t, pool, pid, "main", "m")
	if got := CheckCodexGrounded(context.Background(), pool, pid, ""); !got.Valid {
		t.Fatalf("one module grounds the product; got %q", got.Message)
	}
}

// The guard and the context builder use the same branch fallback: the
// product's resolved default branch, never the literal "main".
func TestIntegration_CheckCodexGrounded_SameFallbackAsContextBuilder(t *testing.T) {
	pool := integrationPool(t)
	ctx := context.Background()

	// Default branch master with modules: a feature branch with no codex of
	// its own falls back to master in both.
	master := groundingProduct(t, pool, "master")
	addModule(t, pool, master, "master", "core")
	for _, branch := range []string{"", "feature/x", "master"} {
		if got := CheckCodexGrounded(ctx, pool, master, branch); !got.Valid {
			t.Errorf("branch %q on a master-default product with master modules: guard refused: %s", branch, got.Message)
		}
	}
	for _, branch := range []string{"", "feature/x", "master"} {
		if n := contextModules(t, pool, master, branch); n != 1 {
			t.Errorf("builder for %q: %d modules, want 1 (master fallback)", branch, n)
		}
	}
	eff, n, err := codex.GroundingBranch(ctx, pool, master, "feature/x")
	if err != nil || eff != "master" || n != 1 {
		t.Errorf("GroundingBranch(feature/x) = %q, %d, %v; want master, 1, nil", eff, n, err)
	}

	// Default branch master, modules only on a literal "main": neither the
	// guard nor the builder may use them.
	stray := groundingProduct(t, pool, "master")
	addModule(t, pool, stray, "main", "stray")
	for _, branch := range []string{"", "feature/x"} {
		if got := CheckCodexGrounded(ctx, pool, stray, branch); got.Valid {
			t.Errorf("branch %q: guard passed on modules of the literal main branch of a master-default product", branch)
		}
	}
	for _, branch := range []string{"", "feature/x"} {
		if n := contextModules(t, pool, stray, branch); n != 0 {
			t.Errorf("builder for %q on stray main modules: %d modules, want 0", branch, n)
		}
	}

	// Modules on the requested branch itself are used as they are.
	own := groundingProduct(t, pool, "master")
	addModule(t, pool, own, "feature/y", "own")
	if got := CheckCodexGrounded(ctx, pool, own, "feature/y"); !got.Valid {
		t.Errorf("feature/y has its own module: %s", got.Message)
	}
	if n := contextModules(t, pool, own, "feature/y"); n != 1 {
		t.Errorf("builder for feature/y: %d modules, want 1", n)
	}
	if got := CheckCodexGrounded(ctx, pool, own, ""); got.Valid {
		t.Error("the default branch master has no module: the guard must refuse")
	}
}

// CheckCodexReady, the one codex check every stage runs: modules without a
// codex version (onboarding incomplete) and a version without modules are
// both codex refusals (Re-index), whichever stage asks.
func TestIntegration_CheckCodexReady_BothFailuresAreCodexRefusals(t *testing.T) {
	pool := integrationPool(t)
	ctx := context.Background()
	noVersion := groundingProduct(t, pool, "main")
	addModule(t, pool, noVersion, "main", "m")
	if got := CheckCodexReady(ctx, pool, noVersion, ""); got.Valid || !IsCodexRefusal(got) {
		t.Errorf("modules without a codex version: %+v, want a codex refusal", got)
	}
	noModule := groundingProduct(t, pool, "main")
	addPattern(t, pool, noModule, "main")
	if _, err := pool.Exec(ctx, `INSERT INTO codex_versions (product_id, branch, product_version, change_source) VALUES ($1, 'main', 1, 'onboarding')`, noModule); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM codex_versions WHERE product_id = $1`, noModule)
	})
	if got := CheckCodexReady(ctx, pool, noModule, ""); got.Valid || !IsCodexRefusal(got) {
		t.Errorf("a version without modules: %+v, want a codex refusal", got)
	}
	addModule(t, pool, noModule, "main", "m")
	if got := CheckCodexReady(ctx, pool, noModule, ""); !got.Valid {
		t.Errorf("version and module: %s", got.Message)
	}
	if IsCodexRefusal(ValidationResult{Missing: []string{"repository"}}) {
		t.Error("a repository failure is not a codex refusal")
	}
}

// The codex.indexed replay re-checks the Re-index request under the row
// lock: a request the sweep withdrew meanwhile is not replayed (the row is
// kept for the operator's next answer); Retry replays regardless.
func TestIntegration_ReplayReindexed_RequiresPendingRequest(t *testing.T) {
	pool := integrationPool(t)
	ctx := context.Background()
	pid := groundingProduct(t, pool, "master")
	item := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO work_items (id, type, state, title, description, product_id) VALUES ($1, 'feature', 'research', 't', 'd', $2)`, item, pid); err != nil {
		t.Fatal(err)
	}
	if err := RefuseUngrounded(ctx, pool, item, item, "research-coordinator", Refusal{
		ProductID: pid, Stage: "research-coordinator", Reason: "r", RerunState: models.WorkItemStateResearch,
		RerunTopic: bus.TopicPRDReady, RerunPayload: []byte(`{"work_id":"` + item.String() + `"}`),
	}); err != nil {
		t.Fatal(err)
	}
	replay := func(mode ReplayMode) bool {
		var ok bool
		if err := workgraph.WithTx(ctx, pool, func(tx pgx.Tx) error {
			var err error
			ok, err = ReplayRefusalTx(ctx, tx, item, "test", "r", mode)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return ok
	}
	if replay(ReplayReindexed) {
		t.Fatal("replayed on codex.indexed without a pending Re-index")
	}
	if _, ok, _ := LoadRefusal(ctx, pool, item); !ok {
		t.Fatal("the refusal must be kept")
	}
	if !replay(ReplayRetry) {
		t.Fatal("Retry must replay")
	}
}

// A product without a configured default branch is refused with the
// configuration problem, not checked against "main".
func TestIntegration_CheckCodexGrounded_NoDefaultBranch(t *testing.T) {
	pool := integrationPool(t)
	ctx := context.Background()
	pid := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO products (id, name) VALUES ($1, $2)`, pid, "p3-nobranch-"+pid.String()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM codex_modules WHERE product_id = $1`, pid)
		_, _ = pool.Exec(context.Background(), `DELETE FROM products WHERE id = $1`, pid)
	})
	addModule(t, pool, pid, "main", "m")
	if got := CheckCodexGrounded(ctx, pool, pid, ""); got.Valid {
		t.Fatal("no repository, so no default branch: the guard must refuse rather than read main")
	}
}

// RefuseUngroundedTx blocks the item, enqueues work.blocked with the
// codex_ungrounded kind, and records the re-run, in the caller's
// transaction. ReplayRefusalTx re-runs it from that record.
func TestIntegration_RefuseUngroundedTx_RecordsAndReplays(t *testing.T) {
	pool := integrationPool(t)
	ctx := context.Background()
	pid := groundingProduct(t, pool, "master")
	item := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO work_items (id, type, state, title, description, product_id) VALUES ($1, 'feature', 'research', 't', 'd', $2)`, item, pid); err != nil {
		t.Fatal(err)
	}
	check := CheckCodexGrounded(ctx, pool, pid, "")
	if check.Valid {
		t.Fatal("fixture must be ungrounded")
	}
	originalEvent := uuid.New()
	payload := []byte(`{"work_id":"` + item.String() + `","prd_content":"x","event_id":"` + originalEvent.String() + `"}`)
	ref := Refusal{
		ProductID: pid, Branch: "", Stage: "research-coordinator", Reason: check.Message,
		RerunState: models.WorkItemStateResearch,
		RerunTopic: bus.TopicPRDReady, RerunKey: item.String(), RerunPayload: payload,
	}
	if err := workgraph.WithTx(ctx, pool, func(tx pgx.Tx) error {
		return RefuseUngroundedTx(ctx, tx, item, item, "research-coordinator", ref)
	}); err != nil {
		t.Fatalf("RefuseUngroundedTx: %v", err)
	}

	var state string
	_ = pool.QueryRow(ctx, `SELECT state::text FROM work_items WHERE id = $1`, item).Scan(&state)
	if state != "blocked" {
		t.Fatalf("state = %s, want blocked", state)
	}
	var raw []byte
	if err := pool.QueryRow(ctx, `SELECT payload FROM outbox_events WHERE aggregate_id = $1 AND topic = $2`, item, bus.TopicWorkBlocked).Scan(&raw); err != nil {
		t.Fatalf("work.blocked not enqueued: %v", err)
	}
	var wb WorkBlockedPayload
	_ = json.Unmarshal(raw, &wb)
	if wb.Kind != BlockKindCodexUngrounded || wb.WorkID != item.String() {
		t.Errorf("work.blocked payload = %+v", wb)
	}
	r, ok, err := LoadRefusal(ctx, pool, item)
	if err != nil || !ok {
		t.Fatalf("LoadRefusal: ok=%v err=%v", ok, err)
	}
	if r.RerunState != "research" || r.RerunTopic != bus.TopicPRDReady || r.IndexBranch != "master" || r.Stage != "research-coordinator" {
		t.Errorf("refusal = %+v", r)
	}

	if err := MarkReindexRequested(ctx, pool, item, "operator"); err != nil {
		t.Fatalf("MarkReindexRequested: %v", err)
	}
	pending, err := PendingReindex(ctx, pool, pid)
	if err != nil || len(pending) != 1 || pending[0].WorkItemID != item {
		t.Fatalf("PendingReindex = %+v, %v", pending, err)
	}

	var replayed bool
	if err := workgraph.WithTx(ctx, pool, func(tx pgx.Tx) error {
		var rerr error
		replayed, rerr = ReplayRefusalTx(ctx, tx, item, "status-orchestrator", "codex re-indexed", ReplayReindexed)
		return rerr
	}); err != nil {
		t.Fatalf("ReplayRefusalTx: %v", err)
	}
	if !replayed {
		t.Fatal("ReplayRefusalTx did not replay a blocked item")
	}
	_ = pool.QueryRow(ctx, `SELECT state::text FROM work_items WHERE id = $1`, item).Scan(&state)
	if state != "research" {
		t.Errorf("state after replay = %s, want research", state)
	}
	var replay []byte
	if err := pool.QueryRow(ctx, `SELECT payload FROM outbox_events WHERE aggregate_id = $1 AND topic = $2`, item, bus.TopicPRDReady).Scan(&replay); err != nil {
		t.Fatalf("prd.ready not re-enqueued: %v", err)
	}
	var got, want map[string]any
	_ = json.Unmarshal(replay, &got)
	_ = json.Unmarshal(payload, &want)
	if got["work_id"] != want["work_id"] || got["prd_content"] != want["prd_content"] {
		t.Errorf("replayed payload = %s, want %s", replay, payload)
	}
	if got["event_id"] == originalEvent.String() || got["event_id"] == nil {
		t.Errorf("the replay must be a new event (event_id %v, original %s): the stage's ledger would drop it", got["event_id"], originalEvent)
	}
	if _, ok, _ := LoadRefusal(ctx, pool, item); ok {
		t.Error("the refusal row must be gone after the replay")
	}
}

// UnpostedReindex lists only refusals that still describe their item
// (blocked on the refusal's own block, or parked from it) and drops the
// others, so the poster never re-posts — or escalates — a stale one.
func TestIntegration_UnpostedReindex_DropsRefusalsThatNoLongerDescribeTheItem(t *testing.T) {
	pool := integrationPool(t)
	ctx := context.Background()
	pid := groundingProduct(t, pool, "master")
	check := CheckCodexGrounded(ctx, pool, pid, "")
	refuse := func() uuid.UUID {
		item := uuid.New()
		if _, err := pool.Exec(ctx, `INSERT INTO work_items (id, type, state, title, description, product_id) VALUES ($1, 'feature', 'research', 't', 'd', $2)`, item, pid); err != nil {
			t.Fatal(err)
		}
		if err := RefuseUngrounded(ctx, pool, item, item, "research-coordinator", Refusal{
			ProductID: pid, Stage: "research-coordinator", Reason: check.Message, RerunState: models.WorkItemStateResearch,
			RerunTopic: bus.TopicPRDReady, RerunKey: item.String(), RerunPayload: []byte(`{"work_id":"` + item.String() + `"}`),
		}); err != nil {
			t.Fatal(err)
		}
		if err := MarkReindexRequested(ctx, pool, item, "operator"); err != nil {
			t.Fatal(err)
		}
		return item
	}
	current, parked, retried, reblocked := refuse(), refuse(), refuse(), refuse()
	if _, err := pool.Exec(ctx, `UPDATE work_items SET state = 'awaiting_user', previous_state = 'blocked' WHERE id = $1`, parked); err != nil {
		t.Fatal(err)
	}
	wg := workgraph.New(pool)
	for _, id := range []uuid.UUID{retried, reblocked} {
		if err := wg.TransitionState(ctx, id, models.WorkItemStateResearch, "someone", "retried elsewhere"); err != nil {
			t.Fatal(err)
		}
	}
	if err := workgraph.WithTx(ctx, pool, func(tx pgx.Tx) error {
		return BlockWorkItemTx(ctx, tx, reblocked, reblocked, "research-coordinator", "No LLM provider configured.")
	}); err != nil {
		t.Fatal(err)
	}

	listed, err := UnpostedReindex(ctx, pool, 1000)
	if err != nil {
		t.Fatal(err)
	}
	got := map[uuid.UUID]bool{}
	for _, r := range listed {
		got[r.WorkItemID] = true
	}
	if !got[current] || !got[parked] {
		t.Errorf("current refusals must be listed: current %v parked %v", got[current], got[parked])
	}
	for name, id := range map[string]uuid.UUID{"retried": retried, "re-blocked": reblocked} {
		if got[id] {
			t.Errorf("%s: a stale refusal was listed", name)
		}
		if _, ok, _ := LoadRefusal(ctx, pool, id); ok {
			t.Errorf("%s: the stale refusal must be dropped", name)
		}
	}
}
