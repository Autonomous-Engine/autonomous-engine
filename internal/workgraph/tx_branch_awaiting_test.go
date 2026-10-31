package workgraph

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/models"
	"github.com/google/uuid"
)

// SetBranchTx and TransitionFromAwaitingUserToStateTx were added for M1-04b
// (pr-coordinator records the branch with pr.ready; dashboard-backend's PRD
// prompt answer transitions with prd.ready). They follow the M1-04-0 rules:
// shared SQL with the pooled twin, no commit or rollback of the caller's tx.

func TestSetBranchTx_UsesSharedUpdateInCallersTx(t *testing.T) {
	tx := &fakeTx{}
	id := uuid.New()
	if err := New(nil).SetBranchTx(context.Background(), tx, id, "agent/x/y"); err != nil {
		t.Fatalf("SetBranchTx: %v", err)
	}
	if !equalStrings(tx.sqls, []string{setBranchSQL}) {
		t.Errorf("statements = %q", tx.sqls)
	}
	if tx.args[0][0] != "agent/x/y" || tx.args[0][2] != id {
		t.Errorf("args = %v", tx.args[0])
	}
	if tx.commits != 0 || tx.rollbacks != 0 {
		t.Errorf("SetBranchTx committed/rolled back the caller's tx")
	}
}

func TestSetBranchTx_RejectsNilTxAndPropagatesErrors(t *testing.T) {
	if err := New(nil).SetBranchTx(context.Background(), nil, uuid.New(), "b"); err == nil {
		t.Error("nil tx accepted")
	}
	sentinel := errors.New("exec failed")
	if err := New(nil).SetBranchTx(context.Background(), &fakeTx{execErr: sentinel}, uuid.New(), "b"); !errors.Is(err, sentinel) {
		t.Errorf("err = %v", err)
	}
}

func TestSetBranch_SharesSQLWithTx(t *testing.T) {
	src := parseWorkgraph(t)
	a, b := src.sqlConsts(t, "SetBranch"), src.sqlConsts(t, "SetBranchTx")
	if !equalStrings(a, []string{"setBranchSQL"}) || !equalStrings(a, b) {
		t.Errorf("SetBranch uses %v, SetBranchTx uses %v; both must use only setBranchSQL", a, b)
	}
	for _, fn := range []string{"SetBranch", "SetBranchTx"} {
		if lit := src.inlineSQL(t, fn); lit != "" {
			t.Errorf("%s carries inline SQL %q", fn, lit)
		}
	}
}

func TestTransitionFromAwaitingUserToStateTx_RunsInCallersTx(t *testing.T) {
	for _, from := range []models.WorkItemState{models.WorkItemStateAwaitingUser, models.WorkItemStatePRDApproval} {
		tx := &fakeTx{currentState: from}
		id := uuid.New()
		err := TransitionFromAwaitingUserToStateTx(context.Background(), tx, id, models.WorkItemStateResearch, "u", "approved")
		if err != nil {
			t.Fatalf("from %s: %v", from, err)
		}
		if !equalStrings(tx.sqls, []string{selectAwaitingStateForUpdateSQL, updateFromAwaitingUserSQL, insertAwaitingTransitionSQL}) {
			t.Errorf("from %s: statements = %q", from, tx.sqls)
		}
		if tx.args[1][0] != models.WorkItemStateResearch || tx.args[1][2] != id {
			t.Errorf("update args = %v", tx.args[1])
		}
		if tx.args[2][2] != from || tx.args[2][3] != models.WorkItemStateResearch || tx.args[2][4] != "u" || tx.args[2][5] != "approved" {
			t.Errorf("transition args = %v", tx.args[2])
		}
		if tx.commits != 0 || tx.rollbacks != 0 {
			t.Errorf("committed/rolled back the caller's tx")
		}
	}
}

func TestTransitionFromAwaitingUserToStateTx_WrongStateWritesNothing(t *testing.T) {
	tx := &fakeTx{currentState: models.WorkItemStateResearch}
	err := TransitionFromAwaitingUserToStateTx(context.Background(), tx, uuid.New(), models.WorkItemStateResearch, "u", "r")
	if err == nil || !strings.Contains(err.Error(), "expected awaiting_user or prd_approval") {
		t.Fatalf("err = %v", err)
	}
	if len(tx.sqls) != 1 {
		t.Errorf("statements = %q, want only the locking select", tx.sqls)
	}
	if err := TransitionFromAwaitingUserToStateTx(context.Background(), nil, uuid.New(), models.WorkItemStateResearch, "u", "r"); err == nil {
		t.Error("nil tx accepted")
	}
}

func TestTransitionFromAwaitingUserToState_PooledDelegatesToTx(t *testing.T) {
	src := parseWorkgraph(t)
	if lit := src.inlineSQL(t, "TransitionFromAwaitingUserToState"); lit != "" {
		t.Errorf("pooled TransitionFromAwaitingUserToState carries inline SQL %q; it must run the Tx variant", lit)
	}
	a, b := src.sqlConsts(t, "TransitionFromAwaitingUserToState"), src.sqlConsts(t, "TransitionFromAwaitingUserToStateTx")
	if len(b) != 3 || !equalStrings(a, b) {
		t.Errorf("pooled uses %v, Tx uses %v", a, b)
	}
}
