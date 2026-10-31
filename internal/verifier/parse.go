package verifier

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// fileLineRE is a "path:line" or "path:start-end" evidence reference.
var fileLineRE = regexp.MustCompile(`^[^\s:]+:[1-9][0-9]*(-[1-9][0-9]*)?$`)

// ParseVerdict strictly decodes a model answer into a Verdict and checks it
// against the criteria the verifier was given. The answer must be one JSON
// object (optionally inside a single ```json fence) and nothing else; unknown
// fields, unknown enum values, a missing, unknown or repeated criterion id,
// a "met" without evidence, and an evidence reference that is not valid
// (see validRef: one of g.CallIDs, a path:line reference — for a met, of a
// file that exists at head when g.PathExists is set — or, R18, the bare
// path of a file that exists at head) are all rejected with a reason the
// re-prompt can show the model. ModelInfo, Error, Gaps and InvalidEvidence
// are not the model's to set; they are cleared.
//
// Run uses parseVerdict instead, which keeps an invalid reference to its
// own criterion (R18).
func ParseVerdict(content string, criterionIDs []string, g Grounding) (Verdict, error) {
	v, probs, err := parseVerdict(content, criterionIDs, g)
	if err != nil {
		return Verdict{}, err
	}
	if len(probs) > 0 {
		return Verdict{}, errors.New(probs[0].Error())
	}
	return v, nil
}

// RefProblem is one evidence reference of a criterion result that is not
// valid (R18): it invalidates that criterion, not the verdict.
type RefProblem struct {
	// Index is the result's position in per_criterion; ID its criterion.
	Index int
	ID    string
	Ref   Ref
	// Reason completes `evidence "<ref>" …`.
	Reason string
}

func (p RefProblem) Error() string {
	return fmt.Sprintf("per_criterion[%d] (%s): evidence %q %s", p.Index, p.ID, string(p.Ref), p.Reason)
}

// parseVerdict is ParseVerdict that returns the invalid evidence
// references separately: a structural problem (not one JSON object, an
// unknown field or enum value, a missing / unknown / repeated criterion, a
// non-met without a note, a malformed defect) is the error; a reference
// that is not valid is a RefProblem of its own criterion.
func parseVerdict(content string, criterionIDs []string, g Grounding) (Verdict, []RefProblem, error) {
	raw, err := extractJSONObject(content)
	if err != nil {
		return Verdict{}, nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var v Verdict
	if err := dec.Decode(&v); err != nil {
		return Verdict{}, nil, fmt.Errorf("decode verdict: %w", err)
	}
	if dec.More() {
		return Verdict{}, nil, errors.New("more than one JSON value in the answer")
	}
	v.ModelInfo = ModelInfo{}
	v.Error = ""
	v.Gaps = nil
	v.InvalidEvidence = nil
	probs, err := v.validate(criterionIDs, g)
	if err != nil {
		return Verdict{}, nil, err
	}
	if v.Defects == nil {
		v.Defects = []Defect{}
	}
	return v, probs, nil
}

// invalidEvidencePrefix opens the note of a criterion an invalid evidence
// reference voided (R18); the gate reads it with Verdict.InvalidEvidence.
const invalidEvidencePrefix = "evidence ref invalid: "

// voidInvalidRefs marks every criterion with a RefProblem not_verifiable:
// its invalid references are dropped, its note names them and keeps what
// the model said, and its id is listed in InvalidEvidence. The other
// criteria, the defects and overall are untouched.
func voidInvalidRefs(v Verdict, probs []RefProblem) Verdict {
	if len(probs) == 0 {
		return v
	}
	byIndex := map[int][]RefProblem{}
	for _, p := range probs {
		byIndex[p.Index] = append(byIndex[p.Index], p)
	}
	results := append([]CriterionResult(nil), v.PerCriterion...)
	for i, ps := range byIndex {
		if i < 0 || i >= len(results) {
			continue
		}
		r := results[i]
		bad := map[Ref]bool{}
		reasons := make([]string, 0, len(ps))
		for _, p := range ps {
			bad[p.Ref] = true
			reasons = append(reasons, fmt.Sprintf("%q %s", string(p.Ref), p.Reason))
		}
		kept := []Ref{}
		for _, e := range r.Evidence {
			if !bad[e] {
				kept = append(kept, e)
			}
		}
		note := invalidEvidencePrefix + strings.Join(reasons, "; ")
		if said := strings.TrimSpace(r.Note); said != "" {
			note += fmt.Sprintf(" (the verifier said %s: %s)", r.Status, said)
		} else {
			note += fmt.Sprintf(" (the verifier said %s)", r.Status)
		}
		results[i] = CriterionResult{ID: r.ID, Status: StatusNotVerifiable, Evidence: kept, Note: note}
		v.InvalidEvidence = append(v.InvalidEvidence, r.ID)
	}
	v.PerCriterion = results
	return v
}

// invalidRefsReprompt is the re-prompt for an answer whose only problem is
// invalid evidence: it names just those citations.
func invalidRefsReprompt(probs []RefProblem) string {
	var b strings.Builder
	b.WriteString("Your answer was rejected only for these evidence references:\n")
	for _, p := range probs {
		fmt.Fprintf(&b, "- %s: %q %s\n", p.ID, string(p.Ref), p.Reason)
	}
	b.WriteString("Each evidence entry must be a tool call id from this run, a path:line reference, or the path of a file that exists at head. " +
		"Reply again with ONLY the JSON object described under Output, with just these citations corrected (or that criterion marked not_verifiable with a note). " +
		"Keep everything else as it was. Do not call more tools unless you must.")
	return b.String()
}

// extractJSONObject returns the answer's JSON object: the whole trimmed
// answer, or the body of one ```/```json fence that is the whole answer.
func extractJSONObject(content string) ([]byte, error) {
	s := strings.TrimSpace(content)
	if strings.HasPrefix(s, "```") {
		nl := strings.IndexByte(s, '\n')
		if nl < 0 || !strings.HasSuffix(s, "```") {
			return nil, errors.New("answer opens a code fence but is not exactly one fenced JSON object")
		}
		lang := strings.TrimSpace(s[3:nl])
		if lang != "" && lang != "json" {
			return nil, fmt.Errorf("answer is fenced as %q, want json", lang)
		}
		s = strings.TrimSpace(s[nl+1 : len(s)-3])
	}
	if s == "" {
		return nil, errors.New("empty answer")
	}
	if !strings.HasPrefix(s, "{") || !strings.HasSuffix(s, "}") {
		return nil, errors.New("answer is not a single JSON object (it must start with { and end with }, with no prose around it)")
	}
	return []byte(s), nil
}

// validate returns a structural problem as the error and the invalid
// evidence references (R18) as RefProblems.
func (v Verdict) validate(criterionIDs []string, g Grounding) ([]RefProblem, error) {
	var probs []RefProblem
	if v.SchemaVersion != SchemaVersion {
		return nil, fmt.Errorf("schema_version is %d, want %d", v.SchemaVersion, SchemaVersion)
	}
	switch v.Overall {
	case OverallApprove, OverallRequestChanges, OverallEscalate:
	default:
		return nil, fmt.Errorf("overall %q is not approve, request_changes or escalate", v.Overall)
	}
	want := map[string]bool{}
	for _, id := range criterionIDs {
		want[id] = true
	}
	seen := map[string]bool{}
	for i, r := range v.PerCriterion {
		if !want[r.ID] {
			return nil, fmt.Errorf("per_criterion[%d]: %q is not one of the criteria given (%s)", i, r.ID, strings.Join(criterionIDs, ", "))
		}
		if seen[r.ID] {
			return nil, fmt.Errorf("per_criterion[%d]: %s appears more than once", i, r.ID)
		}
		seen[r.ID] = true
		switch r.Status {
		case StatusMet, StatusNotMet, StatusNotVerifiable:
		default:
			return nil, fmt.Errorf("per_criterion[%d] (%s): status %q is not met, not_met or not_verifiable", i, r.ID, r.Status)
		}
		if r.Status == StatusMet && len(r.Evidence) == 0 {
			return nil, fmt.Errorf("per_criterion[%d] (%s): met requires evidence (tool call ids or path:line references)", i, r.ID)
		}
		if r.Status != StatusMet && strings.TrimSpace(r.Note) == "" {
			return nil, fmt.Errorf("per_criterion[%d] (%s): %s requires a note saying why", i, r.ID, r.Status)
		}
		// R18: an invalid reference is a problem of this criterion only.
		for _, ref := range r.Evidence {
			if !validRef(ref, g) {
				probs = append(probs, RefProblem{Index: i, ID: r.ID, Ref: ref, Reason: "is neither a tool call id from this run, a path:line reference nor the path of a file at head"})
				continue
			}
			// A met criterion's path:line evidence must name a file that
			// exists at head (a not_met may well cite a file that is
			// missing). A bare path is valid only when it exists.
			s := strings.TrimSpace(string(ref))
			if r.Status == StatusMet && !g.CallIDs[s] && g.PathExists != nil && fileLineRE.MatchString(s) {
				if p := refPath(ref); !g.PathExists(p) {
					probs = append(probs, RefProblem{Index: i, ID: r.ID, Ref: ref, Reason: fmt.Sprintf("names %s, which does not exist in the tree at head", p)})
				}
			}
		}
	}
	var missing []string
	for _, id := range criterionIDs {
		if !seen[id] {
			missing = append(missing, id)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("per_criterion has no result for %s; every criterion needs exactly one", strings.Join(missing, ", "))
	}
	for i, d := range v.Defects {
		switch d.Severity {
		case SeverityCritical, SeverityMajor, SeverityMinor:
		default:
			return nil, fmt.Errorf("defects[%d]: severity %q is not critical, major or minor", i, d.Severity)
		}
		if strings.TrimSpace(d.Description) == "" {
			return nil, fmt.Errorf("defects[%d]: description is empty", i)
		}
		if strings.TrimSpace(d.Category) == "" {
			return nil, fmt.Errorf("defects[%d]: category is empty", i)
		}
		if d.Line < 0 {
			return nil, fmt.Errorf("defects[%d]: line %d is negative", i, d.Line)
		}
		if d.Line > 0 && strings.TrimSpace(d.File) == "" {
			return nil, fmt.Errorf("defects[%d]: a line without a file", i)
		}
	}
	return probs, nil
}

// refPath is the path of a path:line reference.
func refPath(ref Ref) string {
	s := strings.TrimSpace(string(ref))
	if i := strings.LastIndexByte(s, ':'); i > 0 {
		return s[:i]
	}
	return s
}

// Grounding is what evidence references are checked against.
type Grounding struct {
	// CallIDs are the successful tool call ids of the verifier's run.
	CallIDs map[string]bool
	// PathExists reports whether a repository-relative path exists in the
	// tree at head; nil skips the check.
	PathExists func(path string) bool
}

// TreePathExists is a Grounding.PathExists over the tree at root: the path
// must stay inside root and name a regular file (not ".", "..", or a
// directory: R18 review m2).
func TreePathExists(root string) func(string) bool {
	return func(p string) bool {
		p = filepath.FromSlash(strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(p), "./"), "/"))
		if p == "" || filepath.IsAbs(p) {
			return false
		}
		clean := filepath.Clean(p)
		if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			return false
		}
		// R18 review m2: a regular file — a directory is not evidence.
		fi, err := os.Stat(filepath.Join(root, clean))
		return err == nil && fi.Mode().IsRegular()
	}
}

// validRef: a tool call id of this run, a path:line reference, or (R18)
// the bare path of a file that exists at head — file-level evidence, a
// lower grade than path:line (see criterionActionable).
func validRef(ref Ref, g Grounding) bool {
	s := strings.TrimSpace(string(ref))
	if s == "" {
		return false
	}
	if g.CallIDs[s] {
		return true
	}
	if fileLineRE.MatchString(s) {
		return true
	}
	return isBarePathRef(s, g)
}

// barePathRE is a repository path with no line: no spaces or colon, and a
// directory or an extension ("tests/unit/server.test.ts", "README.md").
var barePathRE = regexp.MustCompile(`^[^\s:]*[/.][^\s:]*$`)

// isBarePathRef reports whether s is the bare path of a file at head.
func isBarePathRef(s string, g Grounding) bool {
	if s == "." || s == ".." || strings.HasPrefix(s, "-") {
		return false
	}
	return g.PathExists != nil && barePathRE.MatchString(s) && g.PathExists(s)
}
