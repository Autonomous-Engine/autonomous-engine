package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/bus"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// recordingTx is a pgx.Tx that records Exec calls. Anything else panics via
// the nil embedded interface.
type recordingTx struct {
	pgx.Tx
	sqls    []string
	args    [][]any
	execErr error
}

func (r *recordingTx) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	r.sqls = append(r.sqls, sql)
	r.args = append(r.args, args)
	return pgconn.CommandTag{}, r.execErr
}

func TestEnqueueJSON_EnqueuesWorkItemEventInCallersTx(t *testing.T) {
	tx := &recordingTx{}
	agg := uuid.New()
	payload := map[string]any{"work_id": agg.String(), "to_state": "prd"}
	if err := EnqueueJSON(context.Background(), tx, agg, "state.changed", "the-key", payload); err != nil {
		t.Fatalf("EnqueueJSON: %v", err)
	}
	if len(tx.sqls) != 1 || tx.sqls[0] != insertSQL {
		t.Fatalf("statements = %q, want exactly the outbox insert", tx.sqls)
	}
	a := tx.args[0]
	if a[1] != AggregateTypeWorkItem || AggregateTypeWorkItem != "work_item" {
		t.Errorf("aggregate_type = %v, want work_item", a[1])
	}
	if a[2] != agg {
		t.Errorf("aggregate_id = %v, want %s", a[2], agg)
	}
	if a[3] != "state.changed" || a[4] != "the-key" {
		t.Errorf("topic/key = %v/%v", a[3], a[4])
	}
	var body map[string]any
	if err := json.Unmarshal(a[5].([]byte), &body); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if body["to_state"] != "prd" || body["work_id"] != agg.String() {
		t.Errorf("payload lost fields: %v", body)
	}
	id, _ := body[FieldEventID].(string)
	if id == "" || a[0].(uuid.UUID).String() != id {
		t.Errorf("payload event_id %q does not match row id %v", id, a[0])
	}
	var headers map[string]string
	if err := json.Unmarshal(a[6].([]byte), &headers); err != nil || headers[HeaderEventID] != id {
		t.Errorf("headers = %s (%v), want event_id %s", a[6], err, id)
	}
}

// EnqueueJSON must be NewEvent+Enqueue and nothing else: the same payload
// through both routes yields the same row, apart from the fresh event id.
func TestEnqueueJSON_MatchesNewEventPlusEnqueue(t *testing.T) {
	agg := uuid.New()
	payload := map[string]any{"event_id": uuid.New().String(), "x": 1}

	viaHelper := &recordingTx{}
	if err := EnqueueJSON(context.Background(), viaHelper, agg, "t", "k", payload); err != nil {
		t.Fatal(err)
	}
	ev, err := NewEvent("work_item", agg, "t", "k", payload)
	if err != nil {
		t.Fatal(err)
	}
	viaManual := &recordingTx{}
	if err := Enqueue(context.Background(), viaManual, ev); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(viaHelper.args) != fmt.Sprint(viaManual.args) {
		t.Errorf("EnqueueJSON args %v\n differ from NewEvent+Enqueue %v", viaHelper.args, viaManual.args)
	}
}

func TestEnqueueJSON_Rejects(t *testing.T) {
	ctx := context.Background()
	if err := EnqueueJSON(ctx, nil, uuid.New(), "t", "k", map[string]any{"a": 1}); err == nil ||
		!strings.Contains(err.Error(), "transaction") {
		t.Errorf("nil tx: err = %v", err)
	}
	tx := &recordingTx{}
	if err := EnqueueJSON(ctx, tx, uuid.Nil, "t", "k", map[string]any{"a": 1}); err == nil {
		t.Error("nil aggregate id accepted")
	}
	if err := EnqueueJSON(ctx, tx, uuid.New(), "", "k", map[string]any{"a": 1}); err == nil {
		t.Error("empty topic accepted")
	}
	if err := EnqueueJSON(ctx, tx, uuid.New(), "t", "k", nil); err == nil {
		t.Error("nil payload accepted")
	}
	if err := EnqueueJSON(ctx, tx, uuid.New(), "t", "k", func() {}); err == nil {
		t.Error("unencodable payload accepted")
	}
	if len(tx.sqls) != 0 {
		t.Errorf("rejected events reached the database: %q", tx.sqls)
	}
}

func TestEnqueueJSON_PropagatesInsertError(t *testing.T) {
	sentinel := errors.New("insert failed")
	err := EnqueueJSON(context.Background(), &recordingTx{execErr: sentinel}, uuid.New(), "t", "k", map[string]any{"a": 1})
	if !errors.Is(err, sentinel) {
		t.Errorf("err = %v, want wrapped insert error", err)
	}
}

func TestClaimedByForService(t *testing.T) {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown-host"
	}
	got, err := claimedByForService("status-orchestrator")
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("status-orchestrator/%s/%d", host, os.Getpid())
	if got != want {
		t.Errorf("claimedBy = %q, want %q", got, want)
	}
	for _, bad := range []string{"", "   "} {
		if _, err := claimedByForService(bad); err == nil {
			t.Errorf("service %q accepted", bad)
		}
	}
}

func TestStartForService_RejectsEmptyService(t *testing.T) {
	if _, err := StartForService(context.Background(), nil, nil, nil, ""); err == nil {
		t.Fatal("empty service accepted")
	}
}

func TestStartForService_RequiresPoolAndPublisher(t *testing.T) {
	if _, err := StartForService(context.Background(), nil, &noopPublisher{}, nil, "svc"); err == nil {
		t.Error("nil pool accepted")
	}
}

type noopPublisher struct{}

func (noopPublisher) PublishMessage(context.Context, *bus.Message) error { return nil }

// StartForService names the dispatcher after the service. pgxpool connects
// lazily, so a pool over an unreachable address is enough to build one; the
// context is already cancelled, so the loop exits before it claims anything.
func TestStartForService_NamesDispatcherAfterService(t *testing.T) {
	pool, err := pgxpool.New(context.Background(), "postgres://nobody@127.0.0.1:1/none?connect_timeout=1")
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	defer pool.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	d, err := StartForService(ctx, pool, noopPublisher{}, log.New(io.Discard, "", 0), "gateway")
	if err != nil {
		t.Fatalf("StartForService: %v", err)
	}
	waited := make(chan struct{})
	go func() { d.Wait(); close(waited) }()
	select {
	case <-waited:
	case <-time.After(5 * time.Second):
		t.Fatal("dispatcher did not stop on a cancelled context")
	}
	want, _ := claimedByForService("gateway")
	if d.Name() != want {
		t.Errorf("Name() = %q, want %q", d.Name(), want)
	}
	if !strings.HasPrefix(d.Name(), "gateway/") {
		t.Errorf("Name() = %q, want it to lead with the service", d.Name())
	}
}
