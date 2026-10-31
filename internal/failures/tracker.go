package failures

import (
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"
)

// Entry is one fingerprint's history in a repair loop.
type Entry struct {
	Record Record `json:"record"`
	// Count is the number of check runs this fingerprint failed in.
	Count int `json:"count"`
	// FirstRun / LastRun are the 1-based check runs it was first and last
	// seen in.
	FirstRun int `json:"first_run"`
	LastRun  int `json:"last_run"`
	// Consecutive is the length of its current streak of consecutive
	// failing runs.
	Consecutive int `json:"consecutive"`
	// LastCallID is the synthetic call id of the run that last saw it
	// (the evidence pointer for plan revisions and the dashboard).
	LastCallID string `json:"last_call_id,omitempty"`
}

// Tracker counts fingerprints across the check runs of one repair loop.
// It is not safe for concurrent use.
type Tracker struct {
	entries map[string]*Entry
	order   []string
	prev    map[string]bool
	runs    int
}

// NewTracker returns an empty tracker.
func NewTracker() *Tracker { return &Tracker{entries: map[string]*Entry{}, prev: map[string]bool{}} }

// Observation is what one run added to the tracker.
type Observation struct {
	Run int
	// New are fingerprints first seen in this run.
	New []string
	// Seen are fingerprints seen in an earlier run.
	Seen []string
	// Repeated are fingerprints that failed in this run and in the run
	// immediately before it, with a repair attempt in between: the
	// repair did not fix them. A non-empty Repeated stops the loop.
	Repeated []string
}

// Observe records one failing run's records. afterRepair says whether a
// repair attempt ran between the previous run and this one.
func (t *Tracker) Observe(records []Record, callID string, afterRepair bool) Observation {
	t.runs++
	obs := Observation{Run: t.runs}
	cur := map[string]bool{}
	for _, r := range records {
		if cur[r.Fingerprint] {
			continue
		}
		cur[r.Fingerprint] = true
		e, ok := t.entries[r.Fingerprint]
		if !ok {
			e = &Entry{Record: r, FirstRun: t.runs}
			t.entries[r.Fingerprint] = e
			t.order = append(t.order, r.Fingerprint)
			obs.New = append(obs.New, r.Fingerprint)
		} else {
			obs.Seen = append(obs.Seen, r.Fingerprint)
			e.Record = r // newest location and span
		}
		e.Count++
		if t.prev[r.Fingerprint] {
			e.Consecutive++
			if afterRepair {
				obs.Repeated = append(obs.Repeated, r.Fingerprint)
			}
		} else {
			e.Consecutive = 1
		}
		e.LastRun = t.runs
		e.LastCallID = callID
	}
	for fp, e := range t.entries {
		if !cur[fp] {
			e.Consecutive = 0
		}
	}
	t.prev = cur
	return obs
}

// Entry returns the history of fp.
func (t *Tracker) Entry(fp string) (Entry, bool) {
	e, ok := t.entries[fp]
	if !ok {
		return Entry{}, false
	}
	return *e, true
}

// Entries returns every fingerprint's history, most frequent first (ties:
// first seen first).
func (t *Tracker) Entries() []Entry {
	out := make([]Entry, 0, len(t.order))
	for _, fp := range t.order {
		out = append(out, *t.entries[fp])
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Count > out[j].Count })
	return out
}

// Top returns at most n entries, most frequent first.
func (t *Tracker) Top(n int) []Entry {
	es := t.Entries()
	if len(es) > n {
		es = es[:n]
	}
	return es
}

// Runs is the number of failing runs observed.
func (t *Tracker) Runs() int { return t.runs }

// Short is the fingerprint's first 12 hex characters, for display.
func Short(fp string) string {
	if len(fp) > 12 {
		return fp[:12]
	}
	return fp
}

// Location renders file:line:col ("" when the record has no file).
func (r Record) Location() string {
	if r.File == "" {
		return ""
	}
	s := r.File
	if r.Line > 0 {
		s += fmt.Sprintf(":%d", r.Line)
		if r.Col > 0 {
			s += fmt.Sprintf(":%d", r.Col)
		}
	}
	return s
}

// FormatGrouped renders records for a repair prompt: grouped by file
// (records without a file last), each with its fingerprint, category,
// ownership, location, test, message and a short span. counts, when set,
// gives each fingerprint's number of failing runs so far.
func FormatGrouped(records []Record, counts map[string]int) string {
	groups := map[string][]Record{}
	var files []string
	for _, r := range records {
		k := r.File
		if _, ok := groups[k]; !ok {
			files = append(files, k)
		}
		groups[k] = append(groups[k], r)
	}
	sort.SliceStable(files, func(i, j int) bool {
		if (files[i] == "") != (files[j] == "") {
			return files[j] == ""
		}
		return files[i] < files[j]
	})
	var b strings.Builder
	for _, f := range files {
		name := f
		if name == "" {
			name = "(no file)"
		}
		fmt.Fprintf(&b, "### %s\n", name)
		for _, r := range groups[f] {
			fmt.Fprintf(&b, "- [%s] %s", Short(r.Fingerprint), r.Category)
			if loc := r.Location(); loc != "" {
				fmt.Fprintf(&b, " at %s", loc)
			}
			if r.Test != "" {
				fmt.Fprintf(&b, " in %s", r.Test)
			}
			fmt.Fprintf(&b, " (ownership %s", r.Ownership)
			if !r.Retryable {
				b.WriteString(", not fixable by editing the change")
			}
			if n := counts[r.Fingerprint]; n > 1 {
				fmt.Fprintf(&b, ", failed in %d runs", n)
			}
			b.WriteString(")\n")
			fmt.Fprintf(&b, "  message: %s\n", indentContinuation(r.Message))
			if r.Span != "" && r.Span != r.Message {
				fmt.Fprintf(&b, "  excerpt:\n%s\n", indentBlock(truncate(r.Span, 600), "    "))
			}
		}
	}
	return b.String()
}

func indentContinuation(s string) string { return strings.ReplaceAll(s, "\n", "\n    ") }

func indentBlock(s, prefix string) string {
	ls := lines(s)
	for i := range ls {
		ls[i] = prefix + ls[i]
	}
	return strings.Join(ls, "\n")
}

var (
	reMissingSymbol = regexp.MustCompile(`(?i)undefined: |undeclared name|has no (field or method|attribute|exported member|member)|cannot find name|is not defined|is not exported|not exported by|no exported member|cannot import name|unknown field|attributeerror|nameerror`)
	reMissingFile   = regexp.MustCompile(`(?i)no such file or directory|cannot find module '(\.{1,2}/[^']+)'|cannot find module "(\.{1,2}/[^"]+)"|failed to resolve import "(\.{1,2}/[^"]+)"|enoent|file not found|cannot open|does not exist`)
)

// Contradiction explains why r contradicts the task's plan, or is "" when
// it does not: a failure owned by a dependency or the environment (the
// plan assumed both work), or a missing symbol or file in a file the plan
// names.
func Contradiction(r Record, plannedFiles []string) string {
	switch r.Ownership {
	case OwnedDependency:
		return "a dependency the plan relies on is missing or broken"
	case OwnedEnvironment:
		return "the environment the plan assumes is not available"
	}
	planned := plannedMatch(r, plannedFiles)
	if planned == "" {
		return ""
	}
	switch {
	case reMissingFile.MatchString(r.Message):
		return "a file the plan names (" + planned + ") was not found"
	case reMissingSymbol.MatchString(r.Message):
		return "a symbol the plan expects in " + planned + " is missing"
	}
	return ""
}

// plannedMatch returns the planned file r refers to: its own file, or a
// planned file whose base name (without extension) the message mentions.
func plannedMatch(r Record, planned []string) string {
	rf := fingerprintFile(r.File)
	for _, p := range planned {
		pc := strings.TrimPrefix(path.Clean(strings.ReplaceAll(p, `\`, "/")), "./")
		if rf != "" && (rf == pc || strings.HasSuffix(pc, "/"+rf) || strings.HasSuffix(rf, "/"+pc)) {
			return pc
		}
	}
	for _, p := range planned {
		base := path.Base(strings.ReplaceAll(p, `\`, "/"))
		stem := strings.TrimSuffix(base, path.Ext(base))
		if len(stem) >= 3 && strings.Contains(r.Message, stem) {
			return p
		}
	}
	return ""
}
