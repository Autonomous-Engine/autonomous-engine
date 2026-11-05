package verifier

import (
	"strings"
	"testing"
)

// Live (ciblame item 0f565594), the verifier cited every criterion as
// `path:line: what is there` — "internal/report/report.go:20: ByDay =
// \"day\"" — and the strict path:line pattern voided 14 of 15 criteria
// ("neither a tool call id … nor a path:line reference"). A citation is
// its leading reference; a note after it, separated by a colon or a
// space, is for the reader and does not void the criterion.
func TestAnnotatedReferencesAreTheirLeadingReference(t *testing.T) {
	calls := map[string]bool{"call_1": true}
	exists := func(p string) bool { return p == "a.go" }
	for _, ref := range []string{
		`a.go:1: ByDay = \"day\"`,
		`a.go:1-3 — the constant block`,
		`call_1 (go test ./... output)`,
		`call_1: tests pass`,
	} {
		in := strings.Replace(approveAC1, `"a.go:1"`, `"`+ref+`"`, 1)
		v, err := ParseVerdict(in, []string{"AC-1"}, Grounding{CallIDs: calls, PathExists: exists})
		if err != nil || v.PerCriterion[0].Status != StatusMet {
			t.Fatalf("ref %q: err=%v verdict=%+v, want AC-1 met on its leading reference", ref, err, v)
		}
	}
	// The note does not rescue a reference that is not one.
	for _, ref := range []string{`b.go:1: a file that is not at head`, `the help text: shows day`, `call_9: not a call of this run`} {
		in := strings.Replace(approveAC1, `"a.go:1"`, `"`+ref+`"`, 1)
		if _, err := ParseVerdict(in, []string{"AC-1"}, Grounding{CallIDs: calls, PathExists: exists}); err == nil {
			t.Fatalf("ref %q accepted", ref)
		}
	}
}
