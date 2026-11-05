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
var (
	// lowerRiskMarker starts the part of an answer that names what is NOT
	// high-risk.
	lowerRiskMarker = regexp.MustCompile(`(?i)\b(lower|low|less|least)[ -]?risk`)
	// pathToken is any run of path characters inside prose.
	pathToken = regexp.MustCompile(`[A-Za-z0-9_./*?\[\]-]+`)
	// pathLike is a relative path: lowercase segments joined by "/" (a
	// "Base64/QP" in prose is not one).
	pathLike = regexp.MustCompile(`^[a-z0-9_.-]+(/[a-z0-9_.*?\[\]-]+)+/?$`)
)

// sourceExts are file extensions that make a bare token a source file.
var sourceExts = map[string]bool{
	".go": true, ".ts": true, ".tsx": true, ".js": true, ".jsx": true, ".py": true, ".rs": true,
	".java": true, ".rb": true, ".cs": true, ".kt": true, ".swift": true, ".php": true, ".sql": true,
	".yaml": true, ".yml": true, ".proto": true,
}

// isPathLike reports whether tok, found inside prose, names a path or a
// source file rather than a word.
func isPathLike(tok string) bool {
	tok = strings.Trim(strings.ReplaceAll(tok, "\\", "/"), "/")
	if tok == "" || strings.Trim(tok, "0123456789") == "" {
		return false
	}
	if strings.Contains(tok, "/") {
		return pathLike.MatchString(tok)
	}
	return sourceExts[strings.ToLower(path.Ext(tok))]
}

func RiskZoneTemplates(answer string, evidenceModules []string) []Template {
	// Only the high-risk part of the answer names zones. The audit's answer
	// ended "… Lower risk: cmd/ tools, internal/stringutil …" and the
	// lower-risk list became human_review zones (F27).
	if loc := lowerRiskMarker.FindStringIndex(answer); loc != nil {
		answer = answer[:loc[0]]
	}
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
		// Paths named inside prose — "the parser (part.go, boundary.go)
		// since every consumer …; internal/coding (charsets.go, …)" —
		// are zones too, even when the fragment around them has spaces.
		for _, tok := range pathToken.FindAllString(line, -1) {
			if isPathLike(strings.Trim(tok, "().`'\"")) {
				add(tok)
			}
		}
	}
	for _, m := range evidenceModules {
		add(m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
