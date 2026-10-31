package agentregistry

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// fakeDB records the arguments of every call and answers the seed's
// COUNT(*) with count. INSERT/UPDATE ... RETURNING rows scan a fresh id and
// timestamps.
type fakeDB struct {
	count int
	calls []fakeCall
}

type fakeCall struct {
	sql  string
	args []any
}

func (f *fakeDB) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	f.calls = append(f.calls, fakeCall{sql, args})
	return pgconn.CommandTag{}, nil
}

func (f *fakeDB) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	f.calls = append(f.calls, fakeCall{sql, args})
	return fakeRow{sql: sql, count: f.count}
}

type fakeRow struct {
	sql   string
	count int
}

func (r fakeRow) Scan(dest ...any) error {
	if strings.Contains(r.sql, "COUNT(*)") {
		*(dest[0].(*int)) = r.count
		return nil
	}
	now := time.Now()
	for _, d := range dest {
		switch p := d.(type) {
		case *uuid.UUID:
			*p = uuid.New()
		case *time.Time:
			*p = now
		default:
			return errors.New("fakeRow: unexpected scan target")
		}
	}
	return nil
}

// routingArgs returns the routing_tags ($14) and routing_complexity ($15)
// arguments of an agent_definitions INSERT or UPDATE.
func routingArgs(t *testing.T, c fakeCall) (tags, complexity any) {
	t.Helper()
	if len(c.args) < 15 {
		t.Fatalf("call has %d args, want >= 15: %s", len(c.args), c.sql)
	}
	return c.args[13], c.args[14]
}

func assertNonNilTextArray(t *testing.T, name string, v any) {
	t.Helper()
	s, ok := v.([]string)
	if !ok {
		t.Fatalf("%s: arg is %T, want []string", name, v)
	}
	if s == nil {
		t.Fatalf("%s: arg is a nil []string, which pgx encodes as NULL into a TEXT[] NOT NULL column", name)
	}
}

func TestSeedDefaultsTx_EmptyTable_InsertsNonNullRoutingArrays(t *testing.T) {
	db := &fakeDB{count: 0}
	if err := SeedDefaultsTx(context.Background(), db); err != nil {
		t.Fatalf("SeedDefaultsTx: %v", err)
	}
	var inserts []fakeCall
	for _, c := range db.calls {
		if strings.Contains(c.sql, "INSERT INTO agent_definitions") {
			inserts = append(inserts, c)
		}
	}
	if len(inserts) != 2 {
		t.Fatalf("got %d inserts, want 2", len(inserts))
	}
	for _, c := range inserts {
		name, _ := c.args[0].(string)
		tags, cx := routingArgs(t, c)
		assertNonNilTextArray(t, name+" routing_tags", tags)
		assertNonNilTextArray(t, name+" routing_complexity", cx)
	}
	if got := inserts[0].args[0]; got != "full-stack-engineer" {
		t.Fatalf("first seeded definition = %v, want full-stack-engineer", got)
	}
	if tags, _ := inserts[1].args[13].([]string); len(tags) != 3 {
		t.Fatalf("quick-text-editor routing_tags = %v, want 3 tags", tags)
	}
}

func TestSeedDefaultsTx_NonEmptyTable_IsNoOp(t *testing.T) {
	db := &fakeDB{count: 2}
	if err := SeedDefaultsTx(context.Background(), db); err != nil {
		t.Fatalf("SeedDefaultsTx: %v", err)
	}
	if len(db.calls) != 1 {
		t.Fatalf("got %d calls, want only the count check", len(db.calls))
	}
}

// A definition whose routing slices are nil (e.g. a JSON body with
// "routing_tags": null) is written with empty arrays, never NULL.
func TestCreateTxAndUpdateTx_NilRoutingSlices_SendEmptyArrays(t *testing.T) {
	db := &fakeDB{}
	def := &AgentDefinition{Name: "nil-slices"}
	if err := CreateTx(context.Background(), db, def); err != nil {
		t.Fatalf("CreateTx: %v", err)
	}
	if err := UpdateTx(context.Background(), db, uuid.New(), def); err != nil {
		t.Fatalf("UpdateTx: %v", err)
	}
	if len(db.calls) != 2 {
		t.Fatalf("got %d calls, want 2", len(db.calls))
	}
	for _, c := range db.calls {
		tags, cx := routingArgs(t, c)
		assertNonNilTextArray(t, "routing_tags", tags)
		assertNonNilTextArray(t, "routing_complexity", cx)
	}
}
