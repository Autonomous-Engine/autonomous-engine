package zones

import (
	"path"
	"regexp"
	"sort"
	"strings"
)

// riskDescription is the description of a zone proposed from the
// onboarding risk_zones answer.
const riskDescription = "High-risk area named in onboarding (risk_zones): a human must approve changes."

var (
	riskSplit = regexp.MustCompile(`[,;]|\.\s`)
	riskToken = regexp.MustCompile(`^[A-Za-z0-9_./*?\[\]-]+$`)
)

// RiskZoneTemplates turns the onboarding risk_zones answer into guarded
// (human_review) zone proposals, one per module or path it names. The
// answer is free text — the prefill writes "N of M modules are high-risk
// (...): a, b, c. ..." and a user may type their own list — so only
// fragments that are a single path-like token (no spaces) are kept, and
// on a "label: list" line only the list is read; prose is ignored.
// evidenceModules (the prefill candidate's module evidence) are added as
// well. A token naming a directory or module becomes
// "<token>/**"; one whose last segment has an extension, or that already
// is a glob, is kept as is. Sorted by name, distinct; the wizard lets the
// operator drop one or raise it to escalate (restricted).
func RiskZoneTemplates(answer string, evidenceModules []string) []Template {
	seen := map[string]bool{}
	var out []Template
	add := func(tok string) {
		tok = strings.Trim(strings.TrimSpace(tok), "().`'\"")
		tok = strings.Trim(strings.ReplaceAll(tok, "\\", "/"), "/")
		if tok == "" || !riskToken.MatchString(tok) {
			return
		}
		// A bare number ("3", "20") is never a module.
		if strings.Trim(tok, "0123456789") == "" {
			return
		}
		name := strings.TrimSuffix(tok, "/**")
		if name == "" || seen[name] {
			return
		}
		pattern := tok + "/**"
		if strings.ContainsAny(tok, "*?[") || strings.Contains(path.Base(tok), ".") {
			pattern = tok
		}
		if Validate(name, []string{pattern}, PolicyHumanReview) != nil {
			return
		}
		seen[name] = true
		out = append(out, Template{
			Name: name, Description: riskDescription, Patterns: []string{pattern},
			Policy: PolicyHumanReview, Source: SourceOnboarding,
		})
	}
	for _, line := range strings.Split(answer, "\n") {
		// "label: a, b" — the label is prose, the list follows it.
		if _, list, ok := strings.Cut(line, ": "); ok {
			line = list
		}
		for _, frag := range riskSplit.Split(line, -1) {
			add(frag)
		}
	}
	for _, m := range evidenceModules {
		add(m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
