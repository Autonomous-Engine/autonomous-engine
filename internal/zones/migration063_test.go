package zones

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Migration 063 follows the CLAUDE.md rules: guarded DDL, named constraints
// dropped before they are added, a down that removes what the up adds, and
// the global UNIQUE(name) of migration 003 dropped by its real name.
func TestMigration063FollowsCLAUDEmdRules(t *testing.T) {
	read := func(name string) string {
		b, err := os.ReadFile(filepath.Join("..", "..", "migrations", name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		return strings.ReplaceAll(string(b), "\r\n", "\n")
	}
	up := read("063_code_zones_per_repository.up.sql")
	down := read("063_code_zones_per_repository.down.sql")

	for _, kw := range []string{"CREATE TABLE", "CREATE INDEX", "CREATE UNIQUE INDEX", "ADD COLUMN"} {
		if all, guarded := strings.Count(up, kw), strings.Count(up, kw+" IF NOT EXISTS"); all != guarded {
			t.Errorf("%d of %d %s statements lack IF NOT EXISTS", all-guarded, all, kw)
		}
	}
	if regexp.MustCompile(`(?i)REFERENCES`).MatchString(up) {
		t.Error("063 adds no foreign key (repository_id's ON DELETE CASCADE is migration 009's)")
	}
	for _, m := range regexp.MustCompile(`ADD CONSTRAINT (\w+)`).FindAllStringSubmatch(up, -1) {
		if !strings.Contains(up, "DROP CONSTRAINT IF EXISTS "+m[1]+";") {
			t.Errorf("constraint %s added without a preceding DROP CONSTRAINT IF EXISTS", m[1])
		}
	}
	for _, frag := range []string{
		"DROP CONSTRAINT IF EXISTS code_zones_name_key;",
		"ADD CONSTRAINT code_zones_repository_name_key UNIQUE (repository_id, name)",
		"CREATE TYPE zone_policy AS ENUM ('autonomous', 'human_review', 'escalate')",
	} {
		if !strings.Contains(up, frag) {
			t.Errorf("up migration lacks %q", frag)
		}
	}
	for _, frag := range []string{
		"DROP CONSTRAINT IF EXISTS code_zones_repository_name_key",
		"ADD CONSTRAINT code_zones_name_key UNIQUE (name)",
		"DROP CONSTRAINT IF EXISTS code_zones_source_check",
		"DROP COLUMN IF EXISTS patterns",
		"DROP COLUMN IF EXISTS policy",
		"DROP COLUMN IF EXISTS description",
		"DROP COLUMN IF EXISTS source",
		"DROP COLUMN IF EXISTS updated_at",
		"DROP TYPE IF EXISTS zone_policy",
		"DROP COLUMN IF EXISTS zones_confirmed_at",
	} {
		if !strings.Contains(down, frag) {
			t.Errorf("down migration lacks %q", frag)
		}
	}
	if regexp.MustCompile(`(?im)^ALTER\s+TYPE`).MatchString(up) {
		t.Error("063 must not need the runner's ALTER TYPE extraction")
	}
	// The real name of 003's inline UNIQUE on code_zones.name.
	m003 := read("003_zones_external_changes.up.sql")
	if !strings.Contains(m003, "name          TEXT        NOT NULL UNIQUE") {
		t.Error("migration 003 no longer declares the inline UNIQUE(name) whose default name is code_zones_name_key")
	}
}
