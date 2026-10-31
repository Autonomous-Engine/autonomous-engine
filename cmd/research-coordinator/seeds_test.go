package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// narrowIntake is the shape of a live run's narrow intake (item 2 of the
// 2026-10-04 run), restated on the synthetic sample-app: it names the exact
// files and lines, the three goals, and says research may skip
// repository-wide exploration (files, ~L840/~L848/~L855, MAPFORGE_API_KEY,
// the "no API keys" claim, the skip instruction).
const narrowIntake = `sample-app: Mapforge basemap tiles now require an API key.

The map loads Mapforge basemap tiles from https://tiles.mapforgecdn.example/night_full/{z}/{x}/{y}.png without any key. The three tile URLs are in public/index.html ~L840 and ~L848, and in landing/index.html ~L855.

Goals:
1. Make the key configurable: add a MAPFORGE_API_KEY environment variable and append it to the three mapforgecdn tile URLs.
2. Document the variable.
3. Correct the landing page's "no API keys" claim.

The PR must state that the owner has to obtain a Mapforge API key (https://mapforge.example/keys) and set MAPFORGE_API_KEY. This is a narrow, file-scoped fix: research may skip repository-wide exploration.`

// narrowPRD is a PRD of the shape product-agent produced for that intake
// ("Narrow, File-Specific Bug Fix"): three acceptance criteria.
const narrowPRD = "# Narrow, File-Specific Bug Fix: Mapforge basemap API key\n\n" +
	"## Problem\nMapforge tiles in `public/index.html` (~L840, ~L848) and `landing/index.html` (~L855) are requested without a key.\n\n" +
	"## Acceptance Criteria\n" +
	"- The three mapforgecdn tile URLs carry the key from MAPFORGE_API_KEY.\n" +
	"- MAPFORGE_API_KEY is documented.\n" +
	"- The landing page no longer claims no API keys are needed.\n\n" +
	"## Out of scope\n- Switching basemap provider.\n"

func writeTree(t *testing.T, files ...string) string {
	t.Helper()
	root := t.TempDir()
	for _, f := range files {
		p := filepath.Join(root, filepath.FromSlash(f))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestExtractSeedCandidates_NarrowIntake(t *testing.T) {
	got := extractSeedCandidates(narrowIntake, narrowPRD)
	wantFiles := []string{"public/index.html", "landing/index.html"}
	if !reflect.DeepEqual(seedPaths(got), wantFiles) {
		t.Fatalf("files = %v, want %v", seedPaths(got), wantFiles)
	}
	wantLines := map[string][]int{"public/index.html": {840, 848}, "landing/index.html": {855}}
	if lines := seedLineMap(got); !reflect.DeepEqual(lines, wantLines) {
		t.Fatalf("lines = %v, want %v", lines, wantLines)
	}
}

// The acceptance check of R2: for the narrow intake the validated
// seeds are public/index.html and landing/index.html with their lines, and
// the fan-out is exactly [code_trace].
func TestNarrowIntake_SeedsAndFanOut(t *testing.T) {
	clone := writeTree(t, "public/index.html", "landing/index.html", "package.json", "server.js")
	seeds := validateSeeds(extractSeedCandidates(narrowIntake, narrowPRD), []string{clone}, nil)
	if seeds.Unverified || seeds.Source != seedSourceClone {
		t.Fatalf("seeds not verified against the clone: %+v", seeds)
	}
	if want := []string{"public/index.html", "landing/index.html"}; !reflect.DeepEqual(seeds.Files, want) {
		t.Fatalf("files = %v, want %v", seeds.Files, want)
	}
	if want := (map[string][]int{"public/index.html": {840, 848}, "landing/index.html": {855}}); !reflect.DeepEqual(seeds.Lines, want) {
		t.Fatalf("lines = %v, want %v", seeds.Lines, want)
	}
	types := selectResearchTypes("bug", seeds.Files, !seeds.Unverified, countAcceptanceCriteria(narrowPRD), func(string) string { return "" })
	if want := []string{"code_trace"}; !reflect.DeepEqual(types, want) {
		t.Fatalf("fan-out = %v, want %v", types, want)
	}
}

func TestExtractSeedCandidates_Forms(t *testing.T) {
	cases := []struct {
		name  string
		text  string
		files []string
		lines map[string][]int
	}{
		{"dir/file", "Change cmd/api/server.go to add the flag.", []string{"cmd/api/server.go"}, nil},
		{"file:line", "The panic is at main.go:120.", []string{"main.go"}, map[string][]int{"main.go": {120}}},
		{"file:range", "See internal/x/y.go:10-20 for it.", []string{"internal/x/y.go"}, map[string][]int{"internal/x/y.go": {10, 20}}},
		{"file (~L840)", "edit handlers.go (~L840) only", []string{"handlers.go"}, map[string][]int{"handlers.go": {840}}},
		{"backticked bare", "Update `README.md` with the variable.", []string{"README.md"}, nil},
		{"backticked dir", "in `src/app/page.tsx` the header", []string{"src/app/page.tsx"}, nil},
		{"lines word", "web/app.js lines 12 and 40 build the URL", []string{"web/app.js"}, map[string][]int{"web/app.js": {12, 40}}},
		{"leading ./", "open ./scripts/build.sh first", []string{"scripts/build.sh"}, nil},
		{"bare prose names are not paths", "Use Node.js, e.g. the index.html served by express.", nil, nil},
		{"urls are not paths", "Tiles from https://tiles.example.com/a/b/c.png and http://x.io/y.js", nil, nil},
		{"domain path without scheme", "see mapforge.example/keys for keys", nil, nil},
		{"dedupe and merge lines", "a/b.go:3 then `a/b.go` and a/b.go:1", []string{"a/b.go"}, map[string][]int{"a/b.go": {1, 3}}},
		{"parent traversal rejected", "do not touch ../secret/key.pem", nil, nil},
		{"line window stops at next path", "x/a.go ~L5 and y/b.go ~L9", []string{"x/a.go", "y/b.go"}, map[string][]int{"x/a.go": {5}, "y/b.go": {9}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := extractSeedCandidates(tc.text)
			if files := seedPaths(got); !reflect.DeepEqual(files, tc.files) {
				t.Fatalf("files = %v, want %v", files, tc.files)
			}
			lines := seedLineMap(got)
			if len(tc.lines) == 0 {
				if len(lines) != 0 {
					t.Fatalf("lines = %v, want none", lines)
				}
				return
			}
			if !reflect.DeepEqual(lines, tc.lines) {
				t.Fatalf("lines = %v, want %v", lines, tc.lines)
			}
		})
	}
}

func TestExtractSeedCandidates_CRLF(t *testing.T) {
	got := extractSeedCandidates(strings.ReplaceAll(narrowIntake, "\n", "\r\n"))
	if want := []string{"public/index.html", "landing/index.html"}; !reflect.DeepEqual(seedPaths(got), want) {
		t.Fatalf("files = %v, want %v", seedPaths(got), want)
	}
}

func TestValidateSeeds_DropsMissingPathsAgainstClone(t *testing.T) {
	clone := writeTree(t, "public/index.html")
	seeds := validateSeeds(extractSeedCandidates("public/index.html:840 and src/missing.js:3"), []string{clone}, nil)
	if want := []string{"public/index.html"}; !reflect.DeepEqual(seeds.Files, want) {
		t.Fatalf("files = %v, want %v", seeds.Files, want)
	}
	if _, ok := seeds.Lines["src/missing.js"]; ok {
		t.Fatal("lines kept for a dropped path")
	}
	if seeds.Unverified {
		t.Fatal("clone-validated seeds flagged unverified")
	}
}

func TestValidateSeeds_SecondRepoClone(t *testing.T) {
	a := writeTree(t, "a.txt")
	b := writeTree(t, "web/x.ts")
	seeds := validateSeeds(extractSeedCandidates("web/x.ts"), []string{"", filepath.Join(a, "missing"), a, b}, nil)
	if want := []string{"web/x.ts"}; !reflect.DeepEqual(seeds.Files, want) {
		t.Fatalf("files = %v, want %v", seeds.Files, want)
	}
}

func TestValidateSeeds_DirectoryIsNotAFile(t *testing.T) {
	clone := writeTree(t, "pkg.d/inner.txt")
	seeds := validateSeeds(extractSeedCandidates("look at pkg.d/inner.txt and `pkg.d`"), []string{clone}, nil)
	if want := []string{"pkg.d/inner.txt"}; !reflect.DeepEqual(seeds.Files, want) {
		t.Fatalf("files = %v, want %v", seeds.Files, want)
	}
}

func TestValidateSeeds_CodexFallbackWhenNoClone(t *testing.T) {
	cands := extractSeedCandidates("public/index.html ~L840 and src/ghost.js")
	known := func(p string) bool { return p == "public/index.html" }
	seeds := validateSeeds(cands, []string{filepath.Join(t.TempDir(), "absent")}, codexKnowsFunc(known))
	if seeds.Source != seedSourceCodex || seeds.Unverified {
		t.Fatalf("source=%s unverified=%v, want codex/false", seeds.Source, seeds.Unverified)
	}
	if want := []string{"public/index.html"}; !reflect.DeepEqual(seeds.Files, want) {
		t.Fatalf("files = %v, want %v", seeds.Files, want)
	}
}

func TestValidateSeeds_UnverifiedWhenNothingToCheck(t *testing.T) {
	cands := extractSeedCandidates("public/index.html ~L840")
	for name, codex := range map[string]seedCodex{
		"no codex":            nil,
		"codex knows nothing": codexKnowsFunc(func(string) bool { return false }),
	} {
		t.Run(name, func(t *testing.T) {
			seeds := validateSeeds(cands, nil, codex)
			if !seeds.Unverified || seeds.Source != seedSourceUnverified {
				t.Fatalf("source=%s unverified=%v, want unverified/true", seeds.Source, seeds.Unverified)
			}
			if want := []string{"public/index.html"}; !reflect.DeepEqual(seeds.Files, want) {
				t.Fatalf("files = %v, want %v", seeds.Files, want)
			}
		})
	}
}

func TestValidateSeeds_CapAt12(t *testing.T) {
	var names []string
	var text strings.Builder
	for i := 0; i < 20; i++ {
		n := "d/f" + string(rune('a'+i)) + ".go"
		names = append(names, n)
		text.WriteString(n + " ")
	}
	clone := writeTree(t, names...)
	seeds := validateSeeds(extractSeedCandidates(text.String()), []string{clone}, nil)
	if len(seeds.Files) != maxSeedFiles || maxSeedFiles != 12 {
		t.Fatalf("got %d seeds (cap %d), want 12", len(seeds.Files), maxSeedFiles)
	}
	if !reflect.DeepEqual(seeds.Files, names[:12]) {
		t.Fatalf("cap kept %v, want the first 12 in order", seeds.Files)
	}
}

// codexKnowsFunc adapts a predicate to seedCodex for tests.
type codexKnowsFunc func(string) bool

func (f codexKnowsFunc) Knows(p string) bool { return f(p) }
