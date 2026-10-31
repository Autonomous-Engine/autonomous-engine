package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/codex"
	"github.com/autonomousengine/autonomous-engine/internal/models"
	"github.com/autonomousengine/autonomous-engine/internal/workgraph"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func codexPinParent(t *testing.T, f fixture, pool interface {
	Begin(context.Context) (pgx.Tx, error)
}, version int) {
	t.Helper()
	ctx := context.Background()
	if err := workgraph.WithTx(ctx, pool, func(tx pgx.Tx) error {
		return codex.NewPinStore(nil).PinTx(ctx, tx, codex.Pin{WorkItemID: f.parentWorkID, ProductID: f.productID, Branch: "main", ProductVersion: version, Modules: []string{"app"}})
	}); err != nil {
		t.Fatal(err)
	}
}

// Migration 060: the parent's codex pin reaches the subtask's own pin row,
// its work_items.codex_version and the implementation.tasks message, in the
// plan's transaction.
func TestIntegration_CodexPin_CopiedToSubtaskAndMessage(t *testing.T) {
	pool := integrationPool(t)
	f := newFixture(t, pool)
	codexPinParent(t, f, pool, 6)
	ctx := context.Background()

	pin, err := loadParentCodexPin(ctx, pool, f.parentWorkID)
	if err != nil || pin == nil || pin.ProductVersion != 6 {
		t.Fatalf("loadParentCodexPin = %+v, %v", pin, err)
	}
	taskID := uuid.New()
	msg := f.taskMessageFor(taskID)
	applyCodexPin(&msg, pin)
	item := f.subtaskFor(taskID)
	item.CodexVersion = plannedCodexVersion(pin, nil)
	plan := []plannedSubtask{{repoName: "m103-repo", steps: 1, item: item, msg: msg}}
	if err := commitDecomposition(ctx, pool, workgraph.New(pool), f.parentWorkID, nil, plan); err != nil {
		t.Fatalf("commitDecomposition: %v", err)
	}

	child, found, err := codex.NewPinStore(pool).Get(ctx, taskID)
	if err != nil || !found || child.ProductVersion != 6 || child.Branch != "main" || strings.Join(child.Modules, ",") != "app" {
		t.Fatalf("subtask codex pin = %+v found=%v err=%v", child, found, err)
	}
	var display int
	_ = pool.QueryRow(ctx, `SELECT codex_version FROM work_items WHERE id = $1`, taskID).Scan(&display)
	if display != 6 {
		t.Fatalf("subtask codex_version = %d, want 6", display)
	}
	var payload []byte
	if err := pool.QueryRow(ctx, `SELECT payload FROM outbox_events WHERE aggregate_id = $1 AND topic = 'implementation.tasks'`, f.parentWorkID).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	_ = json.Unmarshal(payload, &m)
	if m["codex_version"] != float64(6) || m["codex_branch"] != "main" {
		t.Fatalf("implementation.tasks payload = %s", payload)
	}
}

// recordVersions writes n codex versions for the fixture's product on main.
func recordVersions(t *testing.T, f fixture, pool *pgxpool.Pool, n int) {
	t.Helper()
	cx := codex.New(pool)
	for i := 0; i < n; i++ {
		if _, err := cx.RecordVersion(context.Background(), codex.NewVersion{ProductID: f.productID, Branch: "main", SectionsChanged: []string{codex.ModuleChange("app")}, Source: models.CodexChangeSourceAutomatedReanalysis, Trigger: codex.TriggerIncrementalIndex}); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM outbox_events WHERE aggregate_id = $1`, f.productID)
	})
}

// A plan is built on the codex current when planning starts, not the one
// the brief was researched on: the brief pinned v3, the codex is at v4, so
// after the plan commits the parent and its subtasks are pinned v4 with the
// brief's modules (the guarantee Part 1's re-plan relies on).
func TestIntegration_CodexPin_PlanRepinsToTheVersionPlannedAgainst(t *testing.T) {
	pool := integrationPool(t)
	f := newFixture(t, pool)
	recordVersions(t, f, pool, 4)
	codexPinParent(t, f, pool, 3)
	ctx := context.Background()

	pin, err := resolvePlanningCodexPin(ctx, codex.New(pool), pool, f.parentWorkID)
	if err != nil || pin == nil || pin.ProductVersion != 4 || strings.Join(pin.Modules, ",") != "app" {
		t.Fatalf("resolvePlanningCodexPin = %+v, %v; want v4 with the brief's modules", pin, err)
	}
	taskID := uuid.New()
	msg := f.taskMessageFor(taskID)
	applyCodexPin(&msg, pin)
	item := f.subtaskFor(taskID)
	item.CodexVersion = plannedCodexVersion(pin, nil)
	if err := commitDecomposition(ctx, pool, workgraph.New(pool), f.parentWorkID, nil, []plannedSubtask{{repoName: "m103-repo", steps: 1, item: item, msg: msg}}); err != nil {
		t.Fatalf("commitDecomposition: %v", err)
	}
	store := codex.NewPinStore(pool)
	for name, id := range map[string]uuid.UUID{"parent": f.parentWorkID, "subtask": taskID} {
		got, found, err := store.Get(ctx, id)
		if err != nil || !found || got.ProductVersion != 4 || strings.Join(got.Modules, ",") != "app" {
			t.Errorf("%s pin = %+v, found=%v err=%v; want v4 app", name, got, found, err)
		}
		var display int
		_ = pool.QueryRow(ctx, `SELECT codex_version FROM work_items WHERE id = $1`, id).Scan(&display)
		if display != 4 {
			t.Errorf("%s codex_version = %d, want 4", name, display)
		}
	}
}

// The parent's pin row changed between resolving the planning pin and
// committing the plan (a second brief): nothing is rolled back. The pin row
// keeps its modules and branch and takes the version the plan was built on;
// subtasks and their messages carry that row's pin.
func TestIntegration_CodexPin_ChangedPinRowIsUsedNotRolledBack(t *testing.T) {
	pool := integrationPool(t)
	f := newFixture(t, pool)
	recordVersions(t, f, pool, 6)
	codexPinParent(t, f, pool, 6)
	ctx := context.Background()
	pin, err := resolvePlanningCodexPin(ctx, codex.New(pool), pool, f.parentWorkID)
	if err != nil || pin == nil || pin.ProductVersion != 6 {
		t.Fatal(pin, err)
	}
	// A second brief re-pins the parent with other modules.
	if err := workgraph.WithTx(ctx, pool, func(tx pgx.Tx) error {
		return codex.NewPinStore(nil).PinTx(ctx, tx, codex.Pin{WorkItemID: f.parentWorkID, ProductID: f.productID, Branch: "main", ProductVersion: 6, Modules: []string{"app", "other"}})
	}); err != nil {
		t.Fatal(err)
	}

	taskID := uuid.New()
	msg := f.taskMessageFor(taskID)
	applyCodexPin(&msg, pin)
	if err := commitDecomposition(ctx, pool, workgraph.New(pool), f.parentWorkID, nil, []plannedSubtask{{repoName: "m103-repo", steps: 1, item: f.subtaskFor(taskID), msg: msg}}); err != nil {
		t.Fatalf("commitDecomposition: %v (want the row's pin used, no rollback)", err)
	}
	child, found, err := codex.NewPinStore(pool).Get(ctx, taskID)
	if err != nil || !found || child.ProductVersion != 6 || strings.Join(child.Modules, ",") != "app,other" {
		t.Fatalf("subtask pin = %+v found=%v err=%v; want v6 app,other", child, found, err)
	}
	var payload []byte
	if err := pool.QueryRow(ctx, `SELECT payload FROM outbox_events WHERE aggregate_id = $1 AND topic = 'implementation.tasks'`, f.parentWorkID).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	_ = json.Unmarshal(payload, &m)
	if strings.Join(toStrings(m["codex_modules"]), ",") != "app,other" || m["codex_version"] != float64(6) {
		t.Fatalf("message pin = %s", payload)
	}

	// An unpinned parent plans unpinned.
	g := newFixture(t, pool)
	if pin, err := loadParentCodexPin(ctx, pool, g.parentWorkID); err != nil || pin != nil {
		t.Fatalf("unpinned parent: %+v, %v", pin, err)
	}
	other := uuid.New()
	if err := commitDecomposition(ctx, pool, workgraph.New(pool), g.parentWorkID, nil, []plannedSubtask{{repoName: "m103-repo", steps: 1, item: g.subtaskFor(other), msg: g.taskMessageFor(other)}}); err != nil {
		t.Fatalf("unpinned plan: %v", err)
	}
	if _, found, _ := codex.NewPinStore(pool).Get(ctx, other); found {
		t.Fatal("an unpinned parent produced a subtask codex pin")
	}
}
