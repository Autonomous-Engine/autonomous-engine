package tools

import (
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/llm/tools/lsp"
)

// The post-write validator judged each edit alone: a function that uses
// strings.ToLower was reverted for "undefined: strings", and the import
// added next was reverted as "imported and not used" — a change that needs
// an import and its use could never land in two edits (four appends
// refused live, one line of a five-step task committed). An import-balance
// diagnostic is transitional: the edit stands and the model is told what
// remains; everything else still reverts.
func TestTransitionalDiagnosticIsAnImportBalance(t *testing.T) {
	src := strings.Join([]string{
		"package enmime",
		"",
		"import \"strings\"",
		"",
		"func key(s string) string {",
		"\treturn strings.ToLower(s)",
		"}",
		"func opt(o ParserOption) {}",
	}, "\n")
	at := func(line, col int, msg string) lsp.Diagnostic {
		return lsp.Diagnostic{Range: lsp.Range{Start: lsp.Position{Line: line - 1, Character: col - 1}}, Severity: 1, Source: "compiler", Message: msg}
	}
	cases := []struct {
		name string
		d    lsp.Diagnostic
		want bool
	}{
		{"missing import used as a package", at(6, 9, "undefined: strings"), true},
		{"unused import", at(3, 8, `"strings" imported and not used`), true},
		{"undefined type", at(8, 12, "undefined: ParserOption"), false},
		{"syntax error", at(5, 1, "expected ';', found 'EOF'"), false},
	}
	for _, c := range cases {
		if got := transitionalDiagnostic(c.d, []byte(src)); got != c.want {
			t.Errorf("%s: transitional = %v, want %v", c.name, got, c.want)
		}
	}
}
