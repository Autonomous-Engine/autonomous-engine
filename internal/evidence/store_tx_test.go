package evidence

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// recordingTx is a pgx.Tx that records Exec calls; anything else panics
// through the nil embedded interface (including Commit/Rollback, which
// InsertTx must leave to the caller).
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

func TestInsertTx_UsesSharedInsertInCallersTx(t *testing.T) {
	ev := sampleEvidence(t)
	tx := &recordingTx{}
	// A Store with a nil pool panics on any pooled query, so this also proves
	// InsertTx never touches the pool.
	id, err := NewStore(nil).InsertTx(context.Background(), tx, &ev)
	if err != nil {
		t.Fatalf("InsertTx: %v", err)
	}
	if id == uuid.Nil {
		t.Error("InsertTx returned the nil id")
	}
	if len(tx.sqls) != 1 || tx.sqls[0] != insertSQL {
		t.Fatalf("statements = %q, want exactly insertSQL", tx.sqls)
	}
	want, err := insertArgs(id, &ev)
	if err != nil {
		t.Fatal(err)
	}
	if len(tx.args[0]) != len(want) || tx.args[0][0] != id || tx.args[0][1] != want[1] {
		t.Errorf("args = %v, want %v", tx.args[0], want)
	}
}

func TestInsertTx_RejectsNilTxAndInvalidEvidence(t *testing.T) {
	ev := sampleEvidence(t)
	if _, err := NewStore(nil).InsertTx(context.Background(), nil, &ev); err == nil {
		t.Error("nil tx accepted")
	}
	tx := &recordingTx{}
	if _, err := NewStore(nil).InsertTx(context.Background(), tx, nil); err == nil {
		t.Error("nil evidence accepted")
	}
	if len(tx.sqls) != 0 {
		t.Errorf("invalid evidence reached the database: %q", tx.sqls)
	}
}

func TestInsertTx_PropagatesExecError(t *testing.T) {
	ev := sampleEvidence(t)
	sentinel := errors.New("insert failed")
	id, err := NewStore(nil).InsertTx(context.Background(), &recordingTx{execErr: sentinel}, &ev)
	if !errors.Is(err, sentinel) || id != uuid.Nil {
		t.Errorf("InsertTx = %v, %v; want nil id and the wrapped error", id, err)
	}
}
