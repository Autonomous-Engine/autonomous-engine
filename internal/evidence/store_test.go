package evidence

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// insertColumns are the columns insertSQL writes, in order. They must match
// migration 044 and the order of insertArgs.
var insertColumns = []string{
	"id", "work_item_id", "task_id", "repository_id", "schema_version",
	"base_sha", "head_sha", "branch", "outcome", "evidence",
}

func TestInsertArgs_MapsColumnsInOrder(t *testing.T) {
	ev := sampleEvidence(t)
	id := uuid.New()

	args, err := insertArgs(id, &ev)
	if err != nil {
		t.Fatalf("insertArgs: %v", err)
	}
	if len(args) != len(insertColumns) {
		t.Fatalf("insertArgs returned %d args, insertSQL has %d columns", len(args), len(insertColumns))
	}
	if got := args[0].(uuid.UUID); got != id {
		t.Errorf("id = %v, want %v", got, id)
	}
	if got := args[1].(uuid.UUID); got.String() != ev.WorkID {
		t.Errorf("work_item_id = %v, want %v", got, ev.WorkID)
	}
	if got := args[2].(string); got != ev.TaskID {
		t.Errorf("task_id = %q, want %q", got, ev.TaskID)
	}
	repo := args[3].(*uuid.UUID)
	if repo == nil || repo.String() != *ev.RepositoryID {
		t.Errorf("repository_id = %v, want %v", repo, *ev.RepositoryID)
	}
	if got := args[4].(int); got != SchemaVersion {
		t.Errorf("schema_version = %d, want %d", got, SchemaVersion)
	}
	if args[5].(string) != ev.BaseSHA || args[6].(string) != ev.HeadSHA || args[7].(string) != ev.Branch {
		t.Errorf("sha/branch columns = %v %v %v", args[5], args[6], args[7])
	}
	if got := args[8].(string); got != string(ev.Outcome) {
		t.Errorf("outcome = %q, want %q", got, ev.Outcome)
	}
	payload := args[9].([]byte)
	if !json.Valid(payload) {
		t.Fatalf("evidence column is not valid JSON: %s", payload)
	}
	parsed, err := Parse(payload)
	if err != nil {
		t.Fatalf("evidence column does not parse back: %v", err)
	}
	if !reflect.DeepEqual(*parsed, ev) {
		t.Errorf("evidence column round trip mismatch:\n got=%+v\nwant=%+v", *parsed, ev)
	}
}

func TestInsertArgs_NullRepositoryID(t *testing.T) {
	for name, repo := range map[string]*string{"nil": nil, "empty": new(string)} {
		t.Run(name, func(t *testing.T) {
			ev := sampleEvidence(t)
			ev.RepositoryID = repo
			args, err := insertArgs(uuid.New(), &ev)
			if err != nil {
				t.Fatalf("insertArgs: %v", err)
			}
			if got := args[3].(*uuid.UUID); got != nil {
				t.Errorf("repository_id = %v, want nil (SQL NULL)", got)
			}
		})
	}
}

func TestInsertArgs_RejectsBadInput(t *testing.T) {
	t.Run("nil evidence", func(t *testing.T) {
		if _, err := insertArgs(uuid.New(), nil); err == nil {
			t.Error("accepted nil evidence")
		}
	})
	t.Run("invalid evidence is rejected before the database", func(t *testing.T) {
		ev := sampleEvidence(t)
		ev.Outcome = OutcomeVerified // inconsistent with the skipped check
		if _, err := insertArgs(uuid.New(), &ev); err == nil || !strings.Contains(err.Error(), "invalid evidence") {
			t.Errorf("error = %v, want invalid evidence", err)
		}
	})
	t.Run("unknown schema version", func(t *testing.T) {
		ev := sampleEvidence(t)
		ev.SchemaVersion = 2
		if _, err := insertArgs(uuid.New(), &ev); !errors.Is(err, ErrUnknownSchemaVersion) {
			t.Errorf("error = %v, want ErrUnknownSchemaVersion", err)
		}
	})
	t.Run("work_id not a uuid", func(t *testing.T) {
		ev := sampleEvidence(t)
		ev.WorkID = "WORK-123"
		if _, err := insertArgs(uuid.New(), &ev); err == nil || !strings.Contains(err.Error(), "work_id") {
			t.Errorf("error = %v, want work_id complaint", err)
		}
	})
	t.Run("repository_id not a uuid", func(t *testing.T) {
		ev := sampleEvidence(t)
		bad := "repo-1"
		ev.RepositoryID = &bad
		if _, err := insertArgs(uuid.New(), &ev); err == nil || !strings.Contains(err.Error(), "repository_id") {
			t.Errorf("error = %v, want repository_id complaint", err)
		}
	})
}

// fakeScanner stands in for a pgx row: it copies the given values into the
// scan destinations in order.
type fakeScanner struct {
	values []any
	err    error
}

func (f fakeScanner) Scan(dest ...any) error {
	if f.err != nil {
		return f.err
	}
	if len(dest) != len(f.values) {
		return errors.New("fakeScanner: destination count mismatch")
	}
	for i, d := range dest {
		switch d := d.(type) {
		case *uuid.UUID:
			*d = f.values[i].(uuid.UUID)
		case *time.Time:
			*d = f.values[i].(time.Time)
		case *[]byte:
			*d = f.values[i].([]byte)
		default:
			return errors.New("fakeScanner: unsupported destination type")
		}
	}
	return nil
}

func TestScanVerification_MapsRow(t *testing.T) {
	ev := sampleEvidence(t)
	payload, _ := json.Marshal(ev)
	id := uuid.New()
	created := time.Date(2026, 9, 9, 13, 0, 0, 0, time.UTC)

	v, err := scanVerification(fakeScanner{values: []any{id, created, payload}})
	if err != nil {
		t.Fatalf("scanVerification: %v", err)
	}
	if v.ID != id || !v.CreatedAt.Equal(created) {
		t.Errorf("identity = %v/%v, want %v/%v", v.ID, v.CreatedAt, id, created)
	}
	if !reflect.DeepEqual(v.Evidence, ev) {
		t.Errorf("evidence mismatch:\n got=%+v\nwant=%+v", v.Evidence, ev)
	}
}

func TestScanVerification_NoRowsIsErrNotFound(t *testing.T) {
	_, err := scanVerification(fakeScanner{err: pgx.ErrNoRows})
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("error = %v, want ErrNotFound", err)
	}
}

func TestScanVerification_RejectsUnknownStoredVersion(t *testing.T) {
	ev := sampleEvidence(t)
	ev.SchemaVersion = 2
	payload, _ := json.Marshal(ev)
	_, err := scanVerification(fakeScanner{values: []any{uuid.New(), time.Now(), payload}})
	if !errors.Is(err, ErrUnknownSchemaVersion) {
		t.Errorf("error = %v, want ErrUnknownSchemaVersion", err)
	}
}

func TestScanVerification_RejectsCorruptPayload(t *testing.T) {
	_, err := scanVerification(fakeScanner{values: []any{uuid.New(), time.Now(), []byte(`{"schema_version":`)}})
	if err == nil {
		t.Error("accepted corrupt evidence JSON")
	}
}

// A Store with a nil pool panics on any query, so these prove the guards
// return before touching the database.
func TestStore_GetLatestByHeadSHA_EmptySHAIsNotFoundWithoutQuery(t *testing.T) {
	s := NewStore(nil)
	v, err := s.GetLatestByHeadSHA(context.Background(), "")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("error = %v, want ErrNotFound", err)
	}
	if v != nil {
		t.Errorf("verification = %+v, want nil", v)
	}
}

func TestStore_GetByWorkItem_NilUUIDIsNotFoundWithoutQuery(t *testing.T) {
	s := NewStore(nil)
	v, err := s.GetByWorkItem(context.Background(), uuid.Nil)
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("error = %v, want ErrNotFound", err)
	}
	if v != nil {
		t.Errorf("verifications = %+v, want nil", v)
	}
}

var placeholderRe = regexp.MustCompile(`\$(\d+)`)

func TestSQLText(t *testing.T) {
	t.Run("insert targets every column once, in order", func(t *testing.T) {
		if !strings.Contains(insertSQL, "INSERT INTO implementation_verifications") {
			t.Fatalf("insertSQL does not target implementation_verifications: %s", insertSQL)
		}
		open := strings.Index(insertSQL, "(")
		closeIdx := strings.Index(insertSQL, ")")
		var cols []string
		for _, c := range strings.Split(insertSQL[open+1:closeIdx], ",") {
			cols = append(cols, strings.TrimSpace(c))
		}
		if !reflect.DeepEqual(cols, insertColumns) {
			t.Errorf("insert columns = %v, want %v", cols, insertColumns)
		}
		matches := placeholderRe.FindAllStringSubmatch(insertSQL, -1)
		if len(matches) != len(insertColumns) {
			t.Errorf("insertSQL has %d placeholders, want %d", len(matches), len(insertColumns))
		}
		for i, m := range matches {
			if want := i + 1; m[1] != strconv.Itoa(want) {
				t.Errorf("placeholder %d is $%s, want $%d", i, m[1], want)
			}
		}
		if !strings.Contains(insertSQL, "$10::jsonb") {
			t.Errorf("evidence must be cast to jsonb: %s", insertSQL)
		}
	})
	t.Run("select by work item filters and orders newest first", func(t *testing.T) {
		if !strings.Contains(selectByWorkItemSQL, "WHERE work_item_id = $1") {
			t.Errorf("missing work_item_id filter: %s", selectByWorkItemSQL)
		}
		if !strings.Contains(selectByWorkItemSQL, "ORDER BY created_at DESC") {
			t.Errorf("missing newest-first ordering: %s", selectByWorkItemSQL)
		}
	})
	t.Run("select latest by head sha filters, orders and limits", func(t *testing.T) {
		for _, frag := range []string{"WHERE head_sha = $1", "ORDER BY created_at DESC", "LIMIT 1"} {
			if !strings.Contains(selectLatestByHeadSHASQL, frag) {
				t.Errorf("missing %q: %s", frag, selectLatestByHeadSHASQL)
			}
		}
	})
	t.Run("selects read exactly the columns scanVerification scans", func(t *testing.T) {
		if !strings.HasPrefix(strings.TrimSpace(selectCols), "SELECT id, created_at, evidence FROM implementation_verifications") {
			t.Errorf("selectCols = %q", selectCols)
		}
	})
}

const migrationVersion = "044_implementation_verifications"

func migrationsDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs(filepath.Join("..", "..", "migrations"))
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func readMigration(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(migrationsDir(t), name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(data)
}

// The migration runner (internal/db/migrate.go) picks up every *.up.sql in
// lexical order and records the filename minus ".up.sql" as the version.
// These checks pin the properties 044 relies on.
func TestMigration044_FollowsRunnerAndCLAUDEmdRules(t *testing.T) {
	up := readMigration(t, migrationVersion+".up.sql")
	down := readMigration(t, migrationVersion+".down.sql")

	t.Run("runner ordering", func(t *testing.T) {
		entries, err := os.ReadDir(migrationsDir(t))
		if err != nil {
			t.Fatal(err)
		}
		var ups []string
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), ".up.sql") {
				ups = append(ups, e.Name())
			}
		}
		sort.Strings(ups)
		idx043, idx044 := -1, -1
		for i, name := range ups {
			switch {
			case strings.HasPrefix(name, "043_"):
				idx043 = i
			case name == migrationVersion+".up.sql":
				idx044 = i
			}
		}
		if idx043 < 0 || idx044 < 0 {
			t.Fatalf("043 (%d) or 044 (%d) not found among %v", idx043, idx044, ups)
		}
		if idx044 <= idx043 {
			t.Errorf("044 must sort after 043 in the runner's lexical order; got 043@%d 044@%d", idx043, idx044)
		}
		// The runner records filepath.Base(path) minus ".up.sql" as the version.
		if got := strings.TrimSuffix(filepath.Base(ups[idx044]), ".up.sql"); got != migrationVersion {
			t.Errorf("runner would derive version %q, want %q", got, migrationVersion)
		}
		if strings.Contains(up, "ALTER TYPE") {
			t.Errorf("044 must not need the runner's ALTER TYPE extraction")
		}
	})

	t.Run("up is re-runnable and names its constraints", func(t *testing.T) {
		for _, frag := range []string{
			"CREATE TABLE IF NOT EXISTS implementation_verifications",
			"CREATE INDEX IF NOT EXISTS idx_implementation_verifications_work_item",
			"CREATE INDEX IF NOT EXISTS idx_implementation_verifications_head_sha",
			"CONSTRAINT implementation_verifications_work_item_id_fkey",
			"REFERENCES work_items(id) ON DELETE CASCADE",
			"CONSTRAINT implementation_verifications_repository_id_fkey",
			"REFERENCES repositories(id) ON DELETE SET NULL",
			"CONSTRAINT implementation_verifications_outcome_check",
			"evidence       JSONB       NOT NULL",
		} {
			if !strings.Contains(up, frag) {
				t.Errorf("up migration missing %q", frag)
			}
		}
		// Every REFERENCES must carry an ON DELETE clause (CLAUDE.md rule 3).
		for _, line := range strings.Split(up, "\n") {
			if strings.Contains(line, "REFERENCES") && !strings.Contains(line, "ON DELETE") {
				t.Errorf("bare REFERENCES without ON DELETE: %q", strings.TrimSpace(line))
			}
		}
	})

	t.Run("up declares every column the store writes", func(t *testing.T) {
		for _, col := range append(insertColumns, "created_at") {
			if !regexp.MustCompile(`(?m)^\s+` + col + `\s`).MatchString(up) {
				t.Errorf("up migration does not declare column %q", col)
			}
		}
		for _, o := range []Outcome{OutcomeVerified, OutcomePartiallyVerified, OutcomeFailed, OutcomeUnverified} {
			if !strings.Contains(up, "'"+string(o)+"'") {
				t.Errorf("outcome CHECK constraint missing %q", o)
			}
		}
	})

	t.Run("down is guarded and complete", func(t *testing.T) {
		for _, frag := range []string{
			"DROP CONSTRAINT IF EXISTS implementation_verifications_outcome_check",
			"DROP CONSTRAINT IF EXISTS implementation_verifications_repository_id_fkey",
			"DROP CONSTRAINT IF EXISTS implementation_verifications_work_item_id_fkey",
			"DROP INDEX IF EXISTS idx_implementation_verifications_head_sha",
			"DROP INDEX IF EXISTS idx_implementation_verifications_work_item",
			"DROP TABLE IF EXISTS implementation_verifications",
		} {
			if !strings.Contains(down, frag) {
				t.Errorf("down migration missing %q", frag)
			}
		}
	})
}

const migration045Version = "045_products_partial_verification_opt_in"

// TestMigration045_FollowsRunnerAndCLAUDEmdRules pins the properties the D2
// opt-in column relies on: it sorts after 044 in the runner's lexical order,
// needs no ALTER TYPE extraction, and both directions are guarded so a
// partially-applied run can be re-run.
func TestMigration045_FollowsRunnerAndCLAUDEmdRules(t *testing.T) {
	up := readMigration(t, migration045Version+".up.sql")
	down := readMigration(t, migration045Version+".down.sql")

	t.Run("runner ordering", func(t *testing.T) {
		entries, err := os.ReadDir(migrationsDir(t))
		if err != nil {
			t.Fatal(err)
		}
		var ups []string
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), ".up.sql") {
				ups = append(ups, e.Name())
			}
		}
		sort.Strings(ups)
		idx044, idx045 := -1, -1
		for i, name := range ups {
			switch name {
			case migrationVersion + ".up.sql":
				idx044 = i
			case migration045Version + ".up.sql":
				idx045 = i
			}
		}
		if idx044 < 0 || idx045 < 0 {
			t.Fatalf("044 (%d) or 045 (%d) not found among %v", idx044, idx045, ups)
		}
		if idx045 <= idx044 {
			t.Errorf("045 must sort after 044 in the runner's lexical order; got 044@%d 045@%d", idx044, idx045)
		}
		if got := strings.TrimSuffix(filepath.Base(ups[idx045]), ".up.sql"); got != migration045Version {
			t.Errorf("runner would derive version %q, want %q", got, migration045Version)
		}
		if strings.Contains(up, "ALTER TYPE") || strings.Contains(down, "ALTER TYPE") {
			t.Errorf("045 must not need the runner's ALTER TYPE extraction")
		}
		if strings.Contains(up, "REFERENCES") {
			t.Errorf("045 is a single boolean column and must not add a foreign key")
		}
	})

	t.Run("up is guarded and additive with a safe default", func(t *testing.T) {
		want := regexp.MustCompile(`(?s)ALTER TABLE products\s+ADD COLUMN IF NOT EXISTS partial_verification_opt_in BOOLEAN NOT NULL DEFAULT false;`)
		if !want.MatchString(up) {
			t.Errorf("up migration does not contain the guarded ADD COLUMN with NOT NULL DEFAULT false:\n%s", up)
		}
		if strings.Contains(up, "DROP") {
			t.Errorf("up migration must be purely additive")
		}
	})

	t.Run("down is guarded", func(t *testing.T) {
		want := regexp.MustCompile(`(?s)ALTER TABLE IF EXISTS products\s+DROP COLUMN IF EXISTS partial_verification_opt_in;`)
		if !want.MatchString(down) {
			t.Errorf("down migration does not contain the guarded DROP COLUMN:\n%s", down)
		}
	})
}
