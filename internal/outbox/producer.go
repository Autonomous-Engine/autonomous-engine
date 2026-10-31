package outbox

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// AggregateTypeWorkItem is the aggregate_type every service publisher uses:
// events are ordered per work item (the parent, when the item has one).
const AggregateTypeWorkItem = "work_item"

// EnqueueJSON is NewEvent + Enqueue for the common case: a work-item event
// with a JSON payload, written inside the caller's transaction.
//
// aggregateID is the work item the event is ordered under — the parent work
// item when the item has one, otherwise the item itself. topic, key and
// payload are exactly what the service published directly before conversion;
// the payload gains the additive event_id field when it is a JSON object.
//
// Like Enqueue it takes a pgx.Tx and has no pool variant: the event must
// commit with the state change it describes. Wrap both in workgraph.WithTx.
func EnqueueJSON(ctx context.Context, tx pgx.Tx, aggregateID uuid.UUID, topic, key string, payload any) error {
	if tx == nil {
		return errors.New("outbox: EnqueueJSON requires the caller's transaction")
	}
	ev, err := NewEvent(AggregateTypeWorkItem, aggregateID, topic, key, payload)
	if err != nil {
		return fmt.Errorf("outbox: build %s event: %w", topic, err)
	}
	return Enqueue(ctx, tx, ev)
}

// StartForService is Start with the dispatcher named after the service:
// ClaimedBy becomes "<service>/<hostname>/<pid>", so outbox_events.claimed_by,
// the dispatcher's log lines and an operator reading a stuck claim can all
// tell which service holds a row. Every converted service main uses this
// rather than Start:
//
//	disp, err := outbox.StartForService(ctx, pool, kb, logger, "status-orchestrator")
//	if err != nil { log.Fatalf(...) }
//	...
//	disp.Wait() // on shutdown, after ctx is cancelled
func StartForService(ctx context.Context, pool *pgxpool.Pool, publisher Publisher, logger *log.Logger, service string) (*Dispatcher, error) {
	claimedBy, err := claimedByForService(service)
	if err != nil {
		return nil, err
	}
	return StartWithOptions(ctx, pool, publisher, Options{Logger: logger, ClaimedBy: claimedBy})
}

// claimedByForService prefixes the default process identity with the
// service name.
func claimedByForService(service string) (string, error) {
	service = strings.TrimSpace(service)
	if service == "" {
		return "", errors.New("outbox: StartForService requires a service name")
	}
	return service + "/" + defaultClaimedBy(), nil
}
