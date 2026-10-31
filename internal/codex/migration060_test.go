package codex

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Migration 060 follows the CLAUDE.md rules: guarded DDL, every foreign key
// with ON DELETE and a named constraint dropped before it is added, and a
// down migration that removes everything the up adds.
func TestMigration060FollowsCLAUDEmdRules(t *testing.T) {
	read := func(name string) string {
		b, err := os.ReadFile(filepath.Join("..", "..", "migrations", name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		return strings.ReplaceAll(string(b), "\r\n", "\n")
	}
	up := read("060_codex_coherence.up.sql")
	down := read("060_codex_coherence.down.sql")

	for _, kw := range []string{"CREATE TABLE", "CREATE INDEX", "CREATE UNIQUE INDEX", "ADD COLUMN"} {
		if all, guarded := strings.Count(up, kw), strings.Count(up, kw+" IF NOT EXISTS"); all != guarded {
			t.Errorf("%d of %d %s statements lack IF NOT EXISTS", all-guarded, all, kw)
		}
	}
	refs := regexp.MustCompile(`(?is)REFERENCES\s+\w+\s*\(\w+\)\s*(ON DELETE (?:CASCADE|SET NULL))?`)
	found := refs.FindAllStringSubmatch(up, -1)
	if len(found) != 2 {
		t.Errorf("%d foreign keys, want 2", len(found))
	}
	for _, m := range found {
		if m[1] == "" {
			t.Errorf("foreign key without ON DELETE: %q", m[0])
		}
	}
	for _, m := range regexp.MustCompile(`ADD CONSTRAINT (\w+)`).FindAllStringSubmatch(up, -1) {
		if !strings.Contains(up, "DROP CONSTRAINT IF EXISTS "+m[1]+";") {
			t.Errorf("constraint %s added without a preceding DROP CONSTRAINT IF EXISTS", m[1])
		}
	}
	for _, frag := range []string{
		"ROW_NUMBER() OVER (PARTITION BY product_id, branch ORDER BY version_number)",
		"FOREIGN KEY (work_item_id) REFERENCES work_items(id) ON DELETE CASCADE",
	} {
		if !strings.Contains(up, frag) {
			t.Errorf("up migration lacks %q", frag)
		}
	}
	for _, frag := range []string{
		"DROP TABLE IF EXISTS work_item_codex_pins",
		"DROP INDEX IF EXISTS work_item_codex_pins_product_branch_idx",
		"DROP INDEX IF EXISTS codex_versions_product_branch_version_key",
		"DROP CONSTRAINT IF EXISTS codex_versions_product_version_check",
		"DROP COLUMN IF EXISTS product_version",
	} {
		if !strings.Contains(down, frag) {
			t.Errorf("down migration lacks %q", frag)
		}
	}
	if strings.Contains(up, "ALTER TYPE") {
		t.Error("060 must not need the runner's ALTER TYPE extraction")
	}
}
