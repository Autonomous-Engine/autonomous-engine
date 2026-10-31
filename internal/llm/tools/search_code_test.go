package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for rel, body := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// B4: search_code passed GNU grep's --include to BusyBox grep in the alpine
// image ("grep: unrecognized option: include=…"). It now searches in Go:
// the test empties PATH so no host grep can answer.
func TestSearchCodeWorksWithoutAHostGrep(t *testing.T) {
	root := writeTree(t, map[string]string{
		"src/config/env.ts":       "export const config = {\n  searchApiKey: process.env.SEARCH_API_KEY || '',\n};\n",
		"src/server/index.ts":     "import { config } from '../config/env.js';\napp.get('/api/settings', () => config.searchApiKey);\n",
		"public/index.html":       "<script>const SEARCH_URL = 'x';</script>\n",
		"node_modules/x/index.ts": "searchApiKey in a dependency\n",
		"vendor/lib/search.ts":    "searchApiKey vendored\n",
		"assets/logo.png":         "\x00\x01searchApiKey",
		"docs/configuration.md":   "Set SEARCH_API_KEY.\n",
	})
	t.Setenv("PATH", "")
	ex := NewExecutor(root)
	run := func(args map[string]interface{}) string {
		r := ex.searchCode(context.Background(), "c1", args)
		if r.IsError {
			t.Fatalf("search_code %v: error %q", args, r.Content)
		}
		return r.Content
	}

	got := run(map[string]interface{}{"pattern": `searchApiKey`, "file_glob": "*.ts"})
	for _, want := range []string{"src/config/env.ts:2:  searchApiKey: process.env.SEARCH_API_KEY || '',", "src/server/index.ts:2:"} {
		if !strings.Contains(got, want) {
			t.Errorf("glob *.ts: missing %q in\n%s", want, got)
		}
	}
	for _, banned := range []string{"node_modules", "logo.png", "unrecognized option"} {
		if strings.Contains(got, banned) {
			t.Errorf("glob *.ts: result contains %q:\n%s", banned, got)
		}
	}

	if got := run(map[string]interface{}{"pattern": `SEARCH_[A-Z]+`, "file_glob": "src/**/*.ts"}); !strings.Contains(got, "src/config/env.ts:2:") || strings.Contains(got, "public/") || strings.Contains(got, "docs/") {
		t.Errorf("glob src/**/*.ts with a regex:\n%s", got)
	}
	if got := run(map[string]interface{}{"pattern": `searchApiKey`, "file_glob": "!vendor/**"}); strings.Contains(got, "vendor/") || !strings.Contains(got, "src/server/index.ts") {
		t.Errorf("negated glob:\n%s", got)
	}
	if got := run(map[string]interface{}{"pattern": `SEARCH_API_KEY`}); !strings.Contains(got, "docs/configuration.md:1:") || !strings.Contains(got, "src/config/env.ts:2:") {
		t.Errorf("no glob:\n%s", got)
	}
	// A pattern that is not valid Go regex (grep users write "\(" for a
	// literal paren) is searched literally rather than failing.
	if got := run(map[string]interface{}{"pattern": `get('/api`}); !strings.Contains(got, "src/server/index.ts:2:") {
		t.Errorf("literal fallback:\n%s", got)
	}
	if got := run(map[string]interface{}{"pattern": `no-such-token-anywhere`}); got != "No matches found" {
		t.Errorf("no matches: %q", got)
	}
}

// R6 review: files over 2 MB (bundles, dumps) are not read.
func TestSearchCodeSkipsLargeFiles(t *testing.T) {
	big := strings.Repeat("filler line without the token\n", (2<<20)/29+10) + "BIG-TOKEN\n"
	root := writeTree(t, map[string]string{"dist2/bundle.js": big, "src/a.ts": "BIG-TOKEN here\n"})
	r := NewExecutor(root).searchCode(context.Background(), "c1", map[string]interface{}{"pattern": "BIG-TOKEN"})
	if strings.Contains(r.Content, "bundle.js") || !strings.Contains(r.Content, "src/a.ts:1:") {
		t.Fatalf("search over a >2 MB file: %.300q", r.Content)
	}
}
