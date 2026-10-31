package jsresolve

import "testing"

func TestResolveTable(t *testing.T) {
	files := map[string]bool{
		"src/x.ts":            true,
		"src/comp.tsx":        true,
		"src/dir/index.ts":    true,
		"src/mod.mts":         true,
		"src/legacy.cts":      true,
		"src/data.json":       true,
		"src/plain.js":        true,
		"src/both.js":         true,
		"src/both.ts":         true,
		"src/noext.ts":        true,
		"src/widget/index.js": true,
	}
	exists := func(p string) bool { return files[p] }
	cases := []struct {
		base string
		want string
		ok   bool
	}{
		{"src/x.js", "src/x.ts", true},              // TypeScript ESM .js -> .ts
		{"src/comp.jsx", "src/comp.tsx", true},      // .jsx -> .tsx
		{"src/comp.js", "src/comp.tsx", true},       // .js -> .tsx
		{"src/dir", "src/dir/index.ts", true},       // directory index
		{"src/mod.mjs", "src/mod.mts", true},        // .mjs -> .mts
		{"src/legacy.cjs", "src/legacy.cts", true},  // .cjs -> .cts
		{"src/data.json", "src/data.json", true},    // literal .json
		{"src/data", "src/data.json", true},         // extension append
		{"src/plain.js", "src/plain.js", true},      // the file as written
		{"src/both.js", "src/both.js", true},        // as written wins over the mapping
		{"src/noext", "src/noext.ts", true},         // extension-less
		{"src/widget", "src/widget/index.js", true}, // directory index (js)
		{"src/missing.js", "", false},               // negative: neither x.js nor x.ts
		{"src/x.mjs", "", false},                    // negative: .mjs never maps to .ts
	}
	for _, tc := range cases {
		got, ok := Resolve(tc.base, exists)
		if ok != tc.ok || got != tc.want {
			t.Errorf("Resolve(%q) = %q, %v; want %q, %v", tc.base, got, ok, tc.want, tc.ok)
		}
	}
}

// TestCandidatesOrder pins the order: the specifier as written, extension
// appends, the TypeScript ESM mapping, then directory indexes.
func TestCandidatesOrder(t *testing.T) {
	c := Candidates("a/x.js")
	idx := func(s string) int {
		for i, v := range c {
			if v == s {
				return i
			}
		}
		t.Fatalf("Candidates(a/x.js) missing %q: %v", s, c)
		return -1
	}
	if !(idx("a/x.js") < idx("a/x.js.ts") && idx("a/x.js.ts") < idx("a/x.ts") && idx("a/x.ts") < idx("a/x.tsx") && idx("a/x.tsx") < idx("a/x.js/index.ts")) {
		t.Fatalf("candidate order wrong: %v", c)
	}
}
