package interactions

import (
	"context"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/bus"
	"github.com/autonomousengine/autonomous-engine/internal/outbox"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// recordingTx is a pgx.Tx that records Exec calls; any other method panics
// through the nil embedded interface. It never commits or rolls back, so a
// test also proves PublishTx leaves both to the caller.
type recordingTx struct {
	pgx.Tx
	sqls    []string
	args    [][]any
	failAt  int // 1-based statement index to fail; 0 = never
	execErr error
}

func (r *recordingTx) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	r.sqls = append(r.sqls, sql)
	r.args = append(r.args, args)
	if r.failAt == len(r.sqls) {
		return pgconn.CommandTag{}, r.execErr
	}
	return pgconn.CommandTag{}, nil
}

func samplePrompt() *Prompt {
	return &Prompt{
		ID:          uuid.New(),
		WorkItemID:  uuid.New(),
		ProductID:   uuid.New(),
		AskingAgent: "product-agent",
		Trigger:     TriggerApprovalPRD,
		PromptType:  PromptTypeApproval,
		Headline:    "PRD ready",
		Question:    "Approve?",
		Actions:     []Action{{ID: "approve", Label: "Approve", Semantic: SemanticContinue}},
		State:       StatePending,
		RequestedAt: time.Now().UTC(),
		PromptedVia: []string{"dashboard"},
	}
}

func TestPublishTx_WritesRowThenEnqueuesInSameTx(t *testing.T) {
	tx := &recordingTx{}
	p := samplePrompt()
	// A nil bus proves PublishTx never publishes directly.
	if err := NewPublisher(nil).PublishTx(context.Background(), tx, p.WorkItemID, p); err != nil {
		t.Fatalf("PublishTx: %v", err)
	}
	if len(tx.sqls) != 2 {
		t.Fatalf("statements = %d (%q), want row insert + outbox insert", len(tx.sqls), tx.sqls)
	}
	if tx.sqls[0] != insertPromptSQL {
		t.Errorf("first statement is not insertPromptSQL: %s", tx.sqls[0])
	}
	if tx.args[0][0] != p.ID || tx.args[0][1] != p.WorkItemID {
		t.Errorf("row args = %v", tx.args[0][:2])
	}
	if !strings.Contains(tx.sqls[1], "INSERT INTO outbox_events") {
		t.Errorf("second statement is not the outbox insert: %s", tx.sqls[1])
	}
	ev := tx.args[1]
	if ev[1] != outbox.AggregateTypeWorkItem || ev[2] != p.WorkItemID {
		t.Errorf("aggregate = %v/%v, want work_item/%s", ev[1], ev[2], p.WorkItemID)
	}
	if ev[3] != bus.TopicUserPromptRequested {
		t.Errorf("topic = %v, want %s", ev[3], bus.TopicUserPromptRequested)
	}
	if ev[4] != p.WorkItemID.String() {
		t.Errorf("key = %v, want the work item id (same partitioning as PublishRequested)", ev[4])
	}
	var got Prompt
	if err := json.Unmarshal(ev[5].([]byte), &got); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if got.ID != p.ID || got.Headline != p.Headline || got.WorkItemID != p.WorkItemID {
		t.Errorf("payload = %+v, want the prompt", got)
	}
}

// A subtask's prompt is ordered under the parent work item: the aggregate is
// whatever the caller passes, while the key and payload stay the prompt's own
// work item, exactly as PublishRequested partitions it.
func TestPublishTx_UsesCallerAggregateButPromptKey(t *testing.T) {
	tx := &recordingTx{}
	p := samplePrompt()
	parent := uuid.New()
	if err := NewPublisher(nil).PublishTx(context.Background(), tx, parent, p); err != nil {
		t.Fatalf("PublishTx: %v", err)
	}
	ev := tx.args[1]
	if ev[2] != parent {
		t.Errorf("aggregate_id = %v, want the parent %s", ev[2], parent)
	}
	if ev[4] != p.WorkItemID.String() {
		t.Errorf("key = %v, want the prompt's work item %s", ev[4], p.WorkItemID)
	}
	var got Prompt
	if err := json.Unmarshal(ev[5].([]byte), &got); err != nil {
		t.Fatal(err)
	}
	if got.WorkItemID != p.WorkItemID {
		t.Errorf("payload work_item_id = %s, want %s", got.WorkItemID, p.WorkItemID)
	}
}

// The event payload must be what PublishRequested puts on the bus today, plus
// only the additive event_id, so consumers need no change.
func TestPublishTx_PayloadMatchesPublishRequestedPlusEventID(t *testing.T) {
	p := samplePrompt()
	tx := &recordingTx{}
	if err := NewPublisher(nil).PublishTx(context.Background(), tx, p.WorkItemID, p); err != nil {
		t.Fatal(err)
	}
	var viaOutbox map[string]any
	if err := json.Unmarshal(tx.args[1][5].([]byte), &viaOutbox); err != nil {
		t.Fatal(err)
	}
	if _, ok := viaOutbox[outbox.FieldEventID]; !ok {
		t.Error("payload has no event_id")
	}
	delete(viaOutbox, outbox.FieldEventID)

	direct, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	var viaBus map[string]any
	if err := json.Unmarshal(direct, &viaBus); err != nil {
		t.Fatal(err)
	}
	a, _ := json.Marshal(viaOutbox)
	b, _ := json.Marshal(viaBus)
	if string(a) != string(b) {
		t.Errorf("outbox payload\n%s\ndiffers from PublishRequested payload\n%s", a, b)
	}
}

func TestPublishTx_DefaultsIDAndRequestedAt(t *testing.T) {
	p := samplePrompt()
	p.ID = uuid.Nil
	p.RequestedAt = time.Time{}
	tx := &recordingTx{}
	if err := NewPublisher(nil).PublishTx(context.Background(), tx, p.WorkItemID, p); err != nil {
		t.Fatal(err)
	}
	if p.ID == uuid.Nil || p.RequestedAt.IsZero() {
		t.Fatalf("defaults not applied: id=%s requested_at=%s", p.ID, p.RequestedAt)
	}
	var got Prompt
	if err := json.Unmarshal(tx.args[1][5].([]byte), &got); err != nil {
		t.Fatal(err)
	}
	if got.ID != p.ID {
		t.Errorf("event carries prompt id %s, row has %s", got.ID, p.ID)
	}
}

func TestPublishTx_Rejects(t *testing.T) {
	ctx := context.Background()
	pub := NewPublisher(nil)
	if err := pub.PublishTx(ctx, nil, uuid.New(), samplePrompt()); err == nil {
		t.Error("nil tx accepted")
	}
	tx := &recordingTx{}
	if err := pub.PublishTx(ctx, tx, uuid.New(), nil); err == nil {
		t.Error("nil prompt accepted")
	}
	p := samplePrompt()
	p.WorkItemID = uuid.Nil
	if err := pub.PublishTx(ctx, tx, uuid.New(), p); err == nil {
		t.Error("prompt without a work item accepted")
	}
	if err := pub.PublishTx(ctx, tx, uuid.Nil, samplePrompt()); err == nil {
		t.Error("nil aggregate id accepted")
	}
	if len(tx.sqls) != 0 {
		t.Errorf("rejected prompts reached the database: %q", tx.sqls)
	}
}

func TestPublishTx_RowInsertFailureSkipsEnqueue(t *testing.T) {
	sentinel := errors.New("fk violation")
	tx := &recordingTx{failAt: 1, execErr: sentinel}
	err := NewPublisher(nil).PublishTx(context.Background(), tx, uuid.New(), samplePrompt())
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want the insert error", err)
	}
	if len(tx.sqls) != 1 {
		t.Errorf("statements = %q, want the enqueue skipped", tx.sqls)
	}
}

func TestPublishTx_EnqueueFailureIsReturned(t *testing.T) {
	sentinel := errors.New("outbox_events missing")
	tx := &recordingTx{failAt: 2, execErr: sentinel}
	if err := NewPublisher(nil).PublishTx(context.Background(), tx, uuid.New(), samplePrompt()); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want the enqueue error", err)
	}
}

// Store.Insert and PublishTx must write the row through the same statement.
func TestPromptInsert_SharedByStoreAndPublishTx(t *testing.T) {
	fset := token.NewFileSet()
	funcs := map[string]*ast.FuncDecl{}
	for _, name := range []string{"store.go", "publisher.go"} {
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range f.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok {
				key := fd.Name.Name
				if fd.Recv != nil {
					key = recvName(fd) + "." + key
				}
				funcs[key] = fd
			}
		}
	}
	reaches := func(fn string) (callsHelper, inlineSQL bool) {
		decl := funcs[fn]
		if decl == nil {
			t.Fatalf("%s not found", fn)
		}
		ast.Inspect(decl.Body, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.Ident:
				if n.Name == "insertPrompt" {
					callsHelper = true
				}
			case *ast.BasicLit:
				if strings.Contains(strings.ToUpper(n.Value), "INSERT INTO USER_INTERACTIONS") {
					inlineSQL = true
				}
			}
			return true
		})
		return
	}
	for _, fn := range []string{"Store.Insert", "Publisher.PublishTx"} {
		calls, inline := reaches(fn)
		if !calls {
			t.Errorf("%s does not write through insertPrompt", fn)
		}
		if inline {
			t.Errorf("%s spells the user_interactions insert inline", fn)
		}
	}
	helperUsesConst := false
	ast.Inspect(funcs["insertPrompt"].Body, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok && id.Name == "insertPromptSQL" {
			helperUsesConst = true
		}
		return true
	})
	if !helperUsesConst {
		t.Error("insertPrompt does not use insertPromptSQL")
	}
}

func recvName(fd *ast.FuncDecl) string {
	t := fd.Recv.List[0].Type
	if s, ok := t.(*ast.StarExpr); ok {
		t = s.X
	}
	if id, ok := t.(*ast.Ident); ok {
		return id.Name
	}
	return ""
}
