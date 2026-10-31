package main

import (
	"reflect"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/zones"
)

// Research zone lookup (codex zones, migration 063) matches affected files
// with the zone globs and affected modules by name or glob, so
// AffectedZones names the zones the work really touches. A file under a
// repository's clone path is matched relative to it, and only against that
// repository's zones.
func TestZonesForAffected(t *testing.T) {
	zs := []rootedZone{
		{root: "/repo-cache/p/api", zone: zones.Zone{Name: "db", Patterns: []string{"internal/db/**"}, ModulePaths: []string{"internal/db"}}},
		{root: "/repo-cache/p/api", zone: zones.Zone{Name: "entry", Patterns: []string{"cmd/*/main.go"}}},
		{root: "/repo-cache/p/web", zone: zones.Zone{Name: "web-auth", Patterns: []string{"src/auth/**"}}},
		{root: "", zone: zones.Zone{Name: "legacy", Patterns: []string{"legacy/**"}, ModulePaths: []string{"billing"}}},
	}
	cases := []struct {
		name    string
		modules []string
		files   []string
		want    []string
	}{
		{"relative file", nil, []string{"internal/db/pool.go"}, []string{"db"}},
		{"glob with a star segment", nil, []string{"cmd/api/main.go"}, []string{"entry"}},
		{"absolute file in its own repository", nil, []string{"/repo-cache/p/api/internal/db/x.go"}, []string{"db"}},
		{"absolute file in another repository", nil, []string{"/repo-cache/p/web/internal/db/x.go"}, nil},
		{"web file", nil, []string{"/repo-cache/p/web/src/auth/login.ts"}, []string{"web-auth"}},
		{"module by derived path", []string{"internal/db"}, nil, []string{"db"}},
		{"module by legacy module path", []string{"billing"}, nil, []string{"legacy"}},
		{"module below a zone", []string{"internal/db/migrate"}, nil, []string{"db"}},
		{"nothing", []string{"internal/codex"}, []string{"README.md"}, nil},
		{"several", []string{"billing"}, []string{"cmd/x/main.go", "internal/db/a.go"}, []string{"db", "entry", "legacy"}},
		{"absolute path never matches a legacy zone", nil, []string{"/srv/legacy/app/x.go"}, nil},
	}
	// A clone path containing a zone's directory name: only the
	// repository-relative part is matched.
	infra := []rootedZone{{root: "/srv/infra/app", zone: zones.Zone{Name: "infra", Patterns: []string{"infra/**"}}}}
	if got := zonesForAffected(infra, nil, []string{"/srv/infra/app/x.go"}); got != nil {
		t.Errorf("clone path /srv/infra/app matched infra/**: %v", got)
	}
	if got := zonesForAffected(infra, nil, []string{"/srv/infra/app/infra/main.tf"}); !reflect.DeepEqual(got, []string{"infra"}) {
		t.Errorf("repository infra/ = %v", got)
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := zonesForAffected(zs, tc.modules, tc.files)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}
