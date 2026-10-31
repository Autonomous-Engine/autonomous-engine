package workgraph

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"sort"
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/models"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// ─── fakes ───────────────────────────────────────────────────────────────────

// fakeTx is a pgx.Tx that records the statements run on it and whether it was
// committed or rolled back. Methods the helpers never call fall through to the
// embedded nil interface and panic, which is what a test wants: an unexpected
// call is a failure, not a silent no-op.
type fakeTx struct {
	pgx.Tx

	sqls        []string
	args        [][]any
	execErr     error
	commitErr   error
	rollbackErr error
	commits     int
	rollbacks   int

	// currentState is what the SELECT ... FOR UPDATE row scans.
	currentState models.WorkItemState
	queryErr     error
}

func (f *fakeTx) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	f.sqls = append(f.sqls, sql)
	f.args = append(f.args, args)
	return pgconn.CommandTag{}, f.execErr
}

func (f *fakeTx) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	f.sqls = append(f.sqls, sql)
	f.args = append(f.args, args)
	return fakeRow{state: f.currentState, err: f.queryErr}
}

func (f *fakeTx) Commit(context.Context) error {
	f.commits++
	return f.commitErr
}

func (f *fakeTx) Rollback(context.Context) error {
	f.rollbacks++
	return f.rollbackErr
}

type fakeRow struct {
	state models.WorkItemState
	err   error
}

func (r fakeRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	*(dest[0].(*models.WorkItemState)) = r.state
	return nil
}

// fakeBeginner hands out one fakeTx.
type fakeBeginner struct {
	tx       *fakeTx
	beginErr error
	begins   int
}

func (b *fakeBeginner) Begin(context.Context) (pgx.Tx, error) {
	b.begins++
	if b.beginErr != nil {
		return nil, b.beginErr
	}
	return b.tx, nil
}

// ─── WithTx ──────────────────────────────────────────────────────────────────

func TestWithTx_CommitsOnSuccess(t *testing.T) {
	tx := &fakeTx{}
	b := &fakeBeginner{tx: tx}
	var got pgx.Tx
	err := WithTx(context.Background(), b, func(t pgx.Tx) error {
		got = t
		return nil
	})
	if err != nil {
		t.Fatalf("WithTx: %v", err)
	}
	if got != pgx.Tx(tx) {
		t.Errorf("fn received %v, want the begun transaction", got)
	}
	if tx.commits != 1 || tx.rollbacks != 0 {
		t.Errorf("commits=%d rollbacks=%d, want 1/0", tx.commits, tx.rollbacks)
	}
}

func TestWithTx_RollsBackOnErrorAndReturnsIt(t *testing.T) {
	tx := &fakeTx{}
	sentinel := errors.New("boom")
	err := WithTx(context.Background(), &fakeBeginner{tx: tx}, func(pgx.Tx) error { return sentinel })
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want the callback's error", err)
	}
	if err != sentinel {
		t.Errorf("err = %q, want the callback's error unwrapped so callers can compare it", err)
	}
	if tx.commits != 0 || tx.rollbacks != 1 {
		t.Errorf("commits=%d rollbacks=%d, want 0/1", tx.commits, tx.rollbacks)
	}
}

func TestWithTx_CallbackErrorWinsOverRollbackError(t *testing.T) {
	tx := &fakeTx{rollbackErr: errors.New("conn gone")}
	sentinel := errors.New("boom")
	err := WithTx(context.Background(), &fakeBeginner{tx: tx}, func(pgx.Tx) error { return sentinel })
	if err != sentinel {
		t.Fatalf("err = %v, want the callback's error", err)
	}
}

func TestWithTx_RollsBackOnPanicAndRepanics(t *testing.T) {
	tx := &fakeTx{}
	defer func() {
		r := recover()
		if r != "kaboom" {
			t.Fatalf("recovered %v, want the original panic value", r)
		}
		if tx.commits != 0 || tx.rollbacks != 1 {
			t.Errorf("commits=%d rollbacks=%d, want 0/1", tx.commits, tx.rollbacks)
		}
	}()
	_ = WithTx(context.Background(), &fakeBeginner{tx: tx}, func(pgx.Tx) error { panic("kaboom") })
	t.Fatal("WithTx returned; want the panic to propagate")
}

func TestWithTx_RollsBackWhenCtxAlreadyCancelled(t *testing.T) {
	// A cancelled ctx must not leave the callback's error unrolled-back: the
	// rollback runs regardless (pgx tears the connection down for a cancelled
	// context either way, but the helper must still ask).
	tx := &fakeTx{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := WithTx(ctx, &fakeBeginner{tx: tx}, func(pgx.Tx) error { return context.Canceled })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if tx.rollbacks != 1 {
		t.Errorf("rollbacks=%d, want 1", tx.rollbacks)
	}
}

func TestWithTx_BeginError(t *testing.T) {
	sentinel := errors.New("no conn")
	called := false
	err := WithTx(context.Background(), &fakeBeginner{beginErr: sentinel}, func(pgx.Tx) error {
		called = true
		return nil
	})
	if !errors.Is(err, sentinel) || !strings.Contains(err.Error(), "begin transaction") {
		t.Fatalf("err = %v, want wrapped begin error", err)
	}
	if called {
		t.Error("callback ran without a transaction")
	}
}

func TestWithTx_CommitError(t *testing.T) {
	sentinel := errors.New("serialization failure")
	tx := &fakeTx{commitErr: sentinel}
	err := WithTx(context.Background(), &fakeBeginner{tx: tx}, func(pgx.Tx) error { return nil })
	if !errors.Is(err, sentinel) || !strings.Contains(err.Error(), "commit transaction") {
		t.Fatalf("err = %v, want wrapped commit error", err)
	}
}

func TestWithTx_RejectsNilArguments(t *testing.T) {
	if err := WithTx(context.Background(), nil, func(pgx.Tx) error { return nil }); err == nil {
		t.Error("nil db: want error")
	}
	b := &fakeBeginner{tx: &fakeTx{}}
	if err := WithTx(context.Background(), b, nil); err == nil {
		t.Error("nil fn: want error")
	}
	if b.begins != 0 {
		t.Error("nil fn must be rejected before a transaction is opened")
	}
}

// ─── Tx variants: runtime ────────────────────────────────────────────────────

func TestTxVariants_RejectNilTransaction(t *testing.T) {
	wg := New(nil)
	ctx := context.Background()
	if _, err := wg.TransitionStateTx(ctx, nil, uuid.New(), models.WorkItemStatePRD, "a", "r"); err == nil {
		t.Error("TransitionStateTx: want error for nil tx")
	}
	if err := wg.AddArtifactTx(ctx, nil, &models.WorkItemArtifact{}); err == nil {
		t.Error("AddArtifactTx: want error for nil tx")
	}
	if err := wg.AssignAgentTx(ctx, nil, &models.AgentAssignment{}); err == nil {
		t.Error("AssignAgentTx: want error for nil tx")
	}
	if err := wg.CompleteAssignmentTx(ctx, nil, uuid.New(), models.AssignmentStatusCompleted); err == nil {
		t.Error("CompleteAssignmentTx: want error for nil tx")
	}
}

func TestTransitionStateTx_RunsSharedStatementsInCallersTx(t *testing.T) {
	tx := &fakeTx{currentState: models.WorkItemStateIntake}
	itemID := uuid.New()
	from, err := New(nil).TransitionStateTx(context.Background(), tx, itemID, models.WorkItemStatePRD, "agent", "why")
	if err != nil {
		t.Fatalf("TransitionStateTx: %v", err)
	}
	if from != models.WorkItemStateIntake {
		t.Errorf("from = %q, want intake", from)
	}
	want := []string{selectStateForUpdateSQL, updateStateSQL, insertTransitionSQL}
	if !equalStrings(tx.sqls, want) {
		t.Errorf("statements = %q, want %q", tx.sqls, want)
	}
	if tx.commits != 0 || tx.rollbacks != 0 {
		t.Errorf("Tx variant must leave commit/rollback to the caller (commits=%d rollbacks=%d)", tx.commits, tx.rollbacks)
	}
	ins := tx.args[2]
	if ins[1] != itemID || ins[2] != models.WorkItemStateIntake || ins[3] != models.WorkItemStatePRD || ins[4] != "agent" || ins[5] != "why" {
		t.Errorf("transition insert args = %v", ins)
	}
}

func TestTransitionStateTx_InvalidTransitionWritesNothing(t *testing.T) {
	tx := &fakeTx{currentState: models.WorkItemStateIntake}
	_, err := New(nil).TransitionStateTx(context.Background(), tx, uuid.New(), models.WorkItemStateReleased, "a", "r")
	if !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("err = %v, want ErrInvalidTransition", err)
	}
	if err.Error() != "invalid transition from intake to released" {
		t.Errorf("message = %q; existing callers log and match this text", err.Error())
	}
	if len(tx.sqls) != 1 {
		t.Errorf("statements = %q, want only the locking select", tx.sqls)
	}
}

func TestTransitionStateTx_NotFound(t *testing.T) {
	tx := &fakeTx{queryErr: pgx.ErrNoRows}
	id := uuid.New()
	_, err := New(nil).TransitionStateTx(context.Background(), tx, id, models.WorkItemStatePRD, "a", "r")
	if err == nil || !strings.Contains(err.Error(), "work item "+id.String()+" not found") {
		t.Fatalf("err = %v, want not found", err)
	}
}

func TestAddArtifactTx_UsesSharedInsertAndDefaults(t *testing.T) {
	tx := &fakeTx{}
	a := &models.WorkItemArtifact{WorkItemID: uuid.New(), Type: models.ArtifactTypePRD, Content: []byte(`{}`), AgentID: "x"}
	if err := New(nil).AddArtifactTx(context.Background(), tx, a); err != nil {
		t.Fatalf("AddArtifactTx: %v", err)
	}
	if !equalStrings(tx.sqls, []string{insertArtifactSQL}) {
		t.Errorf("statements = %q", tx.sqls)
	}
	if a.ID == uuid.Nil || a.CreatedAt.IsZero() {
		t.Errorf("id/created_at not defaulted: %+v", a)
	}
}

func TestAssignAgentTx_UsesSharedInsertAndDefaults(t *testing.T) {
	tx := &fakeTx{}
	a := &models.AgentAssignment{WorkItemID: uuid.New(), AgentType: models.AgentTypeProduct}
	if err := New(nil).AssignAgentTx(context.Background(), tx, a); err != nil {
		t.Fatalf("AssignAgentTx: %v", err)
	}
	if !equalStrings(tx.sqls, []string{insertAssignmentSQL}) {
		t.Errorf("statements = %q", tx.sqls)
	}
	if a.ID == uuid.Nil || a.AssignedAt.IsZero() {
		t.Errorf("id/assigned_at not defaulted: %+v", a)
	}
}

func TestCompleteAssignmentTx_UsesSharedUpdate(t *testing.T) {
	tx := &fakeTx{}
	id := uuid.New()
	if err := New(nil).CompleteAssignmentTx(context.Background(), tx, id, models.AssignmentStatusCompleted); err != nil {
		t.Fatalf("CompleteAssignmentTx: %v", err)
	}
	if !equalStrings(tx.sqls, []string{completeAssignmentSQL}) {
		t.Errorf("statements = %q", tx.sqls)
	}
	if tx.args[0][0] != models.AssignmentStatusCompleted || tx.args[0][2] != id {
		t.Errorf("args = %v", tx.args[0])
	}
}

func TestTxVariants_PropagateExecErrors(t *testing.T) {
	sentinel := errors.New("exec failed")
	ctx := context.Background()
	wg := New(nil)
	if err := wg.AddArtifactTx(ctx, &fakeTx{execErr: sentinel}, &models.WorkItemArtifact{}); !errors.Is(err, sentinel) {
		t.Errorf("AddArtifactTx err = %v", err)
	}
	if err := wg.AssignAgentTx(ctx, &fakeTx{execErr: sentinel}, &models.AgentAssignment{}); !errors.Is(err, sentinel) {
		t.Errorf("AssignAgentTx err = %v", err)
	}
	if err := wg.CompleteAssignmentTx(ctx, &fakeTx{execErr: sentinel}, uuid.New(), models.AssignmentStatusFailed); !errors.Is(err, sentinel) {
		t.Errorf("CompleteAssignmentTx err = %v", err)
	}
	if _, err := wg.TransitionStateTx(ctx, &fakeTx{execErr: sentinel, currentState: models.WorkItemStateIntake}, uuid.New(), models.WorkItemStatePRD, "a", "r"); !errors.Is(err, sentinel) {
		t.Errorf("TransitionStateTx err = %v", err)
	}
}

// ─── Tx variants: SQL-text pinning ───────────────────────────────────────────

// txPairs lists every pooled write and its transactional twin. The pooled
// method's SQL cannot be observed at runtime without a database, so this test
// reads the source instead: both halves of a pair must reach exactly the same
// set of package-level *SQL constants (following calls into package-local
// helpers), and neither may carry SQL as an inline string literal. Together
// that means a change to one path's SQL is a change to the other's.
var txPairs = [][2]string{
	{"CreateWorkItem", "CreateWorkItemTx"},
	{"TransitionState", "TransitionStateTx"},
	{"AddArtifact", "AddArtifactTx"},
	{"AssignAgent", "AssignAgentTx"},
	{"CompleteAssignment", "CompleteAssignmentTx"},
}

func TestTxVariants_ShareSQLConstantsWithPooledOriginals(t *testing.T) {
	src := parseWorkgraph(t)
	for _, pair := range txPairs {
		pooled, txv := pair[0], pair[1]
		t.Run(pooled, func(t *testing.T) {
			a := src.sqlConsts(t, pooled)
			b := src.sqlConsts(t, txv)
			if len(a) == 0 {
				t.Fatalf("%s reaches no *SQL constant", pooled)
			}
			if !equalStrings(a, b) {
				t.Errorf("%s uses %v, %s uses %v — they must share the same SQL constants", pooled, a, txv, b)
			}
			for _, fn := range []string{pooled, txv} {
				if lit := src.inlineSQL(t, fn); lit != "" {
					t.Errorf("%s carries inline SQL %q; move it to a shared constant", fn, lit)
				}
			}
		})
	}
}

// SetAgentDefinitionTx has no pooled twin, but its constant must still be the
// only place that statement is spelled.
func TestSetAgentDefinitionTx_UsesConstant(t *testing.T) {
	src := parseWorkgraph(t)
	got := src.sqlConsts(t, "SetAgentDefinitionTx")
	if !equalStrings(got, []string{"setAgentDefinitionSQL"}) {
		t.Errorf("SetAgentDefinitionTx uses %v", got)
	}
}

type workgraphSource struct {
	funcs  map[string]*ast.FuncDecl
	consts map[string]bool
}

func parseWorkgraph(t *testing.T) *workgraphSource {
	t.Helper()
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi fs.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	s := &workgraphSource{funcs: map[string]*ast.FuncDecl{}, consts: map[string]bool{}}
	for _, f := range pkgs["workgraph"].Files {
		for _, d := range f.Decls {
			switch d := d.(type) {
			case *ast.FuncDecl:
				s.funcs[d.Name.Name] = d
			case *ast.GenDecl:
				if d.Tok != token.CONST {
					continue
				}
				for _, sp := range d.Specs {
					for _, n := range sp.(*ast.ValueSpec).Names {
						if strings.HasSuffix(n.Name, "SQL") {
							s.consts[n.Name] = true
						}
					}
				}
			}
		}
	}
	return s
}

// sqlConsts returns the sorted *SQL constants reachable from fn, following
// calls to package-level functions and to methods/functions on the package
// (by name). WithTx is followed too, but it holds no SQL.
func (s *workgraphSource) sqlConsts(t *testing.T, fn string) []string {
	t.Helper()
	if s.funcs[fn] == nil {
		t.Fatalf("function %s not found", fn)
	}
	found := map[string]bool{}
	seen := map[string]bool{}
	var walk func(name string)
	walk = func(name string) {
		if seen[name] {
			return
		}
		seen[name] = true
		decl := s.funcs[name]
		if decl == nil || decl.Body == nil {
			return
		}
		ast.Inspect(decl.Body, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.Ident:
				if s.consts[n.Name] {
					found[n.Name] = true
				}
				if _, ok := s.funcs[n.Name]; ok {
					walk(n.Name)
				}
			case *ast.SelectorExpr:
				if _, ok := s.funcs[n.Sel.Name]; ok {
					walk(n.Sel.Name)
				}
			}
			return true
		})
	}
	walk(fn)
	out := make([]string, 0, len(found))
	for k := range found {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// inlineSQL returns the first string literal in fn's own body that looks like
// a SQL statement, or "".
func (s *workgraphSource) inlineSQL(t *testing.T, fn string) string {
	t.Helper()
	var hit string
	ast.Inspect(s.funcs[fn].Body, func(n ast.Node) bool {
		lit, ok := n.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING || hit != "" {
			return true
		}
		up := strings.ToUpper(lit.Value)
		for _, kw := range []string{"INSERT ", "UPDATE ", "SELECT ", "DELETE "} {
			if strings.Contains(up, kw) {
				hit = lit.Value
			}
		}
		return true
	})
	return hit
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
