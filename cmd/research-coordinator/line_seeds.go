package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
)

// Line seeds (R10 W5): the file + line evidence research found for the
// change, carried through the brief to the implementation task's
// "## Where to edit" section. In a live run (attempt 9) the code_trace
// evidence had `public/index.html` Lines 836-853 with an `L.tileLayer(…)`
// tile-URL snippet; the brief kept only the paths, the model never read
// line 840 (read_file stops at 400 lines) and invented a `[data-tile-url]`
// selector instead of editing the tile URLs.

// lineSeed is one place to edit. Snippet is at most maxSeedSnippetLines
// lines; when Verified it was re-read from the repository clone and each
// line is prefixed with its number ("840: …").
type lineSeed struct {
	Path      string `json:"path"`
	LineStart int    `json:"line_start"`
	LineEnd   int    `json:"line_end"`
	Snippet   string `json:"snippet,omitempty"`
	Verified  bool   `json:"verified,omitempty"`
}

const (
	maxLineSeeds        = 8
	maxSeedSnippetLines = 5
	// seedWholeFileSpan: a range from line 1 at least this long is "the
	// whole file", not a place to edit.
	seedWholeFileSpan = 20
	// seedMaxSpan: longer ranges are not seeds.
	seedMaxSpan = 80
	// seedAnchorSlack is how far outside the claimed range the snippet's
	// first line may be found.
	seedAnchorSlack = 60
	// seedWholeRangeLines: a range this short is quoted whole (R12 X3);
	// a longer one quotes at most seedLongRangeLines lines.
	seedWholeRangeLines = 20
	seedLongRangeLines  = 3
)

var (
	// "**File:** `public/index.html`", "File: public/index.html"
	seedFileMarker = regexp.MustCompile("(?i)^\\s*[-*]*\\s*\\**file\\**:?\\**\\s*:?\\s*`?([A-Za-z0-9_@./\\-]+\\.[A-Za-z][A-Za-z0-9]{0,7})`?")
	// "**Lines:** 836-853", "Line: 855"
	seedLinesMarker = regexp.MustCompile(`(?i)^\s*[-*]*\s*\**lines?\**:?\**\s*:?\s*(\d+)(?:\s*[-–]\s*(\d+))?\s*$`)
	// "public/index.html:836-853" inline.
	seedInlineRef = regexp.MustCompile("(?:^|[\\s(`'\"])((?:[A-Za-z0-9_@.\\-]+/)*[A-Za-z0-9_@.\\-]+\\.[A-Za-z][A-Za-z0-9]{0,7}):(\\d+)(?:-(\\d+))?")
	// "public/index.html (line 840)", "`landing/index.html` at lines
	// 855-860", "src/server/index.ts, line ~60" in prose (R11 S1a): a path
	// with a directory, then right after it "line(s) N[-M]".
	seedProseRef = regexp.MustCompile("(?i)(?:^|[\\s(`'\"])((?:[A-Za-z0-9_@.\\-]+/)+[A-Za-z0-9_@.\\-]+\\.[A-Za-z][A-Za-z0-9]{0,7})[`'\"]?[\\s,(:]{1,3}(?:(?:at|on|near|around)\\s+)?lines?\\s+~?(\\d+)(?:\\s*[-–]\\s*~?(\\d+))?")
	// seedFileName: a path from JSON must name a file.
	seedFileName = regexp.MustCompile(`[A-Za-z0-9_\-]\.[A-Za-z][A-Za-z0-9]{0,7}$`)
)

// seedContext is what the coordinator knows besides the evidence (R11
// S1): the intake's own verified line map (R2 seed_lines, checked against
// the clone when research was dispatched) and the intake text, whose words
// pick the lines a seed quotes when research gave no snippet.
type seedContext struct {
	// intakeFiles orders intakeLines (the seed files in intake order).
	intakeFiles []string
	intakeLines map[string][]int
	// hintText is the intake description and the PRD.
	hintText string
}

// extractLineSeeds reads the line seeds out of the ok results' evidence, in
// first-seen order, verified against the first clone that has the file.
func extractLineSeeds(results []researchResultMessage, clones []string) []lineSeed {
	return extractLineSeedsWith(results, clones, seedContext{})
}

// extractLineSeedsWith is extractLineSeeds with the intake's context:
// research's seeds first; then, for every file of the intake's verified
// line map research gave no seed for, the intake's lines (R11 S1b). Every
// seed is re-verified against the clone, and its quoted lines come from
// the file (S1c).
func extractLineSeedsWith(results []researchResultMessage, clones []string, sc seedContext) []lineSeed {
	var out []lineSeed
	seen := map[string]bool{}
	pick := newSeedPicker(results, sc.hintText)
	// overlaps: a seed of the same file already covers part of s's range.
	overlaps := func(s lineSeed) bool {
		for _, o := range out {
			if o.Path == s.Path && s.LineStart <= o.LineEnd && o.LineStart <= s.LineEnd {
				return true
			}
		}
		return false
	}
	add := func(s lineSeed) {
		if len(out) >= maxLineSeeds || s.Path == "" || s.LineStart <= 0 {
			return
		}
		if s.LineEnd < s.LineStart {
			s.LineEnd = s.LineStart
		}
		span := s.LineEnd - s.LineStart + 1
		if span > seedMaxSpan || (s.LineStart == 1 && span >= seedWholeFileSpan) || overlaps(s) {
			return
		}
		s = verifySeedWith(s, clones, pick)
		key := s.Path + ":" + strconv.Itoa(s.LineStart) + "-" + strconv.Itoa(s.LineEnd)
		if seen[key] {
			return
		}
		seen[key] = true
		out = append(out, s)
	}
	// The intake's verified lines first: they are ground truth and their
	// slots are reserved (R11 review: a codex_grep hit list filled all 8
	// slots before the fallback ran). Research's seeds follow, up to the
	// cap; one that overlaps a seed already taken adds nothing.
	for _, p := range sc.intakeFiles {
		for _, s := range intakeLineSeeds(p, sc.intakeLines[p]) {
			add(s)
		}
	}
	for _, r := range results {
		if r.Status != ResearchStatusOK {
			continue
		}
		for _, s := range parseLineSeeds(r.Evidence) {
			add(s)
		}
	}
	return out
}

// intakeLineSeeds turns the intake's line numbers for one file into
// ranges: lines less than seedMaxSpan apart share one ("lines 836-853" →
// 836-853), a lone line is its own.
func intakeLineSeeds(path string, lines []int) []lineSeed {
	sorted := append([]int(nil), lines...)
	sort.Ints(sorted)
	var out []lineSeed
	for _, n := range sorted {
		if n <= 0 {
			continue
		}
		if k := len(out) - 1; k >= 0 && n-out[k].LineStart < seedMaxSpan {
			out[k].LineEnd = n
			continue
		}
		out = append(out, lineSeed{Path: path, LineStart: n, LineEnd: n})
	}
	return out
}

// parseLineSeeds finds "File: x / Lines: a-b" blocks (with the code fence
// that follows as the snippet), inline "path:a-b" references, prose
// references ("public/index.html (line 840)", R11 S1a) and the read-tool
// JSON research quotes: a codex_read_file / read_file result or call
// ({"path": …, "start_line": …, "end_line": …}). A live run (attempt 11)
// held its tile-layer lines only in that JSON form.
func parseLineSeeds(evidence string) []lineSeed {
	return append(parseTextSeeds(evidence), parseJSONSeeds(evidence)...)
}

func parseTextSeeds(evidence string) []lineSeed {
	lines := strings.Split(strings.ReplaceAll(evidence, "\r\n", "\n"), "\n")
	var out []lineSeed
	file := ""
	var cur *lineSeed
	flush := func() {
		if cur != nil {
			out = append(out, *cur)
			cur = nil
		}
	}
	for i := 0; i < len(lines); i++ {
		ln := lines[i]
		if m := seedFileMarker.FindStringSubmatch(ln); m != nil {
			flush()
			file = normalizeSeedPath(m[1])
			continue
		}
		if m := seedLinesMarker.FindStringSubmatch(ln); m != nil && file != "" {
			flush()
			a, _ := strconv.Atoi(m[1])
			b := a
			if m[2] != "" {
				b, _ = strconv.Atoi(m[2])
			}
			cur = &lineSeed{Path: file, LineStart: a, LineEnd: b}
			continue
		}
		if strings.HasPrefix(strings.TrimSpace(ln), "```") {
			// A fence: its body is the current seed's snippet.
			var body []string
			j := i + 1
			for ; j < len(lines) && !strings.HasPrefix(strings.TrimSpace(lines[j]), "```"); j++ {
				body = append(body, lines[j])
			}
			if cur != nil && cur.Snippet == "" {
				cur.Snippet = snippetOf(body)
			}
			i = j
			continue
		}
		for _, m := range seedInlineRef.FindAllStringSubmatch(ln, -1) {
			a, _ := strconv.Atoi(m[2])
			b := a
			if m[3] != "" {
				b, _ = strconv.Atoi(m[3])
			}
			out = append(out, lineSeed{Path: normalizeSeedPath(m[1]), LineStart: a, LineEnd: b})
		}
		for _, m := range seedProseRef.FindAllStringSubmatch(ln, -1) {
			a, _ := strconv.Atoi(m[2])
			b := a
			if m[3] != "" {
				b, _ = strconv.Atoi(m[3])
			}
			out = append(out, lineSeed{Path: normalizeSeedPath(m[1]), LineStart: a, LineEnd: b})
		}
	}
	flush()
	return out
}

// maxJSONSeedScan bounds how much evidence the JSON scan reads.
const maxJSONSeedScan = 256 << 10

// parseJSONSeeds decodes every JSON object in the evidence and keeps those
// (at any depth) that name a file and a first line: "path" or "file" with
// "start_line" or "line", and "end_line" when given. A read result's
// "content" is not kept: the quoted lines always come from the clone (S1c).
func parseJSONSeeds(evidence string) []lineSeed {
	if len(evidence) > maxJSONSeedScan {
		evidence = evidence[:maxJSONSeedScan]
	}
	var out []lineSeed
	// lineOnly: the paths that already have a seed from a line-only object
	// ({file, line}: a codex_grep hit, R11 review) — one per path.
	lineOnly := map[string]bool{}
	for i := 0; i < len(evidence); {
		j := strings.IndexByte(evidence[i:], '{')
		if j < 0 {
			break
		}
		i += j
		dec := json.NewDecoder(strings.NewReader(evidence[i:]))
		dec.UseNumber()
		var v any
		if err := dec.Decode(&v); err != nil {
			i++
			continue
		}
		walkJSONSeeds(v, &out, lineOnly)
		i += max(int(dec.InputOffset()), 1)
	}
	return out
}

func walkJSONSeeds(v any, out *[]lineSeed, lineOnly map[string]bool) {
	switch t := v.(type) {
	case []any:
		for _, e := range t {
			walkJSONSeeds(e, out, lineOnly)
		}
	case map[string]any:
		p, _ := t["path"].(string)
		if p == "" {
			p, _ = t["file"].(string)
		}
		start := jsonSeedInt(t["start_line"])
		single := false
		if start == 0 {
			start, single = jsonSeedInt(t["line"]), true
		}
		if p = normalizeSeedPath(strings.TrimSpace(p)); p != "" && start > 0 && seedFileName.MatchString(p) && !(single && lineOnly[p]) {
			end := jsonSeedInt(t["end_line"])
			if end < start {
				end = start
			}
			if single {
				lineOnly[p] = true
			}
			*out = append(*out, lineSeed{Path: p, LineStart: start, LineEnd: end})
		}
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			walkJSONSeeds(t[k], out, lineOnly)
		}
	}
}

func jsonSeedInt(v any) int {
	n, ok := v.(json.Number)
	if !ok {
		return 0
	}
	i, err := n.Int64()
	if err != nil || i <= 0 || i > 1_000_000 {
		return 0
	}
	return int(i)
}

// snippetOf is a fence body dedented to its common indentation, without
// placeholder lines ("// ...") and blank lines, at most 5 lines.
func snippetOf(body []string) string {
	var keep []string
	for _, l := range body {
		t := strings.TrimSpace(l)
		if t == "" || strings.Contains(t, "...") && (strings.HasPrefix(t, "//") || strings.HasPrefix(t, "#")) {
			continue
		}
		keep = append(keep, strings.TrimRight(l, " \t"))
	}
	indent := -1
	for _, l := range keep {
		n := len(l) - len(strings.TrimLeft(l, " \t"))
		if indent < 0 || n < indent {
			indent = n
		}
	}
	for i := range keep {
		if indent > 0 && len(keep[i]) >= indent {
			keep[i] = keep[i][indent:]
		}
	}
	if len(keep) > maxSeedSnippetLines {
		keep = keep[:maxSeedSnippetLines]
	}
	return strings.Join(keep, "\n")
}

// verifySeed re-reads the seed's file from a clone: the snippet's first
// line is looked for near the claimed range, and the quoted lines become
// the file's own lines inside the range that resemble it (e.g.
// public/index.html 836-853 quotes 840 and 848, both tile layers),
// numbered. When research gave no snippet, or one the file does not hold
// (a paraphrase), the anchor is picked from the file itself (R11 S1c,
// seedPicker). A seed that cannot be checked (file not in a clone, range
// past its end) keeps only its path and lines.
func verifySeed(s lineSeed, clones []string) lineSeed {
	return verifySeedWith(s, clones, nil)
}

func verifySeedWith(s lineSeed, clones []string, pick *seedPicker) lineSeed {
	var fileLines []string
	for _, c := range clones {
		if c == "" {
			continue
		}
		b, err := os.ReadFile(filepath.Join(c, filepath.FromSlash(s.Path)))
		if err == nil {
			fileLines = strings.Split(strings.ReplaceAll(string(b), "\r\n", "\n"), "\n")
			break
		}
	}
	if fileLines == nil || s.LineStart > len(fileLines) {
		// Not checkable (no clone, or the range is past the end): keep the
		// place only. An unverified snippet would be a guessed old_string.
		s.Snippet = ""
		return s
	}
	anchor := ""
	for _, l := range strings.Split(s.Snippet, "\n") {
		if t := strings.TrimSpace(l); len(t) >= 8 {
			anchor = t
			break
		}
	}
	end := min(s.LineEnd, len(fileLines))
	var picked []int
	at := -1
	if anchor != "" {
		for d := 0; at < 0 && d <= seedAnchorSlack+(end-s.LineStart); d++ {
			for _, i := range []int{s.LineStart - 1 + d, s.LineStart - 1 - d} {
				if i >= 0 && i < len(fileLines) && strings.TrimSpace(fileLines[i]) == anchor {
					at = i
					break
				}
			}
		}
	}
	if at < 0 {
		// No snippet, or research paraphrased it: the line to quote is
		// chosen from the file (never research's text, S1c).
		anchor = ""
		if i := pick.anchor(s.Path, fileLines, s.LineStart-1, end-1); i >= 0 {
			at, anchor = i, strings.TrimSpace(fileLines[i])
		}
	}
	lo, hi := s.LineStart-1, end-1
	if at >= 0 {
		lo, hi = min(lo, at), max(hi, at)
	}
	// R12 X3: a short range is quoted whole (a live run, attempt 12,
	// quoted only public/index.html:837, `L.control.zoom`, of 836-853, and
	// the model never changed the tile URLs at 840 and 848).
	if hi-lo+1 <= seedWholeRangeLines {
		picked = wholeRangeLines(s.Path, fileLines, lo, hi)
	}
	if len(picked) == 0 {
		// A long range: up to 3 lines, first those whose URL or string
		// literal shares a hint word, then the anchor and lines like it.
		picked = pick.literalLines(fileLines, lo, hi, seedLongRangeLines)
		has := map[int]bool{}
		for _, i := range picked {
			has[i] = true
		}
		add := func(i int) {
			if len(picked) < seedLongRangeLines && !has[i] {
				has[i] = true
				picked = append(picked, i)
			}
		}
		if at >= 0 {
			add(at)
			for i := lo; i <= hi; i++ {
				if seedSimilar(anchor, strings.TrimSpace(fileLines[i])) {
					add(i)
				}
			}
		} else if len(picked) == 0 {
			for i := s.LineStart - 1; i < end; i++ {
				if strings.TrimSpace(fileLines[i]) != "" {
					add(i)
				}
			}
		}
		sort.Ints(picked)
	}
	if len(picked) == 0 {
		return s
	}
	var b strings.Builder
	for k, i := range picked {
		if k > 0 {
			b.WriteString("\n")
		}
		b.WriteString(strconv.Itoa(i+1) + ": " + fileLines[i])
	}
	s.Snippet = b.String()
	s.Verified = true
	if picked[0]+1 < s.LineStart {
		s.LineStart = picked[0] + 1
	}
	if last := picked[len(picked)-1] + 1; last > s.LineEnd {
		s.LineEnd = last
	}
	return s
}

// seedSimilar: the character-bigram Dice coefficient is at least 0.6.
func seedSimilar(a, b string) bool {
	if len(a) < 2 || len(b) < 2 {
		return false
	}
	grams := map[string]int{}
	for i := 0; i+1 < len(a); i++ {
		grams[a[i:i+2]]++
	}
	common := 0
	for i := 0; i+1 < len(b); i++ {
		if g := b[i : i+2]; grams[g] > 0 {
			grams[g]--
			common++
		}
	}
	return 2*float64(common)/float64(len(a)-1+len(b)-1) >= 0.6
}

// seedPicker chooses the line a seed quotes when research gave none (R11
// S1c): a criterion's symbol line inside the range ("L.tileLayer" at
// public/index.html:840), else the code line sharing the most words with
// the intake text. An intake that quotes the tile URLs makes 840 and 848
// outscore the `const map = L.map(…)` line that opens 836-853.
type seedPicker struct {
	symbols map[string][]int
	hints   map[string]bool
	idents  map[string]bool
}

func newSeedPicker(results []researchResultMessage, hintText string) *seedPicker {
	p := &seedPicker{symbols: map[string][]int{}, hints: map[string]bool{}, idents: map[string]bool{}}
	for _, r := range results {
		if r.Status != ResearchStatusOK || r.Criteria == nil {
			continue
		}
		for _, c := range r.Criteria.Criteria {
			for _, s := range c.Symbols {
				if f := normalizeSeedPath(strings.TrimSpace(s.File)); f != "" && s.Line > 0 {
					p.symbols[f] = append(p.symbols[f], s.Line)
				}
			}
		}
	}
	for _, w := range seedWords(hintText) {
		p.hints[w] = true
	}
	for _, id := range seedIdentifiers(hintText) {
		p.idents[id] = true
	}
	return p
}

// seedIdentifiers are s's camelCase or snake_case identifiers (a word with
// an inner capital or underscore), lower-cased.
func seedIdentifiers(s string) []string {
	var out []string
	for _, f := range strings.FieldsFunc(s, func(r rune) bool { return !(unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_') }) {
		if len(f) < 6 {
			continue
		}
		inner := strings.Contains(strings.Trim(f, "_"), "_")
		for i, r := range f {
			if i > 0 && unicode.IsUpper(r) {
				inner = true
				break
			}
		}
		if inner {
			out = append(out, strings.ToLower(f))
		}
	}
	return out
}

// anchor is the 0-based index in lo..hi of the line to quote, or -1.
func (p *seedPicker) anchor(path string, fileLines []string, lo, hi int) int {
	if p == nil || lo < 0 || hi >= len(fileLines) || lo > hi {
		return -1
	}
	lines := append([]int(nil), p.symbols[path]...)
	sort.Ints(lines)
	for _, n := range lines {
		if i := n - 1; i >= lo && i <= hi && seedCodeLine(fileLines[i]) {
			return i
		}
	}
	best, bestScore := -1, 0
	for i := lo; i <= hi; i++ {
		if !seedCodeLine(fileLines[i]) {
			continue
		}
		// Scores are doubled so the literal bonus below breaks ties only.
		score := 0
		seen := map[string]bool{}
		for _, w := range seedWords(fileLines[i]) {
			if !seen[w] && p.hintMatches(w) {
				seen[w] = true
				score += 2
			}
		}
		// An identifier the intake names whole ("createApp") says more
		// than its parts.
		for _, id := range seedIdentifiers(fileLines[i]) {
			if p.idents[id] && !seen["\x00"+id] {
				seen["\x00"+id] = true
				score += 6
			}
		}
		// A URL or a quoted string is usually what such a change edits
		// (the tile URL, not the provider attribution next to it).
		if score > 0 {
			if strings.Contains(fileLines[i], "://") {
				score++
			}
			if seedQuoted.MatchString(fileLines[i]) {
				score++
			}
		}
		if score > bestScore {
			best, bestScore = i, score
		}
	}
	return best
}

// wholeRangeLines are the 0-based indexes lo..hi without the leading and
// trailing non-code lines (blank, comment, a lone closer) and without the
// blank and comment lines between: the code of the range, which the task
// message quotes within its 5,000 chars.
// Comments are told by the file's language (R12 review: a CSS `#map {`
// selector, a C `#include` or a Markdown heading is not a comment).
func wholeRangeLines(path string, fileLines []string, lo, hi int) []int {
	comment := seedCommentMask(path, fileLines, hi)
	nonCode := func(i int) bool {
		t := strings.TrimSpace(fileLines[i])
		return len(t) < 8 || comment[i] // blank, a lone closer, a comment
	}
	for lo <= hi && nonCode(lo) {
		lo++
	}
	for hi >= lo && nonCode(hi) {
		hi--
	}
	var out []int
	for i := lo; i <= hi; i++ {
		if strings.TrimSpace(fileLines[i]) != "" && !comment[i] {
			out = append(out, i)
		}
	}
	return out
}

// seedCommentStyle is a file's comment syntax, by extension.
type seedCommentStyle struct {
	hash, cBlock, html, dashes bool
}

func seedCommentStyleOf(path string) seedCommentStyle {
	base := strings.ToLower(filepath.Base(path))
	switch ext := strings.ToLower(filepath.Ext(base)); {
	case strings.HasPrefix(base, "dockerfile") || base == ".env" || strings.HasPrefix(base, ".env.") || ext == ".env":
		return seedCommentStyle{hash: true}
	case ext == ".sh" || ext == ".bash" || ext == ".zsh" || ext == ".py" || ext == ".yaml" || ext == ".yml" || ext == ".toml":
		return seedCommentStyle{hash: true}
	case ext == ".md" || ext == ".markdown" || ext == ".xml" || ext == ".svg":
		return seedCommentStyle{html: true}
	case ext == ".html" || ext == ".htm" || ext == ".vue" || ext == ".svelte":
		// Markup with inline scripts and styles.
		return seedCommentStyle{cBlock: true, html: true}
	case ext == ".sql" || ext == ".lua":
		return seedCommentStyle{dashes: true}
	}
	// C-like, JS/TS, CSS, Go, Rust, Java … and anything else.
	return seedCommentStyle{cBlock: true}
}

// seedCommentMask marks the comment lines of fileLines[0..hi]: block
// comments are followed from the top of the file, so a `*`-prefixed line
// is a comment only inside one.
func seedCommentMask(path string, fileLines []string, hi int) []bool {
	st := seedCommentStyleOf(path)
	mask := make([]bool, len(fileLines))
	inBlock, inHTML := false, false
	for i := 0; i <= hi && i < len(fileLines); i++ {
		t := strings.TrimSpace(fileLines[i])
		switch {
		case st.cBlock && inBlock:
			mask[i] = true
			inBlock = !strings.Contains(t, "*/")
		case st.html && inHTML:
			mask[i] = true
			inHTML = !strings.Contains(t, "-->")
		case st.cBlock && strings.HasPrefix(t, "//"):
			mask[i] = true
		case st.cBlock && strings.HasPrefix(t, "/*"):
			mask[i] = true
			inBlock = !strings.Contains(t[2:], "*/")
		case st.html && strings.HasPrefix(t, "<!--"):
			mask[i] = true
			inHTML = !strings.Contains(t[4:], "-->")
		case st.hash && strings.HasPrefix(t, "#"):
			mask[i] = true
		case st.dashes && strings.HasPrefix(t, "--"):
			mask[i] = true
		}
	}
	return mask
}

// literalLines are up to n code lines in lo..hi holding a URL or a quoted
// string literal that shares a hint word ("mapforgecdn" in the tile URL
// holds the intake's "mapforge"), URL lines first, in line order.
func (p *seedPicker) literalLines(fileLines []string, lo, hi, n int) []int {
	if p == nil || lo < 0 || hi >= len(fileLines) {
		return nil
	}
	var urls, others []int
	for i := lo; i <= hi; i++ {
		if !seedCodeLine(fileLines[i]) {
			continue
		}
		hinted := func(lits []string) bool {
			for _, lit := range lits {
				for _, w := range seedWords(lit) {
					if p.hintMatches(w) {
						return true
					}
				}
			}
			return false
		}
		switch {
		case hinted(seedURL.FindAllString(fileLines[i], -1)):
			urls = append(urls, i)
		case hinted(seedQuoted.FindAllString(fileLines[i], -1)):
			others = append(others, i)
		}
	}
	out := append(urls, others...)
	if len(out) > n {
		out = out[:n]
	}
	sort.Ints(out)
	return out
}

// seedQuoted matches a quoted string literal of at least 3 characters.
var seedQuoted = regexp.MustCompile("'[^']{3,}'|\"[^\"]{3,}\"|`[^`]{3,}`")

// hintMatches: the line word w is a hint word, or contains one
// ("mapforgecdn" holds the intake's "mapforge", R11 review).
func (p *seedPicker) hintMatches(w string) bool {
	if p.hints[w] {
		return true
	}
	for h := range p.hints {
		if len(h) >= 4 && strings.Contains(w, h) {
			return true
		}
	}
	return false
}

// seedCodeLine: not blank, not a comment, not a lone closer.
func seedCodeLine(l string) bool {
	t := strings.TrimSpace(l)
	if len(t) < 8 {
		return false
	}
	for _, c := range []string{"//", "#", "/*", "*", "<!--", "--"} {
		if strings.HasPrefix(t, c) {
			return false
		}
	}
	return true
}

// seedStopWords are words too common in code and prose to say where.
var seedStopWords = map[string]bool{
	"const": true, "return": true, "function": true, "this": true, "true": true, "false": true,
	"null": true, "import": true, "from": true, "export": true, "async": true, "await": true,
	"http": true, "https": true, "with": true, "that": true, "should": true, "when": true,
	"then": true, "else": true, "string": true, "type": true, "value": true, "file": true,
	"line": true, "lines": true, "the": true, "and": true, "for": true, "new": true, "self": true,
}

// seedWords are s's words: identifiers split at camelCase and at
// punctuation, lower-cased, a plural "s" dropped, at least 4 letters, no
// stop words.
func seedWords(s string) []string {
	var out []string
	var cur []rune
	flush := func() {
		if len(cur) == 0 {
			return
		}
		w := strings.ToLower(string(cur))
		cur = cur[:0]
		if len(w) > 4 && strings.HasSuffix(w, "s") && !strings.HasSuffix(w, "ss") {
			w = w[:len(w)-1]
		}
		if len(w) >= 4 && !seedStopWords[w] {
			out = append(out, w)
		}
	}
	rs := []rune(s)
	for i, r := range rs {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			if unicode.IsUpper(r) && i > 0 && unicode.IsLower(rs[i-1]) {
				flush()
			}
			cur = append(cur, r)
		default:
			flush()
		}
	}
	flush()
	return out
}

// roundClones are the session's clones, or the persisted round's after a
// coordinator restart (the session is in memory only).
func (c *coordinator) roundClones(round *researchRound) []string {
	if out := c.clones(round.WorkID); len(out) > 0 {
		return out
	}
	out := make([]string, 0, len(round.RepoList))
	for _, r := range round.RepoList {
		out = append(out, r.ClonePath)
	}
	return out
}

// seedContext is the intake's seed set and text for the round's work item:
// the session's (set at dispatch), or after a restart the seeds the PRD
// names, validated against the round's clones, with the PRD as the hints.
func (c *coordinator) seedContext(round *researchRound) seedContext {
	var seeds seedSet
	text := round.PRDContent
	found := false
	if c != nil {
		c.mu.Lock()
		s := c.sessions[round.WorkID.String()]
		c.mu.Unlock()
		if s != nil {
			s.mu.Lock()
			if s.dispatched {
				seeds, text, found = s.seeds, s.intakeText+"\n\n"+s.prdContent, true
			}
			s.mu.Unlock()
		}
	}
	if !found {
		seeds = validateSeeds(extractSeedCandidates(round.PRDContent), c.roundClones(round), nil)
	}
	sc := seedContext{hintText: text}
	if !seeds.Unverified {
		sc.intakeFiles, sc.intakeLines = seeds.Files, seeds.Lines
	}
	return sc
}

// clones are the repository clone paths of the work item's research
// session ("" entries skipped by the reader); nil without a session.
func (c *coordinator) clones(workID interface{ String() string }) []string {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	s := c.sessions[workID.String()]
	c.mu.Unlock()
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.repoList))
	for _, r := range s.repoList {
		out = append(out, r.ClonePath)
	}
	return out
}
