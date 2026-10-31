package rollout

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// intakeDB answers the two lookups a bare rollout_id intake makes.
type intakeDB struct {
	status  string
	scratch bool
}

func (d intakeDB) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, errors.New("intakeDB: Exec unused")
}

func (d intakeDB) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, errors.New("intakeDB: Query unused")
}

func (d intakeDB) QueryRow(_ context.Context, sql string, _ ...any) pgx.Row {
	switch {
	case strings.Contains(sql, "FROM rollouts"):
		return scanFunc(func(dest ...any) error {
			*dest[0].(*string) = d.status
			c, b := "c", "b"
			*dest[1].(**string) = &c
			*dest[2].(**string) = &b
			return nil
		})
	case strings.Contains(sql, "FROM products"):
		return scanFunc(func(dest ...any) error {
			*dest[0].(*bool) = d.scratch
			return nil
		})
	}
	return scanFunc(func(...any) error { return pgx.ErrNoRows })
}

type scanFunc func(dest ...any) error

func (f scanFunc) Scan(dest ...any) error { return f(dest...) }

// A bare rollout_id removes the item from production outcomes, so it gets
// the same conditions as a snapshot: a live rollout and a scratch product.
func TestResolveShadowIntake_BareRolloutID(t *testing.T) {
	ctx := context.Background()
	meta := map[string]any{MetaRolloutID: uuid.New().String()}
	for _, tc := range []struct {
		name    string
		db      intakeDB
		code    string
		allowed bool
	}{
		{"live rollout on a scratch product", intakeDB{StatusShadow, true}, "", true},
		{"non-scratch product", intakeDB{StatusShadow, false}, "rollout_needs_scratch_product", false},
		{"approved rollout", intakeDB{StatusApproved, true}, "rollout_not_in_shadow", false},
		{"blocked rollout", intakeDB{StatusBlocked, true}, "rollout_not_in_shadow", false},
	} {
		s, err := ResolveShadowIntake(ctx, tc.db, "benchmark", meta, uuid.New())
		if tc.allowed {
			if err != nil || s == nil || s.RolloutID == nil {
				t.Errorf("%s: %+v %v", tc.name, s, err)
			}
			continue
		}
		var ie *IntakeError
		if !errors.As(err, &ie) || ie.Code != tc.code || ie.Status != http.StatusBadRequest {
			t.Errorf("%s: %v, want 400 %s", tc.name, err, tc.code)
		}
	}
}
