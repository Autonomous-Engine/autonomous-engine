package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func readMigration048(t *testing.T, suffix string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "migrations", "048_plan_tasks."+suffix))
	if err != nil {
		t.Fatalf("read migration 048 %s: %v", suffix, err)
	}
	return string(b)
}

// TestMigration048_FollowsCLAUDEmdRules checks the CLAUDE.md migration rules
// on 048 without a database: guarded DDL, explicit ON DELETE on every foreign
// key, named constraints dropped before they are added, and a down migration.
func TestMigration048_FollowsCLAUDEmdRules(t *testing.T) {
	up := readMigration048(t, "up.sql")
	down := readMigration048(t, "down.sql")

	for _, frag := range []string{
		"CREATE TABLE IF NOT EXISTS plan_tasks",
		"CREATE TABLE IF NOT EXISTS plan_task_dependencies",
		"CREATE INDEX IF NOT EXISTS idx_plan_tasks_parent_status",
		"ON plan_tasks (parent_work_id, status)",
		"CHECK (status IN ('held', 'released', 'completed', 'failed'))",
		"PRIMARY KEY (task_id, depends_on_task_id)",
		"FOREIGN KEY (task_id) REFERENCES plan_tasks(id) ON DELETE CASCADE",
		"FOREIGN KEY (depends_on_task_id) REFERENCES plan_tasks(id) ON DELETE CASCADE",
	} {
		if !strings.Contains(up, frag) {
			t.Errorf("up migration missing %q", frag)
		}
	}

	refs := regexp.MustCompile(`REFERENCES\s+\w+\(\w+\)([^;]*);`).FindAllStringSubmatch(up, -1)
	if len(refs) == 0 {
		t.Fatal("no foreign keys found")
	}
	for _, m := range refs {
		if !strings.Contains(m[1], "ON DELETE") {
			t.Errorf("foreign key without ON DELETE: %s", m[0])
		}
	}

	added := regexp.MustCompile(`ADD CONSTRAINT (\w+)`).FindAllStringSubmatch(up, -1)
	for _, m := range added {
		if !strings.Contains(up, "DROP CONSTRAINT IF EXISTS "+m[1]+";") {
			t.Errorf("constraint %s is added without a preceding DROP CONSTRAINT IF EXISTS", m[1])
		}
	}
	if strings.Contains(up, "ALTER TYPE") {
		t.Error("048 must not need the runner's ALTER TYPE extraction")
	}

	for _, frag := range []string{"DROP TABLE IF EXISTS plan_task_dependencies", "DROP TABLE IF EXISTS plan_tasks"} {
		if !strings.Contains(down, frag) {
			t.Errorf("down migration missing %q", frag)
		}
	}
	if strings.Index(down, "plan_task_dependencies;") > strings.Index(down, "DROP TABLE IF EXISTS plan_tasks;") {
		t.Error("down migration must drop plan_task_dependencies before plan_tasks")
	}
}
