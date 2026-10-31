package codex

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

// Staleness is decided per pin: only the pinned modules that changed, and
// the plan-relevant section keys (which re-plan any pinned item), count.
func TestStalenessFor(t *testing.T) {
	cases := []struct {
		name         string
		pinned       []string
		changed      []string
		wantModules  []string
		wantSections []string
	}{
		{"intersecting module", []string{"auth", "billing"}, []string{"module:billing", "module:search"}, []string{"billing"}, nil},
		{"no intersection", []string{"auth"}, []string{"module:billing", "patterns", "anti_patterns"}, nil, nil},
		{"legacy bare value is a module", []string{"auth"}, []string{"auth"}, []string{"auth"}, nil},
		{"irrelevant section", []string{"auth"}, []string{"section:codebase_architecture", "section:team_notes"}, nil, nil},
		{"plan-relevant section re-plans any pin", []string{"auth"}, []string{"section:review_triggers"}, nil, []string{"review_triggers"}},
		{"plan-relevant section with no pinned modules", nil, []string{"section:risk_zones", "section:autonomous_scope"}, nil, []string{"autonomous_scope", "risk_zones"}},
		{"zones (Part 2)", []string{"x"}, []string{"section:zones", "module:x"}, []string{"x"}, []string{"zones"}},
		{"nothing changed", []string{"auth"}, nil, nil, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := StalenessFor(tc.pinned, tc.changed)
			if !reflect.DeepEqual(got.Modules, tc.wantModules) || !reflect.DeepEqual(got.Sections, tc.wantSections) {
				t.Fatalf("StalenessFor(%v, %v) = %+v, want modules %v sections %v", tc.pinned, tc.changed, got, tc.wantModules, tc.wantSections)
			}
			if want := len(tc.wantModules)+len(tc.wantSections) > 0; got.Stale() != want {
				t.Fatalf("Stale() = %t, want %t", got.Stale(), want)
			}
		})
	}
}

func TestStalenessDescribe(t *testing.T) {
	s := Staleness{Modules: []string{"auth", "billing"}, Sections: []string{"risk_zones"}}
	if got, want := s.Describe(12, 14), "modules auth, billing; sections risk_zones (v12 → v14)"; got != want {
		t.Fatalf("Describe = %q, want %q", got, want)
	}
	if got, want := (Staleness{Modules: []string{"x"}}).Describe(0, 1), "modules x (v0 → v1)"; got != want {
		t.Fatalf("Describe = %q, want %q", got, want)
	}
}

// Migration 062 follows the CLAUDE.md rules.
func TestMigration062FollowsCLAUDEmdRules(t *testing.T) {
	read := func(name string) string {
		b, err := os.ReadFile(filepath.Join("..", "..", "migrations", name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		return strings.ReplaceAll(string(b), "\r\n", "\n")
	}
	up := read("062_codex_replan.up.sql")
	down := read("062_codex_replan.down.sql")
	for _, kw := range []string{"CREATE TABLE", "CREATE INDEX", "ADD COLUMN"} {
		if all, guarded := strings.Count(up, kw), strings.Count(up, kw+" IF NOT EXISTS"); all != guarded {
			t.Errorf("%d of %d %s statements lack IF NOT EXISTS", all-guarded, all, kw)
		}
	}
	refs := regexp.MustCompile(`(?is)REFERENCES\s+\w+\s*\(\w+\)\s*(ON DELETE (?:CASCADE|SET NULL))?`)
	found := refs.FindAllStringSubmatch(up, -1)
	if len(found) != 4 {
		t.Errorf("%d foreign keys, want 4", len(found))
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
	for _, frag := range []string{"DROP TABLE IF EXISTS work_item_codex_events", "DROP TABLE IF EXISTS codex_replan_pending"} {
		if !strings.Contains(down, frag) {
			t.Errorf("down migration lacks %q", frag)
		}
	}
}
