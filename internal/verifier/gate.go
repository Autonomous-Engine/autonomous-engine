package verifier

import (
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
)

// Action is what review-agent does with a verdict.
type Action string

const (
	// ActionApprove: proceed to the (optional) second-pass review.
	ActionApprove Action = "approve"
	// ActionRequestChanges: the existing request-changes path, with the
	// defects as structured feedback.
	ActionRequestChanges Action = "request_changes"
	// ActionEscalate: human_review via the existing transition.
	ActionEscalate Action = "escalate"
)

// Decision is the gate's reading of a verdict.
type Decision struct {
	Action Action `json:"action"`
	// Reason is one line naming what decided it.
	Reason string `json:"reason"`
	// SecondPassRequired is set on an approval of an item that has no
	// acceptance criteria: the verifier had nothing to verify against, so
	// the LLM review runs even when REVIEW_LLM_SECOND_PASS=false.
	SecondPassRequired bool `json:"second_pass_required,omitempty"`
}

// Gate maps a verdict onto review-agent's paths. Precedence, most
// conservative first (R16: a small, fixable problem gets a fix round
// before a human is asked):
//
//  1. an Error → escalate (a human decides; the defects found so far are
//     shown);
//  2. a critical or major security defect (its category is one of
//     SecurityCategories, R18 review M2) → escalate: security fixes are
//     not left to an automatic round;
//  3. a real not_verifiable criterion (the model could not verify a code
//     criterion) → escalate, naming it — not one an invalid evidence
//     reference voided (R18, InvalidEvidence): like a recorded gap it
//     waits for step 5 and escalates at step 6;
//  4. overall escalate from the model → escalate;
//  5. any criterion not_met, any critical or major defect, or overall
//     request_changes → request_changes, the recorded gaps carried in the
//     reason — but only for a small, actionable fix (G2, fixRound): more
//     than FixMaxItems items, or an item with no file, line or check to
//     start from, escalates instead;
//  6. recorded gaps (non-code, unowned and PR-text criteria, R3/R8/R13:
//     not_verifiable criteria whose note is their own gap line in Gaps)
//     with everything else met → escalate for human review (R3
//     fail-closed: never approve with a recorded gap); likewise the
//     criteria an invalid evidence reference voided (R18);
//  7. overall approve with every criterion met → approve.
//
// A verdict that says approve while a criterion is not met or a major
// defect is listed is read as request_changes: the structured fields win
// over the summary word.
func Gate(v Verdict) Decision {
	var notVerifiable, realUnverifiable, gapCriteria, invalidCriteria, notMet []string
	gaps := recordedGapIDs(v)
	invalid := invalidEvidenceIDs(v)
	for _, r := range v.PerCriterion {
		switch r.Status {
		case StatusNotVerifiable:
			notVerifiable = append(notVerifiable, r.ID)
			switch {
			case gaps[r.ID]:
				gapCriteria = append(gapCriteria, r.ID)
			case invalid[r.ID]:
				invalidCriteria = append(invalidCriteria, r.ID)
			default:
				realUnverifiable = append(realUnverifiable, r.ID)
			}
		case StatusNotMet:
			notMet = append(notMet, r.ID)
		}
	}
	if v.Error != "" {
		reason := v.Error
		if len(notVerifiable) > 0 {
			reason += " (criteria " + strings.Join(notVerifiable, ", ") + ")"
		}
		return Decision{Action: ActionEscalate, Reason: reason + gapsSuffix(v)}
	}
	if sec := securityDefects(v.Defects); len(sec) > 0 {
		return Decision{Action: ActionEscalate, Reason: "security-sensitive defect(s) need a human, not an automatic fix round: " + strings.Join(sec, "; ") + gapsSuffix(v)}
	}
	if len(realUnverifiable) > 0 {
		return Decision{Action: ActionEscalate, Reason: "criteria the verifier could not verify: " + strings.Join(realUnverifiable, ", ") + gapsSuffix(v)}
	}
	if v.Overall == OverallEscalate {
		return Decision{Action: ActionEscalate, Reason: "the verifier escalated" + gapsSuffix(v)}
	}
	c := CountDefects(v.Defects)
	switch {
	case len(notMet) > 0:
		return fixRound(v, "criteria not met: "+strings.Join(notMet, ", "))
	case c.Critical > 0 || c.Major > 0:
		return fixRound(v, fmt.Sprintf("%d critical and %d major defect(s)", c.Critical, c.Major))
	case v.Overall == OverallRequestChanges:
		// Nothing blocking is listed: a fix round needs at least one
		// located defect to work from.
		for _, d := range v.Defects {
			if strings.TrimSpace(d.File) != "" {
				return Decision{Action: ActionRequestChanges, Reason: "the verifier requested changes" + gapsSuffix(v)}
			}
		}
		return Decision{Action: ActionEscalate, Reason: "verifier gave no actionable defect for its request_changes (no unmet criterion and no defect with a location)" + gapsSuffix(v)}
	}
	if len(gapCriteria) > 0 {
		return Decision{Action: ActionEscalate, Reason: "recorded gaps need human review (criteria " + strings.Join(gapCriteria, ", ") + "); everything else met" + gapsSuffix(v)}
	}
	if len(invalidCriteria) > 0 {
		// R18: fail closed — a criterion whose evidence was invalid is
		// never approved; a human checks it.
		return Decision{Action: ActionEscalate, Reason: "criteria whose evidence was invalid need human review (criteria " + strings.Join(invalidCriteria, ", ") + "); everything else met" + gapsSuffix(v)}
	}
	if v.Overall == OverallApprove {
		if len(v.PerCriterion) == 0 {
			return Decision{Action: ActionApprove, Reason: "no acceptance criteria to verify; no blocking defect found", SecondPassRequired: true}
		}
		return Decision{Action: ActionApprove, Reason: "every criterion met; no critical or major defect"}
	}
	return Decision{Action: ActionEscalate, Reason: fmt.Sprintf("unrecognised overall %q", v.Overall)}
}

// FixMaxItemsEnv bounds an automatic fix round (R16 G2): a verdict with more
// not_met criteria plus critical/major defects than this escalates instead.
const FixMaxItemsEnv = "VERIFIER_FIX_MAX_ITEMS"

// DefaultFixMaxItems is FixMaxItemsEnv's default.
const DefaultFixMaxItems = 5

// FixMaxItems reads FixMaxItemsEnv; unset, unparsable or below 1 is the
// default.
func FixMaxItems() int {
	if n, err := strconv.Atoi(strings.TrimSpace(os.Getenv(FixMaxItemsEnv))); err == nil && n >= 1 {
		return n
	}
	return DefaultFixMaxItems
}

// fixRound decides a verdict that asks for changes (not_met criteria or
// critical/major defects; security defects already escalated): a fix round
// only when it is small — at most FixMaxItems items — and actionable —
// every item carries a concrete reference the fix can start from.
// Otherwise a human decides, with the reason saying which bound failed.
func fixRound(v Verdict, what string) Decision {
	var items, vague []string
	for _, r := range v.PerCriterion {
		if r.Status != StatusNotMet {
			continue
		}
		items = append(items, r.ID)
		if !criterionActionable(r, v) {
			vague = append(vague, r.ID)
		}
	}
	for _, d := range v.Defects {
		if d.Severity != SeverityCritical && d.Severity != SeverityMajor {
			continue
		}
		label := fmt.Sprintf("%s defect %q", d.Severity, cut(oneLine(d.Category+": "+d.Description), 80))
		items = append(items, label)
		if strings.TrimSpace(d.File) == "" {
			vague = append(vague, label)
		}
	}
	if max := FixMaxItems(); len(items) > max {
		return Decision{Action: ActionEscalate, Reason: fmt.Sprintf("too many problems for an automatic fix round (%d not-met criteria and critical/major defects, limit %d; %s=%d): %s", len(items), max, FixMaxItemsEnv, max, what) + gapsSuffix(v)}
	}
	if len(vague) > 0 {
		return Decision{Action: ActionEscalate, Reason: "verifier gave no actionable defect for " + strings.Join(vague, ", ") + " (no file, line or check to start a fix from): " + what + gapsSuffix(v)}
	}
	return Decision{Action: ActionRequestChanges, Reason: what + gapsSuffix(v)}
}

// criterionActionable reports whether a not_met criterion carries a
// concrete reference a fix can start from: a path:line evidence ref, a
// repository path (with a directory or a :line) or a check (a backticked
// check kind or plan command, or a Go test name) named in its note, or a
// defect line with a location — a located critical/major
// defect that names the criterion, or names no criterion at all (the model
// usually explains a not_met criterion with an unattributed defect). A
// located defect that names only other criteria belongs to them.
func criterionActionable(r CriterionResult, v Verdict) bool {
	barePath := false
	for _, e := range r.Evidence {
		s := strings.TrimSpace(string(e))
		if fileLineRE.MatchString(s) {
			return true
		}
		if barePathRE.MatchString(s) {
			barePath = true
		}
	}
	if noteFileRE.MatchString(r.Note) || noteTestRE.MatchString(r.Note) || namesACheck(r.Note, v.planChecks) {
		return true
	}
	// R18: a bare-path citation (file-level evidence) is a lower grade: it
	// counts only when the note also names a line.
	if barePath && noteLineRE.MatchString(r.Note) {
		return true
	}
	idRE := regexp.MustCompile(`\b` + regexp.QuoteMeta(r.ID) + `\b`)
	for _, d := range v.Defects {
		if strings.TrimSpace(d.File) == "" || (d.Severity != SeverityCritical && d.Severity != SeverityMajor) {
			continue
		}
		text := d.Category + " " + d.Description
		if idRE.MatchString(text) || !criterionIDRE.MatchString(text) {
			return true
		}
	}
	return false
}

var (
	// noteFileRE: a repository path in prose — one with a directory
	// ("src/map.js"), or a bare file name with a common source extension
	// only with a :line suffix ("main.go:12"), so prose like "Node.js" is
	// not a file reference (review minor 3).
	noteFileRE = regexp.MustCompile(`(?i)(?:[\w.-]+/)+[\w.-]+\.[a-z][a-z0-9]{0,7}\b|\b[\w-]+\.(?:go|ts|tsx|js|jsx|mjs|cjs|py|rb|java|kt|rs|c|h|cc|cpp|hpp|cs|php|swift|vue|svelte|html|htm|css|scss|sql|sh|ya?ml|toml|json|md):[1-9][0-9]*\b`)
	// noteTestRE: a Go-style test function name ("TestPager_LastPage").
	noteTestRE = regexp.MustCompile(`\bTest[A-Z_]\w*`)
	// noteBacktickRE: a backticked span; it names a check only when it is
	// a check kind or a command the plan runs (namesACheck).
	noteBacktickRE = regexp.MustCompile("`([^`\n]{3,})`")
	// noteLineRE: a line named in prose ("line 42", "lines 10-12", ":42").
	noteLineRE = regexp.MustCompile(`(?i)\blines?\s+[1-9][0-9]*\b|:[1-9][0-9]*\b`)
	// criterionIDRE: a criterion id ("AC-3") in a defect's text.
	criterionIDRE = regexp.MustCompile(`\bAC-\d+\b`)
)

// checkKinds are the evidence check kinds a note may name in backticks.
var checkKinds = map[string]bool{
	"test": true, "build": true, "lint": true, "typecheck": true,
	"diagnostic": true, "migration": true, "api_compat": true,
}

// namesACheck reports whether a backticked span in note is a check kind or
// a command the plan runs (one is a prefix of the other, so `go test ./...
// -run X` names the plan's "go test ./..."); any other backticked text is
// not a reference a fix can start from (review minor 3).
func namesACheck(note string, planChecks []string) bool {
	for _, m := range noteBacktickRE.FindAllStringSubmatch(note, -1) {
		span := strings.Join(strings.Fields(m[1]), " ")
		if checkKinds[strings.ToLower(span)] {
			return true
		}
		for _, c := range planChecks {
			c = strings.Join(strings.Fields(c), " ")
			if c != "" && (strings.HasPrefix(span, c) || strings.HasPrefix(c, span)) {
				return true
			}
		}
	}
	return false
}

// cut shortens s to n runes ("…" appended).
func cut(s string, n int) string {
	if rs := []rune(s); len(rs) > n {
		return string(rs[:n]) + "…"
	}
	return s
}

// recordedGapIDs are the criteria that are recorded gaps (R3/R8/R13): a
// not_verifiable result whose note is one of v.Gaps and names its own id
// ("criterion AC-3 is …", plan.NonCodeGap). Gaps come only from the
// requirements (ParseVerdict clears the model's), so the model cannot turn
// a criterion it failed to verify into a gap.
func recordedGapIDs(v Verdict) map[string]bool {
	if len(v.Gaps) == 0 {
		return nil
	}
	lines := make(map[string]bool, len(v.Gaps))
	for _, g := range v.Gaps {
		lines[strings.TrimSpace(g)] = true
	}
	out := map[string]bool{}
	for _, r := range v.PerCriterion {
		note := strings.TrimSpace(r.Note)
		if r.Status == StatusNotVerifiable && lines[note] && strings.HasPrefix(note, "criterion "+r.ID+" ") {
			out[r.ID] = true
		}
	}
	return out
}

// SecurityCategories are the defect categories that escalate a critical or
// major defect to a human (R18 review M2). Any other category — including
// the pre-v7 unvalidated_security_path, which attempt 15's verifier used
// for an undefined variable — is an ordinary defect for the fix round.
var SecurityCategories = []string{"injection", "secrets", "authz", "ssrf", "path_traversal", "deserialization", "xss"}

// SecurityCategory reports whether category is exactly one of
// SecurityCategories, lower-cased with "-" and spaces read as "_".
func SecurityCategory(category string) bool {
	c := strings.ToLower(strings.TrimSpace(category))
	c = strings.NewReplacer("-", "_", " ", "_").Replace(c)
	for _, s := range SecurityCategories {
		if c == s {
			return true
		}
	}
	return false
}

// securityDefects names the critical and major defects whose category is a
// security one (SecurityCategory), as "[severity] category at file:line".
func securityDefects(ds []Defect) []string {
	var out []string
	for _, d := range ds {
		if d.Severity != SeverityCritical && d.Severity != SeverityMajor {
			continue
		}
		if !SecurityCategory(d.Category) {
			continue
		}
		s := fmt.Sprintf("[%s] %s", d.Severity, oneLine(d.Category))
		if loc := d.location(); loc != "" {
			s += " at " + loc
		}
		out = append(out, s)
	}
	return out
}

// gapsSuffix names the verdict's recorded gaps (non-code criteria, R3) for
// a decision reason; "" when there are none.
func gapsSuffix(v Verdict) string {
	s := ""
	if len(v.Gaps) > 0 {
		s = "; recorded gaps: " + strings.Join(v.Gaps, "; ")
	}
	var invalid []string
	ids := invalidEvidenceIDs(v)
	for _, r := range v.PerCriterion {
		if ids[r.ID] {
			invalid = append(invalid, r.ID)
		}
	}
	if len(invalid) > 0 {
		s += "; evidence ref invalid (a human checks): " + strings.Join(invalid, ", ")
	}
	return s
}

// invalidEvidenceIDs are the criteria an invalid evidence reference voided
// (R18): not_verifiable, listed in v.InvalidEvidence (set by Run, cleared
// from the model's answer) and noted "evidence ref invalid: …", so the
// model cannot turn a criterion it failed to verify into one.
func invalidEvidenceIDs(v Verdict) map[string]bool {
	if len(v.InvalidEvidence) == 0 {
		return nil
	}
	listed := make(map[string]bool, len(v.InvalidEvidence))
	for _, id := range v.InvalidEvidence {
		listed[id] = true
	}
	out := map[string]bool{}
	for _, r := range v.PerCriterion {
		if r.Status == StatusNotVerifiable && listed[r.ID] && strings.HasPrefix(r.Note, invalidEvidencePrefix) {
			out[r.ID] = true
		}
	}
	return out
}

// renderGaps writes the recorded-gaps section of a comment or feedback.
func renderGaps(b *strings.Builder, v Verdict, heading string) {
	if len(v.Gaps) == 0 {
		return
	}
	b.WriteString(heading)
	for _, g := range v.Gaps {
		fmt.Fprintf(b, "- %s\n", oneLine(g))
	}
}

// DefectCounts is the {critical, major, minor} object M4-05 reads from a
// review artifact's "defects" field (work_item_outcomes.review_defects).
type DefectCounts struct {
	Critical int `json:"critical"`
	Major    int `json:"major"`
	Minor    int `json:"minor"`
}

// CountDefects counts defects by severity.
func CountDefects(ds []Defect) DefectCounts {
	var c DefectCounts
	for _, d := range ds {
		switch d.Severity {
		case SeverityCritical:
			c.Critical++
		case SeverityMajor:
			c.Major++
		case SeverityMinor:
			c.Minor++
		}
	}
	return c
}

// Escalation is the verdict for a run that produced no parsed answer:
// every criterion not_verifiable with the reason, overall escalate.
func Escalation(criterionIDs []string, reason string, info ModelInfo) Verdict {
	v := Verdict{
		SchemaVersion: SchemaVersion,
		PerCriterion:  []CriterionResult{},
		Defects:       []Defect{},
		Overall:       OverallEscalate,
		ModelInfo:     info,
		Error:         reason,
	}
	for _, id := range criterionIDs {
		v.PerCriterion = append(v.PerCriterion, CriterionResult{ID: id, Status: StatusNotVerifiable, Evidence: []Ref{}, Note: reason})
	}
	return v
}

// location renders a defect's file:line ("" when it names no file).
func (d Defect) location() string {
	switch {
	case d.File == "":
		return ""
	case d.Line > 0:
		return fmt.Sprintf("%s:%d", d.File, d.Line)
	default:
		return d.File
	}
}

// RenderFeedback is the request-changes feedback review.complete carries to
// the implementation agent: the unmet criteria and the defects, each with
// its location and reproduction.
func RenderFeedback(v Verdict, d Decision) string {
	var b strings.Builder
	fmt.Fprintf(&b, "INDEPENDENT VERIFIER: %s (%s)\n", strings.ToUpper(string(d.Action)), d.Reason)
	var unmet []CriterionResult
	for _, r := range v.PerCriterion {
		if r.Status != StatusMet {
			unmet = append(unmet, r)
		}
	}
	if len(unmet) > 0 {
		b.WriteString("\nCriteria not satisfied:\n")
		for _, r := range unmet {
			fmt.Fprintf(&b, "- %s [%s]: %s\n", r.ID, r.Status, oneLine(r.Note))
		}
	}
	renderGaps(&b, v, "\nRecorded gaps (not code work; a human verifies them):\n")
	if len(v.Defects) > 0 {
		b.WriteString("\nDefects (fix every critical and major one):\n")
		for _, df := range v.Defects {
			loc := df.location()
			if loc == "" {
				loc = "(no file)"
			}
			fmt.Fprintf(&b, "- [%s] %s — %s: %s\n", df.Severity, loc, df.Category, oneLine(df.Description))
			if strings.TrimSpace(df.Reproduction) != "" {
				fmt.Fprintf(&b, "  Reproduction: %s\n", oneLine(df.Reproduction))
			}
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// RenderComment is the GitHub PR comment: the gate decision, the
// per-criterion table and the defects with file:line.
func RenderComment(v Verdict, d Decision) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## Independent verification: %s\n\n%s\n\n", strings.ReplaceAll(string(d.Action), "_", " "), d.Reason)
	if v.ModelInfo.SharedWithAuthor {
		b.WriteString("> The verifier ran on the review agent's model (no verifier-specific model is configured), so it is independent in context but not in model.\n\n")
	}
	if len(v.PerCriterion) > 0 {
		b.WriteString("| Criterion | Status | Evidence | Note |\n|---|---|---|---|\n")
		for _, r := range v.PerCriterion {
			refs := make([]string, 0, len(r.Evidence))
			for _, e := range r.Evidence {
				refs = append(refs, "`"+cell(string(e))+"`")
			}
			fmt.Fprintf(&b, "| %s | %s | %s | %s |\n", r.ID, statusLabel(r.Status), strings.Join(refs, " "), cell(r.Note))
		}
		b.WriteString("\n")
	} else {
		b.WriteString("_No acceptance criteria were recorded for this work item._\n\n")
	}
	if len(v.Gaps) > 0 {
		renderGaps(&b, v, "### Recorded gaps\n\nThese criteria are not code work; verify them manually.\n\n")
		b.WriteString("\n")
	}
	if len(v.Defects) > 0 {
		b.WriteString("### Defects\n\n")
		for _, df := range v.Defects {
			loc := df.location()
			if loc != "" {
				loc = "`" + loc + "` "
			}
			fmt.Fprintf(&b, "- **%s** %s%s: %s", df.Severity, loc, cell(df.Category), oneLine(df.Description))
			if strings.TrimSpace(df.Reproduction) != "" {
				fmt.Fprintf(&b, " — reproduce: %s", oneLine(df.Reproduction))
			}
			b.WriteString("\n")
		}
	} else {
		b.WriteString("No defects reported.\n")
	}
	if v.Error != "" {
		fmt.Fprintf(&b, "\n_Verifier error: %s_\n", oneLine(v.Error))
	}
	if v.ModelInfo.ModelID != "" {
		fmt.Fprintf(&b, "\n<sub>verifier %s · %s/%s</sub>\n", Version, v.ModelInfo.Provider, v.ModelInfo.ModelID)
	}
	return b.String()
}

func statusLabel(s Status) string {
	switch s {
	case StatusMet:
		return "met"
	case StatusNotMet:
		return "**not met**"
	default:
		return "_not verifiable_"
	}
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func cell(s string) string {
	return strings.ReplaceAll(oneLine(s), "|", `\|`)
}
