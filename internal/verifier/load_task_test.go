package verifier

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// planRowsQuerier serves plan_tasks rows (id, message) and one technical
// brief content.
type planRowsQuerier struct {
	rows  [][2]string
	brief string
}

type fakeRows struct {
	data [][2]string
	i    int
}

func (r *fakeRows) Close()                                       {}
func (r *fakeRows) Err() error                                   { return nil }
func (r *fakeRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *fakeRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *fakeRows) Next() bool                                   { r.i++; return r.i <= len(r.data) }
func (r *fakeRows) Values() ([]any, error)                       { return nil, nil }
func (r *fakeRows) RawValues() [][]byte                          { return nil }
func (r *fakeRows) Conn() *pgx.Conn                              { return nil }
func (r *fakeRows) Scan(dest ...any) error {
	row := r.data[r.i-1]
	*dest[0].(*string) = row[0]
	*dest[1].(*[]byte) = []byte(row[1])
	return nil
}

type briefRow struct{ content string }

func (b briefRow) Scan(dest ...any) error {
	if b.content == "" {
		return pgx.ErrNoRows
	}
	*dest[0].(*[]byte) = []byte(b.content)
	return nil
}

func (q planRowsQuerier) Query(_ context.Context, sql string, _ ...any) (pgx.Rows, error) {
	if !strings.Contains(sql, "FROM plan_tasks") {
		return nil, fmt.Errorf("unexpected query %q", sql)
	}
	return &fakeRows{data: q.rows}, nil
}

func (q planRowsQuerier) QueryRow(_ context.Context, sql string, _ ...any) pgx.Row {
	return briefRow{content: q.brief}
}

func crit(id, text string) string {
	return fmt.Sprintf(`{"id":%q,"text":%q,"source":{"kind":"derived","reason":"r"},"risk":"low"}`, id, text)
}

// R14: the stage verifies one subtask against its own criteria, the plan's
// non-code criteria and the brief criteria no task carries — never a
// sibling's criteria (which that sibling's verification judges).
func TestLoadRequirementsForTaskScopesToTheSubtask(t *testing.T) {
	parent, a, b := uuid.New(), uuid.New(), uuid.New()
	q := planRowsQuerier{
		rows: [][2]string{
			{a.String(), `{"task_spec":{"schema_version":1,"criterion_ids":["AC-1"],"deliverable":"a"},"criteria":[` + crit("AC-1", "a works") + `]}`},
			{b.String(), `{"task_spec":{"schema_version":1,"criterion_ids":["AC-2"],"deliverable":"b"},"criteria":[` + crit("AC-2", "b works") + `]}`},
		},
		brief: `{"criteria":{"schema_version":1,"criteria":[` + crit("AC-1", "a works") + `,` + crit("AC-2", "b works") + `,` + crit("AC-9", "nobody carries me") + `]}}`,
	}
	req, err := LoadRequirementsForTask(context.Background(), q, parent, a)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, c := range req.Criteria {
		ids = append(ids, c.ID)
	}
	if strings.Join(ids, ",") != "AC-1,AC-9" {
		t.Fatalf("criteria = %v, want AC-1 (own) and AC-9 (carried by no task), not the sibling's AC-2", ids)
	}
	if len(req.TaskSpecs) != 1 || req.TaskSpecs[0].TaskID != a.String() {
		t.Fatalf("task specs = %+v, want only the subtask's", req.TaskSpecs)
	}
}

// A subtask with no plan_tasks row (a review fix) is verified against the
// parent's whole requirements.
func TestLoadRequirementsForTaskWithoutAPlanRowUsesTheParent(t *testing.T) {
	parent, a, fix := uuid.New(), uuid.New(), uuid.New()
	q := planRowsQuerier{rows: [][2]string{
		{a.String(), `{"criteria":[` + crit("AC-1", "a works") + `]}`},
	}}
	req, err := LoadRequirementsForTask(context.Background(), q, parent, fix)
	if err != nil {
		t.Fatal(err)
	}
	if len(req.Criteria) != 1 || req.Criteria[0].ID != "AC-1" {
		t.Fatalf("criteria = %+v, want the parent's", req.Criteria)
	}
}
