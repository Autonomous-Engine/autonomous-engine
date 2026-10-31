package tools

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/bus"
	"github.com/autonomousengine/autonomous-engine/internal/outbox"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type cwiTx struct {
	pgx.Tx
	b *cwiBeginner
}

func (t *cwiTx) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	t.b.sqls = append(t.b.sqls, sql)
	t.b.args = append(t.b.args, args)
	if t.b.failAt == len(t.b.sqls) {
		return pgconn.CommandTag{}, errors.New("exec failed")
	}
	return pgconn.CommandTag{}, nil
}
func (t *cwiTx) Commit(context.Context) error   { t.b.commits++; return nil }
func (t *cwiTx) Rollback(context.Context) error { t.b.rollbacks++; return nil }

type cwiBeginner struct {
	failAt             int
	sqls               []string
	args               [][]any
	commits, rollbacks int
}

func (b *cwiBeginner) Begin(context.Context) (pgx.Tx, error) { return &cwiTx{b: b}, nil }

// TestCreateWorkItemWithIntake_RowAndIntakeShareOneTx covers the chat agent's
// create_work_item tool (M1-04e): before, the row was inserted and intake
// published separately, and a publish failure was ignored — leaving a work
// item in intake that nothing would ever pick up.
func TestCreateWorkItemWithIntake_RowAndIntakeShareOneTx(t *testing.T) {
	b := &cwiBeginner{}
	id, product := uuid.New(), uuid.New()
	if err := createWorkItemWithIntake(context.Background(), b, id, "T", "D", "bug", "high", &product); err != nil {
		t.Fatalf("createWorkItemWithIntake: %v", err)
	}
	if b.commits != 1 || len(b.sqls) != 2 {
		t.Fatalf("commits=%d statements=%v", b.commits, b.sqls)
	}
	if b.sqls[0] != createWorkItemSQL {
		t.Errorf("first statement is not createWorkItemSQL: %s", b.sqls[0])
	}
	if a := b.args[0]; a[0] != id || a[1] != "T" || a[3] != "bug" || a[4] != "high" || a[5] != &product {
		t.Errorf("row args = %v", a)
	}
	ev := b.args[1]
	if !strings.Contains(b.sqls[1], "INSERT INTO outbox_events") || ev[1] != outbox.AggregateTypeWorkItem || ev[2] != id ||
		ev[3] != bus.TopicIntake || ev[4] != id.String() {
		t.Errorf("event = %v", ev[:5])
	}
	var payload map[string]any
	if err := json.Unmarshal(ev[5].([]byte), &payload); err != nil {
		t.Fatal(err)
	}
	if payload["work_id"] != id.String() || payload["title"] != "T" || payload["description"] != "D" || len(payload) != 4 {
		t.Errorf("payload = %v, want work_id/title/description plus event_id", payload)
	}

	b = &cwiBeginner{failAt: 2}
	if err := createWorkItemWithIntake(context.Background(), b, id, "T", "D", "bug", "high", nil); err == nil {
		t.Fatal("expected an error")
	}
	if b.commits != 0 || b.rollbacks != 1 {
		t.Errorf("a failed enqueue must roll back the work item: commits=%d rollbacks=%d", b.commits, b.rollbacks)
	}
}

func TestExecutor_NeverPublishesIntakeDirectly(t *testing.T) {
	src, err := os.ReadFile("executor.go")
	if err != nil {
		t.Fatal(err)
	}
	code := regexp.MustCompile(`(?m)//.*$`).ReplaceAllString(string(src), " ")
	if regexp.MustCompile(`\.Publish\([^,]+,\s*bus\.TopicIntake`).MatchString(code) {
		t.Error("executor publishes intake directly; it must be enqueued with the work item")
	}
}
