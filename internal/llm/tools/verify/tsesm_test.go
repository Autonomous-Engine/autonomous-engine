package verify

import (
	"os"
	"path/filepath"
	"testing"
)

func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, content := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// TestVerifyImports_TypeScriptESMSpecifiers: a TypeScript ESM repository
// writes `./x.js` for a source x.ts (live-run attempt 6: 17 false
// positives at the final gate). Each mapped form resolves; a specifier
// with neither source is still reported.
func TestVerifyImports_TypeScriptESMSpecifiers(t *testing.T) {
	root := t.TempDir()
	src := "import { env } from '../config/env.js';\n" +
		"import { V } from '../ui/view.jsx';\n" +
		"import { m } from '../lib/m.mjs';\n" +
		"import { c } from '../lib/c.cjs';\n" +
		"import { l } from '../lib';\n" +
		"import data from '../data.json';\n" +
		"import { gone } from './gone.js';\n"
	writeTree(t, root, map[string]string{
		"package.json":        `{"name":"x","type":"module"}`,
		"src/config/env.ts":   "export const env = {};\n",
		"src/ui/view.tsx":     "export const V = 1;\n",
		"src/lib/m.mts":       "export const m = 1;\n",
		"src/lib/c.cts":       "export const c = 1;\n",
		"src/lib/index.ts":    "export const l = 1;\n",
		"src/data.json":       "{}\n",
		"src/server/index.ts": src,
	})

	res, err := VerifyImports(root, "src/server/index.ts", src)
	if err != nil {
		t.Fatal(err)
	}
	if res.Checked != 7 {
		t.Fatalf("checked = %d, want 7", res.Checked)
	}
	if len(res.Unresolved) != 1 || res.Unresolved[0].Import != "./gone.js" {
		t.Fatalf("unresolved = %+v, want only ./gone.js", res.Unresolved)
	}
}

// TestVerifyImports_AliasTypeScriptESMSpecifier: a tsconfig path alias
// resolves through the same mapping (`@/config/env.js` → src/config/env.ts).
func TestVerifyImports_AliasTypeScriptESMSpecifier(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"tsconfig.json":     `{"compilerOptions":{"baseUrl":".","paths":{"@/*":["./src/*"]}}}`,
		"src/config/env.ts": "export const env = {};\n",
	})
	res, err := VerifyImports(root, "src/app.ts", "import { env } from '@/config/env.js';\n")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Unresolved) != 0 {
		t.Fatalf("unresolved = %+v, want none", res.Unresolved)
	}
}
