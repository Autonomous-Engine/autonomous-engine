package codex

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func scheduleSweep(t *testing.T, pool pgxBeginner, ev IndexedEvent, debounce time.Duration) {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := ScheduleSweepTx(ctx, tx, ev, debounce); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

type pgxBeginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// Three codex.indexed events for one product and branch collapse into one
// pending sweep that is due a debounce after the LAST of them; nothing is
// due before that, and it is claimed once.
func TestScheduleSweepCollapsesBurstAndDebouncesFromTheLastEvent(t *testing.T) {
	pool := integrationPool(t)
	ctx := context.Background()
	pid := newProduct(t, pool, "main")
	for v, sec := range []string{"module:a", "module:b", "section:risk_zones"} {
		scheduleSweep(t, pool, IndexedEvent{ProductID: pid, Branch: "main", ProductVersion: v + 1, SectionsChanged: []string{sec}}, 10*time.Minute)
	}
	var n, events, latest int
	var sections []string
	var due time.Time
	if err := pool.QueryRow(ctx, `SELECT count(*) OVER (), event_count, latest_version, sections_changed, due_at FROM codex_replan_pending WHERE product_id = $1`, pid).
		Scan(&n, &events, &latest, &sections, &due); err != nil {
		t.Fatal(err)
	}
	if n != 1 || events != 3 || latest != 3 {
		t.Fatalf("rows=%d events=%d latest=%d, want 1/3/3", n, events, latest)
	}
	if want := []string{"module:a", "module:b", "section:risk_zones"}; !reflect.DeepEqual(sections, want) {
		t.Fatalf("sections = %v, want %v", sections, want)
	}
	if until := time.Until(due); until < 9*time.Minute || until > 11*time.Minute {
		t.Fatalf("due in %s, want ~10m", until)
	}
	if s, err := claimForPool(ctx, pool, pid); err != nil || s != nil {
		t.Fatalf("claim before due = %+v, %v; want nothing", s, err)
	}
	// The quiet window passes.
	if _, err := pool.Exec(ctx, `UPDATE codex_replan_pending SET due_at = now() - interval '1 second' WHERE product_id = $1`, pid); err != nil {
		t.Fatal(err)
	}
	s, err := claimForPool(ctx, pool, pid)
	if err != nil || s == nil {
		t.Fatalf("claim = %+v, %v", s, err)
	}
	if s.Branch != "main" || s.LatestVersion != 3 || s.EventCount != 3 {
		t.Fatalf("claimed %+v", s)
	}
	// Claimed: a second claimer (another replica) gets nothing until the
	// lease expires.
	if again, err := claimForPool(ctx, pool, pid); err != nil || again != nil {
		t.Fatalf("second claim = %+v, %v; want nothing", again, err)
	}
	if done, err := CompleteSweep(ctx, pool, *s); err != nil || !done {
		t.Fatalf("complete = %t, %v", done, err)
	}
	if c := countRows(t, pool, `SELECT count(*) FROM codex_replan_pending WHERE product_id = $1`, pid); c != 0 {
		t.Fatalf("%d pending rows left", c)
	}
}

// claimForPool claims pid's due sweep only: other packages' tests share the
// database.
func claimForPool(ctx context.Context, q Querier, pid uuid.UUID) (*PendingSweep, error) {
	return ClaimDueSweep(ctx, q, time.Minute, pid)
}

func countRows(t *testing.T, q Querier, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := q.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// An event that arrives while a sweep is being processed re-arms the
// debounce: completing the claimed sweep then removes nothing, and the row
// is swept again after the new quiet window.
func TestSweepClaimSurvivesANewEvent(t *testing.T) {
	pool := integrationPool(t)
	ctx := context.Background()
	pid := newProduct(t, pool, "main")
	scheduleSweep(t, pool, IndexedEvent{ProductID: pid, Branch: "main", ProductVersion: 1, SectionsChanged: []string{"module:a"}}, 0)
	s, err := claimForPool(ctx, pool, pid)
	if err != nil || s == nil {
		t.Fatalf("claim = %+v, %v", s, err)
	}
	scheduleSweep(t, pool, IndexedEvent{ProductID: pid, Branch: "main", ProductVersion: 2, SectionsChanged: []string{"module:b"}}, 10*time.Minute)
	if done, err := CompleteSweep(ctx, pool, *s); err != nil || done {
		t.Fatalf("complete after a new event = %t, %v; want false", done, err)
	}
	if c := countRows(t, pool, `SELECT count(*) FROM codex_replan_pending WHERE product_id = $1 AND claim_token IS NULL AND latest_version = 2`, pid); c != 1 {
		t.Fatalf("re-armed rows = %d, want 1", c)
	}
}

// A sweep claimed by a process that died is claimable again once its lease
// has expired (a restart loses nothing).
func TestSweepLeaseExpires(t *testing.T) {
	pool := integrationPool(t)
	ctx := context.Background()
	pid := newProduct(t, pool, "main")
	scheduleSweep(t, pool, IndexedEvent{ProductID: pid, Branch: "main", ProductVersion: 1}, 0)
	if s, err := claimForPool(ctx, pool, pid); err != nil || s == nil {
		t.Fatalf("claim = %+v, %v", s, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE codex_replan_pending SET due_at = now() - interval '1 second' WHERE product_id = $1`, pid); err != nil {
		t.Fatal(err)
	}
	if s, err := claimForPool(ctx, pool, pid); err != nil || s == nil {
		t.Fatalf("claim after the lease = %+v, %v", s, err)
	}
}

// Item events: the history counts automatic re-plans only, and the
// acknowledged version is the newest to_version of any event.
func TestItemEventsAndReplanHistory(t *testing.T) {
	pool := integrationPool(t)
	ctx := context.Background()
	pid := newProduct(t, pool, "main")
	item := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO work_items (id, type, state, title, description, product_id) VALUES ($1, 'feature', 'implementation', 'p', 'd', $2)`, item, pid); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM work_items WHERE id = $1`, item) })

	h, err := LoadReplanHistory(ctx, pool, item)
	if err != nil || h.AutomaticReplans != 0 || h.AcknowledgedVersion != 0 || !h.LastReplanAt.IsZero() {
		t.Fatalf("empty history = %+v, %v", h, err)
	}
	before, err := DBNow(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	rec := func(ev ItemEvent) ItemEvent {
		t.Helper()
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		out, err := RecordItemEventTx(ctx, tx, ev)
		if err != nil {
			_ = tx.Rollback(ctx)
			t.Fatal(err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		return out
	}
	base := ItemEvent{WorkItemID: item, ProductID: pid, Branch: "main", Modules: []string{"a"}, ItemState: "implementation"}
	e1 := base
	e1.Kind, e1.Automatic, e1.FromVersion, e1.ToVersion, e1.Reason = EventReplanned, true, 1, 3, "codex_changed: modules a (v1 → v3)"
	first := rec(e1)
	between, _ := DBNow(ctx, pool)
	e2 := base
	e2.Kind, e2.Automatic, e2.FromVersion, e2.ToVersion = EventReplanned, false, 3, 4
	second := rec(e2)
	e3 := base
	e3.Kind, e3.FromVersion, e3.ToVersion, e3.Cause = EventNoted, 4, 6, "refused"
	rec(e3)

	h, err = LoadReplanHistory(ctx, pool, item)
	if err != nil {
		t.Fatal(err)
	}
	if h.AutomaticReplans != 1 || h.AcknowledgedVersion != 6 || h.LastReplanAt.Before(before) {
		t.Fatalf("history = %+v (before %s)", h, before)
	}
	events, err := ListItemEvents(ctx, pool, item)
	if err != nil || len(events) != 3 || events[0].Kind != EventReplanned || events[0].Reason != e1.Reason || !reflect.DeepEqual(events[0].Modules, []string{"a"}) {
		t.Fatalf("events = %+v, %v", events, err)
	}

	if events[2].Cause != "refused" {
		t.Errorf("cause = %q, want refused", events[2].Cause)
	}

	// The planner's commit rule (review fix 4): a re-plan brief commits
	// only when it is the latest re-plan; an ordinary brief only when no
	// re-plan happened after it was produced.
	allowed := func(replanEvent uuid.UUID, briefAt time.Time) bool {
		t.Helper()
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx) //nolint:errcheck
		ok, _, err := ReplanBriefAllowedTx(ctx, tx, item, replanEvent, briefAt)
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	if allowed(first.ID, time.Time{}) {
		t.Error("an earlier re-plan's brief commits after a later re-plan")
	}
	if !allowed(second.ID, time.Time{}) {
		t.Error("the latest re-plan's brief does not commit")
	}
	if allowed(uuid.Nil, before) {
		t.Error("a brief produced before the re-plans commits (redelivered old brief)")
	}
	if allowed(uuid.Nil, between) {
		t.Error("a brief produced between the two re-plans commits")
	}
	after, _ := DBNow(ctx, pool)
	if !allowed(uuid.Nil, after) {
		t.Error("a brief produced after the last re-plan (fresh research) does not commit")
	}
	if !allowed(uuid.Nil, time.Time{}) {
		t.Error("a brief of unknown age is refused")
	}
}

// Review fix 6: the prompt lookup only returns escalations.
func TestItemEventByPromptIsEscalationsOnly(t *testing.T) {
	pool := integrationPool(t)
	ctx := context.Background()
	pid := newProduct(t, pool, "main")
	item, prompt := uuid.New(), uuid.New()
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO work_items (id, type, state, title, description, product_id) VALUES ($1, 'feature', 'implementation', 'p', 'd', $2)`, []any{item, pid}},
		{`INSERT INTO user_interactions (id, work_item_id, product_id, asking_agent, trigger, prompt_type, headline, question, state) VALUES ($3, $1, $2, 'x', 'codex:replan', 'escalation', 'h', 'q', 'answered')`, []any{item, pid, prompt}},
		{`INSERT INTO work_item_codex_events (work_item_id, product_id, branch, kind, from_version, to_version, prompt_id) VALUES ($1, $2, 'main', 'continued', 1, 2, $3)`, []any{item, pid, prompt}},
	} {
		if _, err := pool.Exec(ctx, q.sql, q.args...); err != nil {
			t.Fatalf("%s: %v", q.sql, err)
		}
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM user_interactions WHERE id = $1`, prompt)
		_, _ = pool.Exec(context.Background(), `DELETE FROM work_items WHERE id = $1`, item)
	})
	if _, found, err := ItemEventByPrompt(ctx, pool, prompt); err != nil || found {
		t.Fatalf("a continued event was returned for the prompt (found=%t, %v)", found, err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO work_item_codex_events (work_item_id, product_id, branch, kind, from_version, to_version, prompt_id, created_at) VALUES ($1, $2, 'main', 'escalated', 1, 2, $3, now() - interval '1 minute')`, item, pid, prompt); err != nil {
		t.Fatal(err)
	}
	if ev, found, err := ItemEventByPrompt(ctx, pool, prompt); err != nil || !found || ev.Kind != EventEscalated {
		t.Fatalf("escalation = %+v found=%t %v", ev, found, err)
	}
}

// Review fix 6: two claimers at once never get the same row (SKIP LOCKED);
// a row held by one is skipped, not waited for.
func TestClaimDueSweepSkipsLockedRows(t *testing.T) {
	pool := integrationPool(t)
	ctx := context.Background()
	pid := newProduct(t, pool, "main")
	scheduleSweep(t, pool, IndexedEvent{ProductID: pid, Branch: "main", ProductVersion: 1}, 0)
	scheduleSweep(t, pool, IndexedEvent{ProductID: pid, Branch: "release", ProductVersion: 1}, 0)

	// A row locked by another transaction is skipped.
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `SELECT 1 FROM codex_replan_pending WHERE product_id = $1 AND branch = 'main' FOR UPDATE`, pid); err != nil {
		t.Fatal(err)
	}
	done := make(chan *PendingSweep, 1)
	go func() {
		s, _ := ClaimDueSweep(ctx, pool, time.Minute, pid)
		done <- s
	}()
	select {
	case s := <-done:
		if s == nil || s.Branch != "release" {
			t.Fatalf("claim while main is locked = %+v, want release", s)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("claim waited for a locked row instead of skipping it")
	}
	_ = tx.Rollback(ctx)

	// Two concurrent claimers on the same due rows get different rows.
	if _, err := pool.Exec(ctx, `UPDATE codex_replan_pending SET due_at = now() - interval '1 second', claim_token = NULL WHERE product_id = $1`, pid); err != nil {
		t.Fatal(err)
	}
	results := make(chan *PendingSweep, 2)
	for i := 0; i < 2; i++ {
		go func() {
			s, _ := ClaimDueSweep(ctx, pool, time.Minute, pid)
			results <- s
		}()
	}
	a, b := <-results, <-results
	if a == nil || b == nil || a.Branch == b.Branch {
		t.Fatalf("concurrent claims = %+v / %+v, want one row each", a, b)
	}
	if again, _ := ClaimDueSweep(ctx, pool, time.Minute, pid); again != nil {
		t.Fatalf("third claim = %+v, want nothing (both leased)", again)
	}
}
