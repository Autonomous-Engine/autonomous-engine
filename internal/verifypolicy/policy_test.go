package verifypolicy

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/evidence"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestDefaultIsFailClosed(t *testing.T) {
	id := uuid.New()
	p := Default(id)
	if p.NoChecksPolicy != NoChecksBlock || p.Source != SourceDefault || p.RepositoryID != id {
		t.Fatalf("default = %+v, want block/default for %s", p, id)
	}
	if !reflect.DeepEqual(p.RequiredChecks, []evidence.CheckKind{evidence.CheckKindTest}) {
		t.Fatalf("default required_checks = %v, want [test]", p.RequiredChecks)
	}
	if !reflect.DeepEqual(p.SecurityReviewPaths, GenericSecurityReviewPaths) {
		t.Fatalf("default security paths = %v, want the generic list", p.SecurityReviewPaths)
	}
	// The default must not alias the package list.
	p.SecurityReviewPaths[0] = "mutated"
	if GenericSecurityReviewPaths[0] == "mutated" {
		t.Fatal("Default aliases GenericSecurityReviewPaths")
	}
	if err := Default(id).Validate(); err != nil {
		t.Fatalf("default does not validate: %v", err)
	}
}

func TestNormalizeAndValidate(t *testing.T) {
	blank := "   "
	cmd := "  npm test  "
	p := Policy{
		TestCommand:         &cmd,
		RequiredChecks:      []evidence.CheckKind{" Test", "lint", "test", ""},
		SecurityReviewPaths: []string{" auth* ", "auth*", "", `db\migrations\**`},
		Source:              SourceAdmin,
	}.Normalize()
	if p.TestCommand == nil || *p.TestCommand != "npm test" {
		t.Fatalf("test_command = %v, want trimmed npm test", p.TestCommand)
	}
	if !reflect.DeepEqual(p.RequiredChecks, []evidence.CheckKind{"test", "lint"}) {
		t.Fatalf("required_checks = %v", p.RequiredChecks)
	}
	if !reflect.DeepEqual(p.SecurityReviewPaths, []string{"auth*", "db/migrations/**"}) {
		t.Fatalf("security paths = %v", p.SecurityReviewPaths)
	}
	if p.NoChecksPolicy != NoChecksBlock {
		t.Fatalf("empty no_checks_policy normalised to %q, want block", p.NoChecksPolicy)
	}
	if err := p.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if q := (Policy{TestCommand: &blank, Source: SourceAdmin}).Normalize(); q.TestCommand != nil {
		t.Fatalf("blank override kept: %q", *q.TestCommand)
	}

	bad := []struct {
		name string
		p    Policy
		want string
	}{
		{"no_checks_policy", Policy{NoChecksPolicy: "allow", Source: SourceAdmin}, "no_checks_policy"},
		{"diagnostic not selectable", Policy{NoChecksPolicy: NoChecksBlock, RequiredChecks: []evidence.CheckKind{"diagnostic"}, Source: SourceAdmin}, "required_checks"},
		{"unknown kind", Policy{NoChecksPolicy: NoChecksBlock, RequiredChecks: []evidence.CheckKind{"typecheck"}, Source: SourceAdmin}, "required_checks"},
		{"bad glob", Policy{NoChecksPolicy: NoChecksBlock, SecurityReviewPaths: []string{"auth["}, Source: SourceAdmin}, "malformed glob"},
		{"bad source", Policy{NoChecksPolicy: NoChecksBlock, Source: "api"}, "source"},
		{"multi-line command", Policy{NoChecksPolicy: NoChecksBlock, TestCommand: strPtr("npm test\nrm -rf /"), Source: SourceAdmin}, "single line"},
	}
	for _, tc := range bad {
		if err := tc.p.Validate(); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: Validate = %v, want error containing %q", tc.name, err, tc.want)
		}
	}
}

func strPtr(s string) *string { return &s }

func TestEffectiveTestCommand(t *testing.T) {
	override := " make check "
	empty := ""
	cases := []struct {
		name     string
		p        Policy
		detected string
		want     string
	}{
		{"no override uses detection", Policy{}, "npm test", "npm test"},
		{"no override, nothing detected", Policy{}, "", ""},
		{"override wins over detection", Policy{TestCommand: &override}, "npm test", "make check"},
		{"override supplies a command", Policy{TestCommand: &override}, "", "make check"},
		{"blank override ignored", Policy{TestCommand: &empty}, "go test ./...", "go test ./..."},
	}
	for _, tc := range cases {
		if got := tc.p.EffectiveTestCommand(tc.detected); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestMatchPath(t *testing.T) {
	cases := []struct {
		pattern, file string
		want          bool
	}{
		{"auth*", "internal/auth/login.go", true},
		{"auth*", "src/AuthService.ts", true},
		{"auth*", "src/oauth.go", false},
		{"*credential*", "internal/credentials/store.go", true},
		{"*secret*", "config/secrets.yaml", true},
		{"crypto*", "internal/crypto/aes.go", true},
		{"sandbox*", "cmd/sandbox-runner/main.go", true},
		{"migrations/**", "migrations/051_x.up.sql", true},
		{"migrations/**", "db/migrations/2024/001.sql", true},
		{"migrations/**", "docs/migration-guide.md", false},
		{"go.mod", "go.mod", true},
		{"go.mod", "tools/go.mod", true},
		{"go.mod", "go.sum", false},
		{"requirements*.txt", "requirements-dev.txt", true},
		{"src/**/*.sql", "src/a/b/c.sql", true},
		{"src/**/*.sql", "lib/a/c.sql", false},
		{".github/workflows/*", `.github\workflows\ci.yml`, true},
		{"**", "anything/at/all", true},
		{"", "x", false},
		{"auth[", "auth[", false},
	}
	for _, tc := range cases {
		if got := MatchPath(tc.pattern, tc.file); got != tc.want {
			t.Errorf("MatchPath(%q, %q) = %v, want %v", tc.pattern, tc.file, got, tc.want)
		}
	}
}

func TestSecurityReviewMatches(t *testing.T) {
	p := Default(uuid.New())
	got := p.SecurityReviewMatches([]string{"README.md", "internal/auth/x.go", "migrations/1.sql", "internal/auth/x.go", "src/app.ts"})
	if !reflect.DeepEqual(got, []string{"internal/auth/x.go", "migrations/1.sql"}) {
		t.Fatalf("matches = %v", got)
	}
	if m := (Policy{}).SecurityReviewMatches([]string{"internal/auth/x.go"}); len(m) != 0 {
		t.Fatalf("empty path list matched %v", m)
	}
}

func TestSuggestedPathsAndLanguage(t *testing.T) {
	dir := t.TempDir()
	if lang := DetectLanguage(dir); lang != "" {
		t.Fatalf("empty dir language = %q", lang)
	}
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if lang := DetectLanguage(dir); lang != "go" {
		t.Fatalf("language = %q, want go", lang)
	}
	got := SuggestedSecurityReviewPaths("go")
	if !reflect.DeepEqual(got[:len(GenericSecurityReviewPaths)], GenericSecurityReviewPaths) {
		t.Fatalf("suggested %v must start with the generic list", got)
	}
	if !strings.Contains(strings.Join(got, ","), "go.sum") {
		t.Fatalf("suggested %v must include go.sum (dependency provenance)", got)
	}
	if got := SuggestedSecurityReviewPaths(""); !reflect.DeepEqual(got, GenericSecurityReviewPaths) {
		t.Fatalf("unknown language suggested %v", got)
	}
}

// fakeRow / fakeQuerier drive LoadVerificationPolicy without a database.
type fakeRow struct {
	vals []any
	err  error
}

func (r fakeRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	for i, d := range dest {
		switch p := d.(type) {
		case **string:
			*p, _ = r.vals[i].(*string)
		case *[]string:
			*p, _ = r.vals[i].([]string)
		case *string:
			*p, _ = r.vals[i].(string)
		case **time.Time:
			*p, _ = r.vals[i].(*time.Time)
		default:
			return errors.New("unexpected scan target")
		}
	}
	return nil
}

type fakeQuerier struct{ row fakeRow }

func (q fakeQuerier) QueryRow(context.Context, string, ...any) pgx.Row { return q.row }

func TestLoadVerificationPolicyFailsClosed(t *testing.T) {
	ctx := context.Background()
	id := uuid.New()

	p, found, err := LoadVerificationPolicy(ctx, fakeQuerier{fakeRow{err: pgx.ErrNoRows}}, id)
	if err != nil || found || !reflect.DeepEqual(p, Default(id)) {
		t.Fatalf("no row: (%+v, %v, %v), want default, not found, nil", p, found, err)
	}
	p, found, err = LoadVerificationPolicy(ctx, fakeQuerier{fakeRow{err: errors.New("boom")}}, id)
	if err == nil || found || p.NoChecksPolicy != NoChecksBlock {
		t.Fatalf("query error: (%+v, %v, %v), want default block plus error", p, found, err)
	}
	p, found, err = (*Store)(nil).LoadVerificationPolicy(ctx, id)
	if err == nil || found || p.NoChecksPolicy != NoChecksBlock {
		t.Fatalf("nil store: (%+v, %v, %v), want default block plus error", p, found, err)
	}

	cmd := "make test"
	at := time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC)
	row := fakeRow{vals: []any{&cmd, []string{"test", "lint"}, "partial_with_human_review", []string{"auth*"}, "alice", &at, "admin"}}
	p, found, err = LoadVerificationPolicy(ctx, fakeQuerier{row}, id)
	if err != nil || !found {
		t.Fatalf("stored row: found=%v err=%v", found, err)
	}
	want := Policy{
		RepositoryID: id, TestCommand: &cmd,
		RequiredChecks:      []evidence.CheckKind{"test", "lint"},
		NoChecksPolicy:      NoChecksPartialWithHumanReview,
		SecurityReviewPaths: []string{"auth*"},
		SetBy:               "alice", SetAt: &at, Source: SourceAdmin,
	}
	if !reflect.DeepEqual(p, want) {
		t.Fatalf("loaded %+v, want %+v", p, want)
	}

	// A stored row that no longer validates (e.g. a hand-edited value)
	// falls back to the default rather than being trusted.
	row = fakeRow{vals: []any{(*string)(nil), []string{"typecheck"}, "partial_with_human_review", []string{}, "", (*time.Time)(nil), "admin"}}
	p, found, err = LoadVerificationPolicy(ctx, fakeQuerier{row}, id)
	if err == nil || found || p.NoChecksPolicy != NoChecksBlock {
		t.Fatalf("invalid row: (%+v, %v, %v), want default block plus error", p, found, err)
	}
}

type recordingExecer struct {
	sql  string
	args []any
	err  error
}

func (e *recordingExecer) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	e.sql, e.args = sql, args
	return pgconn.CommandTag{}, e.err
}

func TestUpsertTxValidatesAndNormalises(t *testing.T) {
	ctx := context.Background()
	ex := &recordingExecer{}
	if _, err := UpsertTx(ctx, ex, Policy{Source: SourceAdmin}); err == nil {
		t.Fatal("upsert without repository id succeeded")
	}
	id := uuid.New()
	if _, err := UpsertTx(ctx, ex, Policy{RepositoryID: id, NoChecksPolicy: "yolo", Source: SourceAdmin}); err == nil {
		t.Fatal("upsert with an invalid no_checks_policy succeeded")
	}
	if ex.sql != "" {
		t.Fatal("an invalid policy reached the database")
	}
	cmd := " npm test "
	got, err := UpsertTx(ctx, ex, Policy{
		RepositoryID: id, TestCommand: &cmd,
		RequiredChecks: []evidence.CheckKind{"test", "test"},
		Source:         SourceOnboarding, SetBy: "bob",
	})
	if err != nil {
		t.Fatalf("UpsertTx: %v", err)
	}
	if got.NoChecksPolicy != NoChecksBlock || got.SetAt == nil || *got.TestCommand != "npm test" {
		t.Fatalf("stored form = %+v", got)
	}
	if !strings.Contains(ex.sql, "ON CONFLICT (repository_id)") {
		t.Fatalf("sql is not an upsert: %s", ex.sql)
	}
	if !reflect.DeepEqual(ex.args[2], []string{"test"}) || ex.args[3] != "block" || ex.args[7] != "onboarding" {
		t.Fatalf("args = %v", ex.args)
	}
	ex.err = errors.New("fk violation")
	if _, err := UpsertTx(ctx, ex, Policy{RepositoryID: id, Source: SourceAdmin}); err == nil {
		t.Fatal("exec error swallowed")
	}
}
