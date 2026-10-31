package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/db"
	"github.com/autonomousengine/autonomous-engine/internal/models"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Without a database the detail renders no pin and an empty event list, and
// the list entries carry no codex fields.
func TestCodexPinViews_NoDatabase(t *testing.T) {
	pin, events := loadCodexPin(context.Background(), nil, uuid.New())
	b, _ := json.Marshal(map[string]any{"codex_pin": pin, "codex_events": events})
	if string(b) != `{"codex_events":[],"codex_pin":null}` {
		t.Fatalf("got %s", b)
	}
	item := &models.WorkItem{ID: uuid.New(), Title: "t"}
	entries := withCodexPins(context.Background(), nil, []*models.WorkItem{item})
	b, _ = json.Marshal(entries)
	if strings.Contains(string(b), "codex_pin") || !strings.Contains(string(b), `"title":"t"`) {
		t.Fatalf("list entry = %s, want the work item's fields and no codex_pin", b)
	}
}

// Codex coherence Part 1: the list carries each item's pin and, when the
// codex changed since planning and it was not re-planned, the note; the
// detail carries the pin and every codex event (TEST_POSTGRES_DSN).
func TestIntegration_CodexPinViews(t *testing.T) {
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN not set — skipping DB-integration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := db.Migrate(ctx, pool, filepath.Join("..", "..", "migrations")); err != nil {
		t.Fatal(err)
	}
	product, replanned, noted, unpinned := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO products (id, name) VALUES ($1, $2)`, []any{product, "p1-dash-" + product.String()}},
		{`INSERT INTO work_items (id, type, state, title, product_id) VALUES ($1, 'feature', 'planning', 'replanned', $2), ($3, 'feature', 'verification', 'noted', $2), ($4, 'feature', 'intake', 'unpinned', $2)`, []any{replanned, product, noted, unpinned}},
		{`INSERT INTO work_item_codex_pins (work_item_id, product_id, branch, product_version, modules) VALUES ($1, $2, 'main', 3, '{auth,billing}'), ($3, $2, 'main', 2, '{auth}')`, []any{replanned, product, noted}},
		{`INSERT INTO work_item_codex_events (work_item_id, product_id, branch, kind, from_version, to_version, modules, reason) VALUES ($1, $2, 'main', 'replanned', 3, 5, '{billing}', 'codex_changed: modules billing (v3 → v5)')`, []any{replanned, product}},
		{`INSERT INTO work_item_codex_events (work_item_id, product_id, branch, kind, from_version, to_version, modules, reason) VALUES ($1, $2, 'main', 'noted', 2, 5, '{auth}', 'codex changed since planning: modules auth (v2 → v5)')`, []any{noted, product}},
	} {
		if _, err := pool.Exec(ctx, q.sql, q.args...); err != nil {
			t.Fatalf("%s: %v", q.sql, err)
		}
	}
	defer func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM work_items WHERE product_id = $1`, product)
		_, _ = pool.Exec(context.Background(), `DELETE FROM products WHERE id = $1`, product)
	}()

	var q rowQuerier = pool
	pin, events := loadCodexPin(ctx, q, replanned)
	if pin == nil || pin.ProductVersion != 3 || pin.Branch != "main" || strings.Join(pin.Modules, ",") != "auth,billing" {
		t.Fatalf("pin = %+v", pin)
	}
	if len(events) != 1 || events[0].Kind != "replanned" || strings.Join(events[0].Modules, ",") != "billing" || events[0].FromVersion != 3 || events[0].ToVersion != 5 {
		t.Fatalf("events = %+v", events)
	}

	items := []*models.WorkItem{{ID: replanned, Title: "replanned"}, {ID: noted, Title: "noted"}, {ID: unpinned, Title: "unpinned"}}
	b, _ := json.Marshal(withCodexPins(ctx, q, items))
	var got []map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if p, _ := got[0]["codex_pin"].(map[string]any); p == nil || p["product_version"] != float64(3) {
		t.Errorf("replanned entry = %v", got[0])
	}
	if _, ok := got[0]["codex_changed_since_planning"]; ok {
		t.Errorf("a re-planned item is not flagged: %v", got[0])
	}
	note, _ := got[1]["codex_changed_since_planning"].(map[string]any)
	if note == nil || note["to_version"] != float64(5) || !strings.Contains(note["reason"].(string), "modules auth") {
		t.Errorf("noted entry = %v", got[1])
	}
	if _, ok := got[2]["codex_pin"]; ok || got[2]["title"] != "unpinned" {
		t.Errorf("unpinned entry = %v", got[2])
	}
}
