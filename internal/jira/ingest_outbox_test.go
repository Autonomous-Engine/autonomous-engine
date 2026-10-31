package jira

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

// The ingester's only event is intake for the work item it creates; it must
// be enqueued with that work item. Telemetry allowlist: empty.
func TestIngest_NeverPublishesDirectly(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	strip := regexp.MustCompile(`(?m)//.*$`)
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
			continue
		}
		b, err := os.ReadFile(filepath.Clean(n))
		if err != nil {
			t.Fatal(err)
		}
		code := strip.ReplaceAllString(string(b), " ")
		for _, needle := range []string{".Publish(", ".PublishMessage("} {
			if strings.Contains(code, needle) {
				t.Errorf("%s calls %s; intake must be enqueued in the outbox with the work item", n, needle)
			}
		}
	}
}

func TestCommitIngest_WorkItemIntakeAndTicketShareOneTx(t *testing.T) {
	b := &fakeBeginner{}
	product := uuid.New()
	item := &models.WorkItem{ID: uuid.New(), ExternalRef: "ABC-1", Type: models.WorkItemTypeTask, State: models.WorkItemStateIntake, ProductID: &product}
	if err := commitIngest(context.Background(), b, workgraph.New(nil), item, "ABC-1", product, "desc"); err != nil {
		t.Fatalf("commitIngest: %v", err)
	}
	if b.commits != 1 || len(b.sqls) != 3 {
		t.Fatalf("commits=%d statements=%v", b.commits, b.sqls)
	}
	if !strings.Contains(b.sqls[0], "INSERT INTO work_items") || !strings.Contains(b.sqls[1], "INSERT INTO outbox_events") || !strings.Contains(b.sqls[2], "INSERT INTO jira_ingested_tickets") {
		t.Errorf("statement order = %v", b.sqls)
	}
	ev := b.args[1]
	if ev[1] != outbox.AggregateTypeWorkItem || ev[2] != item.ID || ev[3] != bus.TopicIntake || ev[4] != item.ID.String() {
		t.Errorf("event = %v %v %v %v", ev[1], ev[2], ev[3], ev[4])
	}
	var payload map[string]any
	if err := json.Unmarshal(ev[5].([]byte), &payload); err != nil {
		t.Fatal(err)
	}
	if payload["work_id"] != item.ID.String() || payload["product_id"] != product.String() ||
		payload["description"] != "desc" || payload["source"] != "jira" || len(payload) != 6 {
		t.Errorf("payload = %v", payload)
	}
	if md, _ := payload["metadata"].(map[string]any); md["jira_key"] != "ABC-1" {
		t.Errorf("metadata = %v", payload["metadata"])
	}

	// Recording the ticket failing rolls back the work item and the event,
	// so the next poll retries the ticket instead of creating a duplicate.
	b = &fakeBeginner{failAt: 3}
	if err := commitIngest(context.Background(), b, workgraph.New(nil), item, "ABC-1", product, "desc"); err == nil {
		t.Fatal("expected an error")
	}
	if b.commits != 0 || b.rollbacks != 1 {
		t.Errorf("commits=%d rollbacks=%d", b.commits, b.rollbacks)
	}
}
