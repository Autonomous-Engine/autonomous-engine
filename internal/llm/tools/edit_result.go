package tools

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// R10 W2: edit_file's results help the model recover. A live run (attempt 9)
// re-applied two edits that "succeeded" again (duplicated compose and docs
// lines), retried a bare "not found" nine times, and never learned where
// an applied edit landed.

// lineAt is the 1-based line of byte offset off in content.
func lineAt(content string, off int) int { return strings.Count(content[:off], "\n") + 1 }

// occurrences are the byte offsets of every non-overlapping match.
func occurrences(content, needle string) []int {
	var out []int
	for from := 0; ; {
		i := strings.Index(content[from:], needle)
		if i < 0 {
			return out
		}
		out = append(out, from+i)
		from += i + len(needle)
	}
}

// coveredByNew reports whether the old_string match at idx sits inside an
// existing copy of new_string (new_string extends old_string and is already
// there), with the line new_string starts at.
func coveredByNew(content string, idx int, oldString, newString string) (int, bool) {
	k := strings.Index(newString, oldString)
	if k < 0 || newString == oldString {
		return 0, false
	}
	start := idx - k
	if start < 0 || !strings.HasPrefix(content[start:], newString) {
		return 0, false
	}
	return lineAt(content, start), true
}

// nearestOccurrence is the match whose line is closest to line (ties: the
// first).
func nearestOccurrence(content string, occ []int, line int) int {
	best, bestD := occ[0], -1
	for _, o := range occ {
		d := lineAt(content, o) - line
		if d < 0 {
			d = -d
		}
		if bestD < 0 || d < bestD {
			best, bestD = o, d
		}
	}
	return best
}

// replacedTextPresent reports an edit whose old_string is gone and whose
// new_string is already in the file once: applied earlier. It is strict:
// new_string must not be part of old_string, must have at least 2
// meaningful lines, and no line old_string removes may still be in the file
// (a whitespace-mismatched old_string whose text is still there is "not
// found", never "already applied").
func replacedTextPresent(content, oldString, newString string) bool {
	if strings.TrimSpace(newString) == "" || strings.Contains(oldString, newString) || strings.Count(content, newString) != 1 {
		return false
	}
	meaningful := 0
	newLines := map[string]bool{}
	for _, l := range strings.Split(newString, "\n") {
		t := strings.TrimSpace(l)
		newLines[t] = true
		if len(t) >= 8 && !isPurePunctuation(t) {
			meaningful++
		}
	}
	if meaningful < 2 {
		return false
	}
	for _, l := range strings.Split(oldString, "\n") {
		t := strings.TrimSpace(l)
		if t == "" || newLines[t] {
			continue
		}
		if strings.Contains(content, t) {
			return false
		}
	}
	return true
}

func alreadyAppliedResult(path string, line int) string {
	return fmt.Sprintf("Edit to %s already applied at line %d — no change (new_string is already there).", path, line)
}

// notFoundMessage is the "not found" result with the nearest candidate
// lines: where old_string's first line does occur (and where it then
// stops matching), else up to 3 lines with the same first 20 chars or a
// similarity of at least 0.8.
func notFoundMessage(path, content, oldString string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "old_string not found in %s. It must match exactly, whitespace and indentation included.", path)
	fileLines := strings.Split(content, "\n")
	oldLines := strings.Split(oldString, "\n")
	first, firstIdx := "", -1
	for i, l := range oldLines {
		if t := strings.TrimSpace(l); t != "" {
			first, firstIdx = t, i
			break
		}
	}
	if first == "" {
		return b.String()
	}
	// old_string's first line is in the file: say where the rest diverges.
	var partial []string
	for i, fl := range fileLines {
		if strings.TrimSpace(fl) != first || len(partial) >= 3 {
			continue
		}
		k := firstIdx + 1
		j := i + 1
		for k < len(oldLines) && j < len(fileLines) && oldLines[k] == fileLines[j] {
			k++
			j++
		}
		switch {
		case fl != oldLines[firstIdx]:
			partial = append(partial, fmt.Sprintf("  %d: %s   (same text, different indentation)", i+1, truncateRunes(fl, 200)))
		case j < len(fileLines) && k < len(oldLines):
			partial = append(partial, fmt.Sprintf("  %d: %s   (old_string line %d differs: the file has %d: %s)", i+1, truncateRunes(fl, 200), k+1, j+1, truncateRunes(fileLines[j], 200)))
		default:
			partial = append(partial, fmt.Sprintf("  %d: %s", i+1, truncateRunes(fl, 200)))
		}
	}
	if len(partial) > 0 {
		b.WriteString(" Its first line is in the file:\n")
		b.WriteString(strings.Join(partial, "\n"))
		return b.String()
	}
	type cand struct {
		line  int
		text  string
		score float64
	}
	var cands []cand
	prefix := first
	if len(prefix) > 20 {
		prefix = prefix[:20]
	}
	for i, fl := range fileLines {
		t := strings.TrimSpace(fl)
		if t == "" {
			continue
		}
		s := similarity(first, t)
		if len(first) >= 20 && strings.HasPrefix(t, prefix) && s < 0.8 {
			s = 0.8
		}
		if s >= 0.8 {
			cands = append(cands, cand{i + 1, fl, s})
		}
	}
	sort.SliceStable(cands, func(i, j int) bool { return cands[i].score > cands[j].score })
	if len(cands) > 3 {
		cands = cands[:3]
	}
	if len(cands) == 0 {
		b.WriteString(" No similar line is in the file: read_file it again and copy old_string from the result.")
		return b.String()
	}
	sort.SliceStable(cands, func(i, j int) bool { return cands[i].line < cands[j].line })
	b.WriteString(" Nearest lines:\n")
	for _, c := range cands {
		fmt.Fprintf(&b, "  %d: %s\n", c.line, truncateRunes(strings.TrimRight(c.text, " \t\r"), 200))
	}
	return strings.TrimRight(b.String(), "\n")
}

// similarity is the Dice coefficient of the two strings' character
// bigrams (1 for equal strings).
func similarity(a, b string) float64 {
	if a == b {
		return 1
	}
	if len(a) > 300 {
		a = a[:300]
	}
	if len(b) > 300 {
		b = b[:300]
	}
	if len(a) < 2 || len(b) < 2 {
		return 0
	}
	grams := map[string]int{}
	for i := 0; i+1 < len(a); i++ {
		grams[a[i:i+2]]++
	}
	common := 0
	for i := 0; i+1 < len(b); i++ {
		g := b[i : i+2]
		if grams[g] > 0 {
			grams[g]--
			common++
		}
	}
	return 2 * float64(common) / float64(len(a)-1+len(b)-1)
}

var (
	appliedLinesRE = regexp.MustCompile(`at lines (\d+)-(\d+)`)
	alreadyLineRE  = regexp.MustCompile(`already applied at line (\d+)`)
	foundTimesRE   = regexp.MustCompile(`found (\d+) times`)
)

// editLedgerLine is an edit_file / write_file ledger line: the path and the
// outcome class, never the new_string (attempt 9's ledger rendered
// `edit_file("new_string":"## Configuration…) -> error: not found`, the
// path cut away).
func editLedgerLine(tool, args, content string, isError bool) string {
	var a map[string]any
	_ = json.Unmarshal([]byte(args), &a)
	p, _ := a["path"].(string)
	head := p
	if old, _ := a["old_string"].(string); old != "" {
		firstLine := strings.SplitN(strings.TrimSpace(old), "\n", 2)[0]
		head += ", old:" + strconv.Quote(truncateRunes(firstLine, 40))
	}
	return fmt.Sprintf("- %s(%s) -> %s", tool, head, editOutcome(tool, content, isError))
}

func editOutcome(tool, content string, isError bool) string {
	if !isError {
		if m := alreadyLineRE.FindStringSubmatch(content); m != nil {
			return "already-applied L" + m[1]
		}
		if m := appliedLinesRE.FindStringSubmatch(content); m != nil {
			return "applied L" + m[1] + "-" + m[2]
		}
		if tool == "write_file" {
			return "written"
		}
		return "applied"
	}
	lc := strings.ToLower(content)
	switch {
	case foundTimesRE.MatchString(content):
		return "ambiguous(" + foundTimesRE.FindStringSubmatch(content)[1] + ")"
	case strings.Contains(lc, "would duplicate a block"):
		return "rejected: dup-block"
	case strings.Contains(lc, "suspicious duplicates"):
		return "rejected: duplicates"
	case strings.Contains(lc, "unparsable"):
		return "rejected: syntax"
	case strings.Contains(lc, "read_file must be called"):
		return "rejected: not read"
	case strings.Contains(lc, "read partially"):
		return "rejected: partial read"
	case strings.Contains(lc, "no longer available"):
		return "refused: tool removed"
	case strings.Contains(lc, "not found"):
		return "not-found"
	case strings.HasPrefix(content, "REJECTED"):
		return "rejected: " + truncateRunes(strings.TrimSpace(strings.TrimPrefix(firstLineOf(content), "REJECTED:")), 60)
	}
	return "error: " + errorClass(content)
}

func firstLineOf(s string) string { return strings.SplitN(strings.TrimSpace(s), "\n", 2)[0] }
