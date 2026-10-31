package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/bus"
	"github.com/autonomousengine/autonomous-engine/internal/models"
	"github.com/autonomousengine/autonomous-engine/internal/outbox"
	"github.com/autonomousengine/autonomous-engine/internal/workgraph"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type fakeTx struct {
	pgx.Tx
	db *fakeBeginner
}

func (t *fakeTx) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	t.db.sqls = append(t.db.sqls, sql)
	t.db.args = append(t.db.args, args)
	if t.db.failAt == len(t.db.sqls) {
		return pgconn.CommandTag{}, errors.New("exec failed")
	}
	return pgconn.CommandTag{}, nil
}
func (t *fakeTx) Commit(context.Context) error   { t.db.commits++; return nil }
func (t *fakeTx) Rollback(context.Context) error { t.db.rollbacks++; return nil }

type fakeBeginner struct {
	failAt             int
	sqls               []string
	args               [][]any
	commits, rollbacks int
}

func (b *fakeBeginner) Begin(context.Context) (pgx.Tx, error) { return &fakeTx{db: b}, nil }

func sources(t *testing.T) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
			continue
		}
		b, err := os.ReadFile(filepath.Clean(n))
		if err != nil {
			t.Fatal(err)
		}
		out[n] = regexp.MustCompile(`(?m)//.*$`).ReplaceAllString(string(b), " ")
	}
	return out
}

// The portal's only event is intake, which accompanies the work item it
// creates: it must go through the outbox. Telemetry allowlist: empty.
func TestPortal_NeverPublishesDirectly(t *testing.T) {
	for name, code := range sources(t) {
		for _, needle := range []string{".Publish(", ".PublishMessage(", "publishJSON("} {
			if strings.Contains(code, needle) {
				t.Errorf("%s calls %s; intake must be enqueued with the work item", name, needle)
			}
		}
	}
}

func TestPortal_StartsOneDispatcherAndWaitsForIt(t *testing.T) {
	starts, waits := 0, 0
	for _, code := range sources(t) {
		starts += strings.Count(code, `outbox.StartForService(ctx, pool, kb, log.Default(), "portal")`)
		waits += strings.Count(code, "dispatcher.Wait()")
	}
	if starts != 1 || waits != 1 {
		t.Errorf("dispatcher starts=%d waits=%d, want 1 and 1", starts, waits)
	}
}

func TestCommitSubmission_WorkItemSubmissionAndIntakeShareOneTx(t *testing.T) {
	b := &fakeBeginner{}
	product := uuid.New()
	item := &models.WorkItem{ID: uuid.New(), Type: models.WorkItemTypeFeature, State: models.WorkItemStateIntake, Title: "t", ProductID: &product}
	req := submitRequest{Title: "t", Description: "d", Category: "feature", Priority: "high", ContactEmail: "a@b.c"}
	if err := commitSubmission(context.Background(), b, workgraph.New(nil), item, "TRK-1", req, product); err != nil {
		t.Fatalf("commitSubmission: %v", err)
	}
	if b.commits != 1 || len(b.sqls) != 3 {
		t.Fatalf("commits=%d statements=%d (%v), want 1 tx with work item, submission, outbox", b.commits, len(b.sqls), b.sqls)
	}
	if !strings.Contains(b.sqls[0], "INSERT INTO work_items") || !strings.Contains(b.sqls[1], "INSERT INTO portal_submissions") || !strings.Contains(b.sqls[2], "INSERT INTO outbox_events") {
		t.Errorf("statement order = %v", b.sqls)
	}
	ev := b.args[2]
	if ev[1] != outbox.AggregateTypeWorkItem || ev[2] != item.ID || ev[3] != bus.TopicIntake || ev[4] != item.ID.String() {
		t.Errorf("event = %v/%v %v/%v, want work_item/<item> intake/<item>", ev[1], ev[2], ev[3], ev[4])
	}
	var payload map[string]any
	if err := json.Unmarshal(ev[5].([]byte), &payload); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"work_id": item.ID.String(), "product_id": product.String(), "tracking_id": "TRK-1",
		"type": string(item.Type), "priority": string(item.Priority), "event_id": ev[0].(uuid.UUID).String(),
	}
	if len(payload) != len(want) {
		t.Errorf("payload = %v, want %v", payload, want)
	}
	for k, v := range want {
		if payload[k] != v {
			t.Errorf("payload %s = %v, want %v", k, payload[k], v)
		}
	}

	// The submission insert failing rolls back the work item and the event.
	b = &fakeBeginner{failAt: 2}
	if err := commitSubmission(context.Background(), b, workgraph.New(nil), item, "TRK-1", req, product); err == nil {
		t.Fatal("expected an error")
	}
	if b.commits != 0 || b.rollbacks != 1 || len(b.sqls) != 2 {
		t.Errorf("commits=%d rollbacks=%d statements=%d", b.commits, b.rollbacks, len(b.sqls))
	}
}
