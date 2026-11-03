package tools

import (
	"regexp"
	"strings"

	"github.com/autonomousengine/autonomous-engine/internal/llm/tools/lsp"
)

var (
	undefinedIdentRE = regexp.MustCompile(`^undefined: ([A-Za-z_][A-Za-z0-9_]*)$`)
	unusedImportRE   = regexp.MustCompile(`imported and not used`)
)

// transitionalDiagnostic reports an import-balance diagnostic: an import
// not used yet, or a package used before its import. An edit that needs an
// import and its use cannot land in one write and the other; the edit
// stands and the next one balances it (the build still fails until it
// does). Everything else — a missing type, a syntax error — is real.
func transitionalDiagnostic(d lsp.Diagnostic, content []byte) bool {
	msg := strings.TrimSpace(d.Message)
	if unusedImportRE.MatchString(msg) {
		return true
	}
	m := undefinedIdentRE.FindStringSubmatch(msg)
	if m == nil {
		return false
	}
	// The identifier is used as a package: `ident.` at the diagnostic.
	lines := strings.Split(string(content), "\n")
	if d.Range.Start.Line < 0 || d.Range.Start.Line >= len(lines) {
		return false
	}
	rest := lines[d.Range.Start.Line]
	if d.Range.Start.Character >= 0 && d.Range.Start.Character <= len(rest) {
		rest = rest[d.Range.Start.Character:]
	}
	return strings.HasPrefix(rest, m[1]+".")
}
