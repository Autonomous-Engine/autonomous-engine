package workgraph

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// TxBeginner is anything that can open a transaction. *pgxpool.Pool satisfies
// it; the interface exists so WithTx can be tested without a database.
type TxBeginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// WithTx runs fn inside one transaction on db: it begins, calls fn, and
// commits when fn returns nil. When fn returns an error the transaction is
// rolled back and that error is returned unchanged, so callers can still
// compare it with errors.Is or ==. When fn panics the transaction is rolled
// back and the panic is re-raised.
//
// This is the shape every outbox producer takes: the state write (one of the
// *Tx methods on WorkGraph) and outbox.Enqueue / EnqueueJSON go in the same fn,
// so the row and its event commit together or not at all.
//
//	err := workgraph.WithTx(ctx, pool, func(tx pgx.Tx) error {
//		if _, err := wg.TransitionStateTx(ctx, tx, id, to, agentID, reason); err != nil {
//			return err
//		}
//		return outbox.EnqueueJSON(ctx, tx, id, topic, id.String(), payload)
//	})
//
// The rollback uses a context detached from ctx's cancellation, so a request
// that timed out mid-callback still asks the server to abort rather than
// leaving that to connection teardown.
func WithTx(ctx context.Context, db TxBeginner, fn func(pgx.Tx) error) (err error) {
	if db == nil {
		return errors.New("with tx: nil database")
	}
	if fn == nil {
		return errors.New("with tx: nil callback")
	}
	tx, err := db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}

	committed := false
	defer func() {
		if committed {
			return
		}
		// Runs on an error return and while a panic unwinds; the panic, if
		// any, continues after this deferred call returns.
		_ = tx.Rollback(context.WithoutCancel(ctx))
	}()

	if err := fn(tx); err != nil {
		return err
	}
	committed = true
	if err := tx.Commit(ctx); err != nil {
		// pgx has already ended the transaction on a failed commit; there is
		// nothing left to roll back.
		return fmt.Errorf("commit transaction: %w", err)
	}
	return nil
}
