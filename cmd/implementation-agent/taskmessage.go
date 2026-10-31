package main

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/autonomousengine/autonomous-engine/internal/plan"
)

// taskStep is one "## Step N: title" section of the planner's description.
type taskStep struct {
	title    string
	body     []string
	files    string
	criteria string
}

var (
	stepHeadingRE = regexp.MustCompile(`(?m)^## Step (\d+): (.*)$`)
	sectionRE     = regexp.MustCompile(`(?m)^## `)
	bracketListRE = regexp.MustCompile(`^\[(.*)\]$`)
)

// parseTaskSteps reads the steps out of a planner description; nil when the
// description has none (a legacy or single-step task).
func parseTaskSteps(desc string) []taskStep {
	desc = strings.ReplaceAll(desc, "\r\n", "\n")
	locs := stepHeadingRE.FindAllStringSubmatchIndex(desc, -1)
	var steps []taskStep
	for k, loc := range locs {
		end := len(desc)
		if k+1 < len(locs) {
			end = locs[k+1][0]
		}
		rest := desc[loc[1]:end]
		if m := sectionRE.FindStringIndex(rest); m != nil {
			rest = rest[:m[0]]
		}
		st := taskStep{title: strings.TrimSpace(desc[loc[4]:loc[5]])}
		deliverable := ""
		for _, ln := range strings.Split(rest, "\n") {
			t := strings.TrimSpace(ln)
			switch {
			case t == "":
			case strings.HasPrefix(t, "Files:"):
				st.files = listField(strings.TrimPrefix(t, "Files:"))
			case strings.HasPrefix(t, "Criteria:"):
				st.criteria = strings.TrimSpace(strings.TrimPrefix(t, "Criteria:"))
			case strings.HasPrefix(t, "Deliverable:"):
				deliverable = strings.TrimSpace(strings.TrimPrefix(t, "Deliverable:"))
			case strings.HasPrefix(t, "Depends on"), strings.HasPrefix(t, "Verification:"):
				// Order is the step numbering; checks are listed once.
			default:
				st.body = append(st.body, t)
			}
		}
		if len(st.body) == 0 && deliverable != "" {
			st.body = []string{deliverable}
		}
		steps = append(steps, st)
	}
	return steps
}

// listField renders "[a b c]" (Go's %v of a slice) as "a, b, c".
func listField(s string) string {
	s = strings.TrimSpace(s)
	if m := bracketListRE.FindStringSubmatch(s); m != nil {
		s = m[1]
	}
	return strings.Join(strings.Fields(s), ", ")
}

// buildTaskMessage is the first user message of the implementation loop
// (T3): the title, the steps once (numbered, with their files and
// criteria), the acceptance criteria once (id and text), the checks once,
// and the files in scope (files_to_change ∪ the step files). A live run
// (attempt 6) sent 12,105 chars that repeated the steps in a "Deliverable"
// list, in each criterion's verbatim "Source:" quote and in a technical
// brief restating them, plus per-file evidence notes and a Next.js
// "orphan files" note. The brief is kept (trimmed) only when the
// description has no steps. The checkpoint and the ledger keep this
// message verbatim (B2).
func buildTaskMessage(task implementationTaskMessage) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Task: %s\n", strings.TrimSpace(task.Title))

	steps := parseTaskSteps(task.Description)
	if len(steps) > 0 {
		b.WriteString("\n## Steps\n")
		for i, st := range steps {
			fmt.Fprintf(&b, "%d. %s", i+1, st.title)
			var tags []string
			if st.files != "" {
				tags = append(tags, "files: "+st.files)
			}
			if st.criteria != "" {
				tags = append(tags, st.criteria)
			}
			if len(tags) > 0 {
				fmt.Fprintf(&b, " (%s)", strings.Join(tags, "; "))
			}
			b.WriteString("\n")
			for _, ln := range st.body {
				if restatesCriteria(ln, st.criteria, task.Criteria) {
					continue
				}
				fmt.Fprintf(&b, "   %s\n", ln)
			}
			if tl := stepTarget(st, task.Criteria, task.LineSeeds); tl != "" {
				fmt.Fprintf(&b, "   %s\n", tl)
			}
		}
	} else {
		if d := legacyDescription(task.Description); d != "" {
			b.WriteString("\n## Description\n")
			b.WriteString(d)
			b.WriteString("\n")
		}
		if br := trimBrief(task.TechnicalBrief); br != "" {
			b.WriteString("\n## Technical brief\n")
			b.WriteString(br)
			b.WriteString("\n")
		}
	}

	b.WriteString(whereToEdit(task.LineSeeds))

	if len(task.Criteria) > 0 {
		b.WriteString("\n## Acceptance criteria\n")
		for _, c := range task.Criteria {
			fmt.Fprintf(&b, "- %s: %s\n", c.ID, strings.TrimSpace(c.Text))
		}
	}

	if checks := taskChecks(task); len(checks) > 0 {
		b.WriteString("\n## Checks\n")
		for _, c := range checks {
			fmt.Fprintf(&b, "- %s\n", c)
		}
	}

	scope, stepFiles := taskScope(task)
	if len(scope) > 0 {
		b.WriteString("\n## Files in scope\n")
		b.WriteString(strings.Join(scope, ", "))
		b.WriteString("\n")
		if needsEnvTestHint(steps, stepFiles, scope) {
			b.WriteString(envTestHint + "\n")
		}
	}
	if len(task.RelevantPatterns) > 0 {
		b.WriteString("\n## Patterns to follow\n")
		for _, p := range task.RelevantPatterns {
			fmt.Fprintf(&b, "- %s\n", p)
		}
	}
	if len(task.RelevantAntiPatterns) > 0 {
		b.WriteString("\n## Patterns to avoid\n")
		for _, p := range task.RelevantAntiPatterns {
			fmt.Fprintf(&b, "- %s\n", p)
		}
	}
	if p := strings.TrimSpace(task.CodexPatterns); p != "" {
		b.WriteString("\n## Codex patterns\n")
		b.WriteString(truncate(p, 1200))
		b.WriteString("\n")
	}
	if len(task.CrossRepoTouchpoints) > 1 {
		fmt.Fprintf(&b, "\nOther repositories this change touches: %s (edit only this one).\n", strings.Join(task.CrossRepoTouchpoints, ", "))
	}
	return strings.TrimRight(b.String(), "\n")
}

// stepCriterionIDRE finds the criterion ids of a step's "Criteria:" field.
var stepCriterionIDRE = regexp.MustCompile(`[A-Za-z]+-\d+`)

// taskWordRE splits text into words for restatesCriteria.
var taskWordRE = regexp.MustCompile(`[a-z0-9_]+`)

// taskStopWords are not counted by restatesCriteria.
var taskStopWords = map[string]bool{
	"the": true, "and": true, "for": true, "with": true, "that": true, "this": true, "from": true,
	"into": true, "are": true, "its": true, "all": true, "any": true, "has": true, "have": true,
	"will": true, "should": true, "must": true, "can": true, "when": true, "then": true, "than": true,
	"was": true, "were": true, "been": true, "also": true, "each": true, "both": true, "which": true,
	"use": true, "using": true, "via": true, "per": true, "your": true, "their": true, "there": true,
	"same": true, "new": true,
}

// limiterRE: a negation or limiter; a line with one is never dropped (it
// may be what the criterion does not say). A quoted literal is content,
// not an instruction ("… TILER_API_KEY is not configured …" in a notice
// text): quotedSpanRE spans are left out of the check.
var limiterRE = regexp.MustCompile(`(?i)\b(not|only|never|except|without|instead|dont|don't|don’t)\b`)

// quotedSpanRE is a backticked span, or a quoted one that opens after a
// space or punctuation (so the apostrophe of "don't" opens nothing).
var quotedSpanRE = regexp.MustCompile("(^|[\\s(\\[{:,])('[^']{2,}'|\"[^\"]{2,}\"|`[^`]+`)")

// specificTermRE: a path, a dotted, camelCase or underscored identifier,
// or a backticked or quoted term.
var specificTermRE = regexp.MustCompile("`[^`]+`|'[^']{2,}'|\"[^\"]{2,}\"|[A-Za-z0-9_.-]*/[A-Za-z0-9_./-]+|[A-Za-z_][A-Za-z0-9_]*(?:\\.[A-Za-z0-9_]+)+|[a-z][a-z0-9]*[A-Z][A-Za-z0-9]*|[A-Za-z0-9]+_[A-Za-z0-9_]+")

// restatesCriteria: a step's body line restates one of the step's
// criteria, which the message lists anyway — at least 80% of its words
// (3+ letters, stop words left out) are in that criterion, every specific
// term of the line (path, dotted/camelCase/underscored identifier,
// backticked or quoted term) is in it too, and the line has no negation or
// limiter (R12: live runs, attempts 11 and 12, had every step body restate
// its criteria, about 900 chars; the room goes to the whole "Where to
// edit" ranges and the step's Target line).
func restatesCriteria(line, ids string, criteria []plan.Criterion) bool {
	if limiterRE.MatchString(quotedSpanRE.ReplaceAllString(line, "$1")) {
		return false
	}
	words := func(s string) []string {
		var out []string
		for _, w := range taskWordRE.FindAllString(strings.ToLower(s), -1) {
			if len(w) >= 3 && !taskStopWords[w] {
				out = append(out, w)
			}
		}
		return out
	}
	lw := words(line)
	if len(lw) == 0 {
		return false
	}
	terms := specificTermRE.FindAllString(line, -1)
	for _, id := range stepCriterionIDRE.FindAllString(ids, -1) {
		for _, c := range criteria {
			if c.ID != id {
				continue
			}
			text := unquoted(strings.ToLower(c.Text))
			missing := false
			for _, term := range terms {
				if !strings.Contains(text, unquoted(strings.ToLower(strings.Trim(term, "`'\".")))) {
					missing = true
					break
				}
			}
			if missing {
				continue
			}
			in := map[string]bool{}
			for _, w := range words(c.Text) {
				in[w] = true
			}
			n := 0
			for _, w := range lw {
				if in[w] {
					n++
				}
			}
			if 5*n >= 4*len(lw) {
				return true
			}
		}
	}
	return false
}

// quoteCharsRE are the quote characters unquoted drops.
var quoteCharsRE = regexp.MustCompile("[`'\"]")

// unquoted drops quote characters, so a term and a criterion that quote
// the same text differently ("{'tilerApiKey': string}" in a step body,
// `{"tilerApiKey": string}` in its criterion; live run attempt 13b) still
// match (R15: the room for attempt 13b's Target line).
func unquoted(s string) string { return quoteCharsRE.ReplaceAllString(s, "") }

// modifyVerbRE: a criterion that changes something in place ("append …",
// "replace", "change", "add … to the … URL"). R18: in any inflection
// (attempt 15's AC-1 read "… and appended to the tile URLs").
var modifyVerbRE = regexp.MustCompile(`(?i)\b(append(s|ed|ing)?|replac(e|es|ed|ing)|chang(e|es|ed|ing))\b|\badd(s|ed|ing)?\b[^.;]{0,80}?\bto the\b[^.;]{0,40}?\burls?\b`)

// urlVerbRE (R18): prefix / suffix / rewrite / update modify in place only
// when their clause is about a URL ("Update the tile URLs to carry the
// key"); "update the meta description" or "update README.md" is not a
// Target (live run attempt 13b's step 4).
var urlVerbRE = regexp.MustCompile(`(?i)\b(prefix(es|ed|ing)?|suffix(es|ed|ing)?|rewrit(e|es|ten|ing)|rewrote|updat(e|es|ed|ing))\b`)

// negatedRE: text that ends in a negation ("must not ", "never ", "don't ",
// "no " as in "makes no changes").
var negatedRE = regexp.MustCompile(`(?i)(^|\W)(not|never|no|don't|don’t|dont)\s*$`)

// clauseBreakRE ends a clause: a period or semicolon before a space (a
// path like "landing/index.html" does not end one).
var clauseBreakRE = regexp.MustCompile(`[.;](\s|$)`)

// clauseAt is the clause of text around byte offsets [from, to).
func clauseAt(text string, from, to int) string {
	start := 0
	for _, loc := range clauseBreakRE.FindAllStringIndex(text[:from], -1) {
		start = loc[1]
	}
	end := len(text)
	if loc := clauseBreakRE.FindStringIndex(text[to:]); loc != nil {
		end = to + loc[0]
	}
	return text[start:end]
}

// asksToModify: the criterion has a modify verb that is not negated (R12
// review: "must not change the URL" is no Target).
func asksToModify(text string) bool {
	for _, loc := range modifyVerbRE.FindAllStringIndex(text, -1) {
		if !negatedRE.MatchString(text[:loc[0]]) {
			return true
		}
	}
	for _, loc := range urlVerbRE.FindAllStringIndex(text, -1) {
		if !negatedRE.MatchString(text[:loc[0]]) && urlWordRE.MatchString(strings.ToLower(clauseAt(text, loc[0], loc[1]))) {
			return true
		}
	}
	return false
}

// quotedLiteralRE is a quoted string literal of at least 3 characters.
var quotedLiteralRE = regexp.MustCompile("'[^']{3,}'|\"[^\"]{3,}\"|`[^`]{3,}`")

// urlWordRE: a criterion about a URL.
var urlWordRE = regexp.MustCompile(`\burls?\b`)

// stepTargetMaxLines bounds the lines one path contributes to a Target.
const stepTargetMaxLines = 3

// stepTarget is a step's one "Target:" line (R12 X4b), or "": when one of
// the step's criteria appends to / replaces / changes something and names
// a step file whose "## Where to edit" lines quote a literal, the line
// names those lines; when no such criterion names any step file, every
// step file's quoted lines are matched against them (R15 T1). A criterion about a URL targets the quoted URL lines;
// otherwise the quoted literals that share a word with it. A live run
// (attempt 12) added a new keyed L.tileLayer and left the keyless tile URLs
// at public/index.html:840,848 and landing/index.html:855 unchanged.
func stepTarget(st taskStep, criteria []plan.Criterion, seeds []lineSeed) string {
	var texts []string
	for _, id := range stepCriterionIDRE.FindAllString(st.criteria, -1) {
		for _, c := range criteria {
			if c.ID == id && asksToModify(c.Text) {
				texts = append(texts, c.Text)
			}
		}
	}
	if len(texts) == 0 {
		return ""
	}
	var files []string
	for _, f := range strings.Split(st.files, ",") {
		if f = strings.TrimSpace(f); f != "" {
			files = append(files, f)
		}
	}
	// R15 T1: when no modify criterion names any of the step's files
	// ("Both HTML pages … append `?api_key=<key>` to the Tiler tile URLs",
	// live run attempt 13b), every step file is targeted with all of them;
	// a criterion that names a step file keeps the path-named rule.
	named := false
	for _, f := range files {
		for _, t := range texts {
			named = named || strings.Contains(t, f)
		}
	}
	var parts []string
	for _, f := range files {
		var crit []string
		for _, t := range texts {
			if !named || strings.Contains(t, f) {
				crit = append(crit, t)
			}
		}
		if len(crit) == 0 {
			continue
		}
		joined := strings.ToLower(strings.Join(crit, " "))
		wantURL := urlWordRE.MatchString(joined)
		var nums []string
		for _, s := range seeds {
			if s.Path != f || !s.Verified {
				continue
			}
			for _, q := range strings.Split(s.Snippet, "\n") {
				n, code, ok := strings.Cut(q, ": ")
				if !ok || len(nums) >= stepTargetMaxLines {
					continue
				}
				if wantURL && strings.Contains(code, "://") || !wantURL && literalSharesWord(code, joined) {
					nums = append(nums, strings.TrimSpace(n))
				}
			}
		}
		if len(nums) > 0 {
			parts = append(parts, f+":"+strings.Join(nums, ","))
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return "Target: modify the quoted literal(s) at " + strings.Join(parts, " and ") + " — not a new layer/block."
}

// literalSharesWord: a quoted literal of code has a word of 4+ letters that
// is in text (lower-cased).
func literalSharesWord(code, text string) bool {
	for _, lit := range quotedLiteralRE.FindAllString(code, -1) {
		for _, w := range taskWordRE.FindAllString(strings.ToLower(lit), -1) {
			if len(w) >= 4 && strings.Contains(text, w) {
				return true
			}
		}
	}
	return false
}

// envTestHint is the task message's one line for a test of env-driven
// configuration (R11 S4): a live run's (attempt 11) new test set
// process.env.TILER_API_KEY after src/config/env.ts had read it at import,
// so the final gate failed on the agent's own test.
const envTestHint = "Tests that set env vars must set them before importing the module under test, or re-import it (vi/jest.resetModules or dynamic import)."

// configModuleRE matches a configuration or environment code module
// (src/config/env.ts, app/settings.py, config.js, internal/config/load.go).
var configModuleRE = regexp.MustCompile(`(?i)(^|/)(config|configs|settings|conf)/[^/]+\.(ts|tsx|js|mjs|cjs|py|go|rb)$|(^|/)(env|environment|config|settings)\.(ts|tsx|js|mjs|cjs|py|go|rb)$`)

// needsEnvTestHint: a step's files include a test, and the files in scope
// include a config/env module.
func needsEnvTestHint(steps []taskStep, stepFiles, scope []string) bool {
	files := append([]string(nil), stepFiles...)
	for _, st := range steps {
		for _, f := range strings.Split(st.files, ",") {
			if f = strings.TrimSpace(f); f != "" {
				files = append(files, f)
			}
		}
	}
	hasTest := false
	for _, f := range files {
		hasTest = hasTest || fileRank(f) == 1
	}
	if !hasTest {
		return false
	}
	for _, f := range scope {
		n := strings.ToLower(strings.TrimPrefix(strings.ReplaceAll(f, "\\", "/"), "./"))
		if fileRank(n) != 1 && configModuleRE.MatchString(n) {
			return true
		}
	}
	return false
}

// taskChecks is every verification of the task once: commands with their
// kind, then the descriptive checks.
func taskChecks(task implementationTaskMessage) []string {
	if task.TaskSpec == nil {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	for _, v := range task.TaskSpec.Verification {
		var s string
		switch {
		case strings.TrimSpace(v.Command) != "":
			s = fmt.Sprintf("`%s` (%s)", strings.TrimSpace(v.Command), v.CheckKind)
		case strings.TrimSpace(v.Description) != "":
			s = fmt.Sprintf("%s (%s)", strings.TrimSpace(v.Description), v.CheckKind)
		default:
			continue
		}
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// legacyDescription is a description without steps, minus the planner's
// per-file evidence and grounding notes.
func legacyDescription(desc string) string {
	desc = strings.ReplaceAll(desc, "\r\n", "\n")
	for _, cut := range []string{"\n## File evidence", "\n## Grounding note"} {
		if i := strings.Index(desc, cut); i >= 0 {
			desc = desc[:i]
		}
	}
	return strings.TrimSpace(desc)
}

// trimBrief drops a model preamble before the brief's first heading and
// bounds it.
func trimBrief(brief string) string {
	brief = strings.TrimSpace(strings.ReplaceAll(brief, "\r\n", "\n"))
	if i := strings.Index(brief, "\n#"); i >= 0 && !strings.HasPrefix(brief, "#") {
		brief = strings.TrimSpace(brief[i+1:])
	}
	return truncate(brief, 2000)
}

// lineSeed is a place to edit research found (R10 W5), as the planner
// sends it: path, line range and up to 5 quoted lines ("840: …" when
// verified against the base commit).
type lineSeed struct {
	Path      string `json:"path"`
	LineStart int    `json:"line_start"`
	LineEnd   int    `json:"line_end"`
	Snippet   string `json:"snippet,omitempty"`
	Verified  bool   `json:"verified,omitempty"`
}

const (
	whereToEditMaxSeeds  = 8
	whereToEditMaxLines  = 20 // a short range is quoted whole (R12 X3)
	whereToEditLineChars = 180
	whereToEditMaxChars  = 1500
	// R12 X4a: change the lines in place (attempt 12 added a parallel
	// keyed tile layer and left the quoted URLs).
	whereToEditSectionHead = "\n## Where to edit\nResearch located these lines (base commit). Change these existing lines in place (edit_file on the quoted line) unless the step says to add new code; do not add a parallel code path that leaves these lines unchanged.\n"
)

// whereToEdit is the task message's "## Where to edit" section: each seed
// as path:start-end with its quoted lines, bounded (8 seeds, 5 lines of 180
// chars each, 1,500 chars in all); "" without seeds.
func whereToEdit(seeds []lineSeed) string {
	if len(seeds) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(whereToEditSectionHead)
	for i, s := range seeds {
		if i >= whereToEditMaxSeeds {
			break
		}
		var e strings.Builder
		fmt.Fprintf(&e, "- %s:%d", s.Path, s.LineStart)
		if s.LineEnd > s.LineStart {
			fmt.Fprintf(&e, "-%d", s.LineEnd)
		}
		e.WriteString("\n")
		// Only lines verified against the base commit are quoted: an
		// unverified snippet is research's paraphrase, not file text.
		for k, ln := range strings.Split(strings.TrimRight(s.Snippet, "\n"), "\n") {
			if !s.Verified || k >= whereToEditMaxLines || strings.TrimSpace(ln) == "" {
				continue
			}
			fmt.Fprintf(&e, "    %s\n", truncate(ln, whereToEditLineChars))
		}
		if b.Len()+e.Len() > whereToEditMaxChars {
			break
		}
		b.WriteString(e.String())
	}
	return b.String()
}

// seedLines is each seeded file's first seed line (read_file's default
// window centres on it).
func seedLines(seeds []lineSeed) map[string]int {
	out := map[string]int{}
	for _, s := range seeds {
		line := s.LineStart
		// A verified snippet names the exact lines: centre on the first,
		// or on the first with a URL (a whole quoted range, R12 X3, opens
		// on its first code line; the fixture's tile URL is at 840 of 836-853).
		if s.Verified {
			quoted := strings.Split(s.Snippet, "\n")
			pick := quoted[0]
			for _, q := range quoted {
				if strings.Contains(q, "://") {
					pick = q
					break
				}
			}
			if first, _, ok := strings.Cut(pick, ":"); ok {
				if n, err := strconv.Atoi(strings.TrimSpace(first)); err == nil && n > 0 {
					line = n
				}
			}
		}
		if _, ok := out[s.Path]; !ok && line > 0 {
			out[s.Path] = line
		}
	}
	return out
}
