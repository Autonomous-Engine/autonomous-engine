package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Migration 058 (research rounds) follows the CLAUDE.md rules: guarded DDL,
// every foreign key with ON DELETE, named constraints, and a down migration.
func TestMigration058_FollowsCLAUDEmdRules(t *testing.T) {
	read := func(name string) string {
		b, err := os.ReadFile(filepath.Join("..", "..", "migrations", name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		return strings.ReplaceAll(string(b), "\r\n", "\n")
	}
	up := read("058_research_rounds.up.sql")
	down := read("058_research_rounds.down.sql")

	for _, kw := range []string{"CREATE TABLE", "CREATE INDEX"} {
		if all, guarded := strings.Count(up, kw), strings.Count(up, kw+" IF NOT EXISTS"); all != guarded {
			t.Errorf("%d of %d %s statements lack IF NOT EXISTS", all-guarded, all, kw)
		}
	}
	refs := regexp.MustCompile(`(?is)REFERENCES\s+\w+\s*\(\w+\)\s*(ON DELETE (?:CASCADE|SET NULL))?`)
	for _, m := range refs.FindAllStringSubmatch(up, -1) {
		if m[1] == "" {
			t.Errorf("foreign key without ON DELETE: %q", m[0])
		}
	}
	named := regexp.MustCompile(`CONSTRAINT \w+_fkey\s+FOREIGN KEY`)
	if fks, n := strings.Count(up, "FOREIGN KEY"), len(named.FindAllString(up, -1)); fks == 0 || fks != n {
		t.Errorf("%d foreign keys, %d named *_fkey", fks, n)
	}
	for _, want := range []string{"DROP TABLE IF EXISTS research_round_tasks", "DROP TABLE IF EXISTS research_rounds"} {
		if !strings.Contains(down, want) {
			t.Errorf("down migration lacks %q", want)
		}
	}
	if strings.Contains(up, "ALTER TYPE") {
		t.Error("058 must not need the runner's ALTER TYPE extraction")
	}
}
