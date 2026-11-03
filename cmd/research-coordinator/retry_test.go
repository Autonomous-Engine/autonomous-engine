package main

import (
	"testing"

	"github.com/google/uuid"
)

// A prd.ready carrying retry=true (the dashboard's Retry) must dispatch
// again even though this process already dispatched the item once: the
// in-memory "dispatched" flag was making Retry a silent no-op (F37).
func TestPrepareDispatchRetryForgetsDispatchedSession(t *testing.T) {
	coord := newCoordinator()
	workID := uuid.New()
	coord.getOrCreate(workID).dispatched = true

	if coord.prepareDispatch(workID, false) {
		t.Fatal("plain redelivery of a dispatched item must not dispatch again")
	}
	if !coord.prepareDispatch(workID, true) {
		t.Fatal("retry must dispatch again")
	}
	if coord.getOrCreate(workID).dispatched {
		t.Fatal("retry must leave a fresh, undispatched session")
	}
}

func TestPrepareDispatchFirstDeliveryDispatches(t *testing.T) {
	coord := newCoordinator()
	if !coord.prepareDispatch(uuid.New(), false) {
		t.Fatal("first delivery must dispatch")
	}
}
