package zones

import (
	"reflect"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/models"
	"github.com/autonomousengine/autonomous-engine/internal/verifypolicy"
)

// Zone globs are security_review_paths globs: for every pattern and file
// below, a zone matches exactly when verifypolicy.MatchPath does.
func TestMatchParityWithVerifyPolicy(t *testing.T) {
	patterns := []string{
		"internal/models/**", "cmd/*/main.go", "cmd/**", "migrations/**", ".env*",
		"docker-compose.yml", "auth*", "go.mod", "**", "src/**/*.ts", "Internal/DB/**",
		"internal\\db\\**", "/internal/db/", "[", "a/[/b",
	}
	files := []string{
		"internal/models/user.go", "cmd/api/main.go", "cmd/api/server/main.go", "x/cmd/api/main.go",
		"migrations/001.up.sql", "db/migrations/2024/001.sql", ".env", ".env.local", "config/.env.prod",
		"docker-compose.yml", "deploy/docker-compose.yml", "internal/auth/login.go", "src/authService.ts",
		"go.mod", "tools/go.mod", "src/a/b/c.ts", "src/c.ts", "internal/db/pool.go", "INTERNAL/db/x.go",
		"internal\\db\\pool.go", "", "README.md",
	}
	for _, p := range patterns {
		z := Zone{Name: "z", Patterns: []string{p}}
		for _, f := range files {
			if got, want := z.Matches(f), verifypolicy.MatchPath(p, f); got != want {
				t.Errorf("zone %q matches %q = %v, MatchPath = %v", p, f, got, want)
			}
			if got, want := MatchPath(p, f), verifypolicy.MatchPath(p, f); got != want {
				t.Errorf("MatchPath(%q, %q) = %v, want %v", p, f, got, want)
			}
		}
	}
}

func TestMatchHitsAndStrictest(t *testing.T) {
	reason := "release freeze"
	zs := []Zone{
		{Name: "safe", Patterns: []string{"internal/models/**", "cmd/*/main.go"}, Policy: PolicyAutonomous, Status: models.ZoneStatusActive},
		{Name: "guarded", Patterns: []string{"cmd/**"}, Policy: PolicyHumanReview, Status: models.ZoneStatusActive},
		{Name: "restricted", Patterns: []string{"migrations/**"}, Policy: PolicyEscalate, Status: models.ZoneStatusFrozen, FrozenReason: &reason},
	}
	hits := Match(zs, []string{"cmd/api/main.go", "internal/models/u.go", "README.md", "cmd/api/main.go"})
	if got := Names(hits); !reflect.DeepEqual(got, []string{"guarded", "safe"}) {
		t.Fatalf("names = %v", got)
	}
	if got := hits[1].Files; !reflect.DeepEqual(got, []string{"cmd/api/main.go", "internal/models/u.go"}) {
		t.Fatalf("safe files = %v", got)
	}
	if got := Strictest(hits); got != PolicyHumanReview {
		t.Fatalf("strictest = %s", got)
	}
	if got := Strictest(nil); got != PolicyAutonomous {
		t.Fatalf("strictest(nil) = %s", got)
	}
	all := Match(zs, []string{"migrations/001.sql", "cmd/x/y.go"})
	if Strictest(all) != PolicyEscalate {
		t.Fatalf("strictest = %s", Strictest(all))
	}
	frozen := FrozenHits(all)
	if len(frozen) != 1 || frozen[0].Zone.FrozenText() != "zone restricted is frozen: release freeze" {
		t.Fatalf("frozen = %+v", frozen)
	}
	if got := Names(WithPolicy(all, PolicyHumanReview)); !reflect.DeepEqual(got, []string{"guarded"}) {
		t.Fatalf("human_review = %v", got)
	}
	if (Zone{Name: "n"}).FrozenText() != "zone n is frozen: no reason given" {
		t.Fatal("empty reason")
	}
}

func TestMatchesModule(t *testing.T) {
	z := Zone{Name: "db", Patterns: []string{"internal/db/**", "auth*"}, ModulePaths: DeriveModulePaths([]string{"internal/db/**", "auth*"})}
	for m, want := range map[string]bool{
		"internal/db": true, "internal/db/migrate": true, "internal/auth": true, "auth": true,
		"internal/codex": false, "": false, "internal": false,
	} {
		if got := z.MatchesModule(m); got != want {
			t.Errorf("MatchesModule(%q) = %v, want %v", m, got, want)
		}
	}
}

// An exclusion pattern ("!glob") takes matching files out of the zone,
// whatever order the patterns are in; it never adds files.
func TestExclusionPatterns(t *testing.T) {
	z := Zone{Name: "secrets", Patterns: []string{"!**/*.example", ".env", ".env.*", "**/secrets/**"}}
	for f, want := range map[string]bool{
		".env": true, ".env.local": true, "secrets/db.yaml": true, "app/secrets/k.json": true,
		".env.example": false, "secrets/db.yaml.example": false, "app/x.example": false, "README.md": false,
	} {
		if got := z.Matches(f); got != want {
			t.Errorf("Matches(%q) = %v, want %v", f, got, want)
		}
	}
	if got := DeriveModulePaths(z.Patterns); !reflect.DeepEqual(got, []string{".env"}) {
		t.Errorf("module paths = %v (exclusions and globs contribute none)", got)
	}
	if err := Validate("only-excl", []string{"!a/**"}, PolicyEscalate); err == nil {
		t.Error("a zone of only exclusions must not validate")
	}
	if err := Validate("empty-excl", []string{"a/**", "!"}, PolicyEscalate); err == nil {
		t.Error("an empty exclusion must not validate")
	}
	if err := Validate("bad-excl", []string{"a/**", "!a/[/b"}, PolicyEscalate); err == nil {
		t.Error("a malformed exclusion glob must not validate")
	}
}

// Zones match repository-relative paths only: an absolute path never
// matches (MatchPath would otherwise match "infra/**" anywhere inside
// "/srv/infra/app/x.go"); RepoRelative makes a path under the clone root
// relative and refuses one outside it.
func TestZonesAnchorToTheRepositoryRoot(t *testing.T) {
	z := Zone{Name: "infra", Patterns: []string{"infra/**"}}
	for _, abs := range []string{"/srv/infra/app/x.go", "/infra/main.tf", `C:\srv\infra\app\x.go`, "C:/infra/x.tf"} {
		if z.Matches(abs) {
			t.Errorf("absolute %q matched", abs)
		}
	}
	for _, tc := range []struct {
		file, root, want string
		ok               bool
	}{
		{"/srv/infra/app/x.go", "/srv/infra/app", "x.go", true},
		{"/srv/infra/app/infra/main.tf", "/srv/infra/app/", "infra/main.tf", true},
		{"infra/main.tf", "/srv/infra/app", "infra/main.tf", true},
		{"./infra/main.tf", "", "infra/main.tf", true},
		{"/srv/other/x.go", "/srv/infra/app", "", false},
		{"/srv/infra/application/x.go", "/srv/infra/app", "", false},
		{"/srv/infra/app/x.go", "", "", false},
		{`C:\repos\app\infra\a.tf`, `C:\repos\app`, "infra/a.tf", true},
	} {
		got, ok := RepoRelative(tc.file, tc.root)
		if got != tc.want || ok != tc.ok {
			t.Errorf("RepoRelative(%q, %q) = %q, %v; want %q, %v", tc.file, tc.root, got, ok, tc.want, tc.ok)
		}
	}
	rel, _ := RepoRelative("/srv/infra/app/x.go", "/srv/infra/app")
	if z.Matches(rel) {
		t.Error("x.go at the repository root matched infra/**")
	}
}

func TestDeriveModulePaths(t *testing.T) {
	got := DeriveModulePaths([]string{"internal/db/**", "migrations/*", "cmd/*/main.go", ".env*", "docker-compose.yml", "/internal/db/", "**", " src\\api\\** "})
	want := []string{"docker-compose.yml", "internal/db", "migrations", "src/api"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	if got := DeriveModulePaths(nil); got == nil || len(got) != 0 {
		t.Fatalf("nil patterns = %#v", got)
	}
}

func TestValidate(t *testing.T) {
	if err := Validate("db", []string{"internal/db/**"}, PolicyEscalate); err != nil {
		t.Fatal(err)
	}
	for name, err := range map[string]error{
		"no name":     Validate(" ", []string{"a"}, PolicyEscalate),
		"bad policy":  Validate("a", []string{"a"}, "maybe"),
		"no patterns": Validate("a", nil, PolicyAutonomous),
		"bad glob":    Validate("a", []string{"a/[/b"}, PolicyAutonomous),
	} {
		if err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
	if got := NormalizePatterns([]string{" a/** ", "", "a/**", "b\\c"}); !reflect.DeepEqual(got, []string{"a/**", "b/c"}) {
		t.Fatalf("normalize = %v", got)
	}
}
