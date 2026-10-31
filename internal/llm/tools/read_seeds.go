package tools

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// R10 W5: when the task names a place to edit in a file ("## Where to
// edit"), a read of that file without a range opens around it instead of
// at lines 1-400. A live run (attempt 9) read public/index.html's first 400
// lines and never saw the code it had to change at lines 840/848.

// seedReadRadius is how many lines before the seed the default window
// starts (the window stays readDefaultLines long).
const seedReadRadius = 200

// SetReadSeeds sets, per repository path, the line a default read centres
// on.
func (e *Executor) SetReadSeeds(lines map[string]int) {
	e.observedMu.Lock()
	defer e.observedMu.Unlock()
	e.readSeeds = map[string]int{}
	for p, n := range lines {
		if k := pinPath(p); k != "" && n > 0 {
			e.readSeeds[k] = n
		}
	}
}

func (e *Executor) readSeed(path string) int {
	e.observedMu.Lock()
	defer e.observedMu.Unlock()
	return e.readSeeds[pinPath(path)]
}

// defaultReadResultMaxChars is READ_RESULT_MAX_CHARS's default (R12 X1):
// a live run's (attempt 12) 400-line windows of public/index.html and
// landing/index.html were about 15k chars each (long lines), and the model
// read them again and again.
// The default is 4,800 so a default window fits the 5,000-char
// pinned-read budget (R12 review).
const defaultReadResultMaxChars = 4800

// readResultMaxChars bounds a read without a range by chars as well as by
// lines: READ_RESULT_MAX_CHARS, default 4,800.
func readResultMaxChars() int {
	if v, err := strconv.Atoi(strings.TrimSpace(os.Getenv("READ_RESULT_MAX_CHARS"))); err == nil && v > 0 {
		return v
	}
	return defaultReadResultMaxChars
}

// fileLinesOf splits content into its lines, each with its line break.
func fileLinesOf(content string) []string {
	lines := strings.SplitAfter(content, "\n")
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1]
	}
	return lines
}

// defaultWindow returns the args of a read without a range of a file whose
// lines (each with its line break) are lines, and the note to append: at
// most readDefaultLines lines and readResultMaxChars chars (R12 X1),
// around the file's seed when it has one (R10 W5) — the seed line first,
// then a line after and a line before in turn while they fit, so the seed
// stays centred. ok is false when the read has a range or the whole file
// fits. A window without a seed keeps read_file's own "[lines 1-b of n; …
// start_line=b+1 for more]" hint (note is "").
func (e *Executor) defaultWindow(path string, args map[string]interface{}, lines []string) (map[string]interface{}, string, bool) {
	if _, ok := args["start_line"]; ok {
		return args, "", false
	}
	if _, ok := args["end_line"]; ok {
		return args, "", false
	}
	total, chars := len(lines), 0
	for _, l := range lines {
		chars += len(l)
	}
	maxChars := readResultMaxChars()
	if total == 0 || (total <= readDefaultLines && chars <= maxChars) {
		return args, "", false
	}
	seed := e.readSeed(path)
	if seed > total {
		seed = 0
	}
	var start, end int
	if seed <= 0 {
		start, end = 1, 1
		size := len(lines[0])
		for end < min(total, readDefaultLines) && size+len(lines[end]) <= maxChars {
			size += len(lines[end])
			end++
		}
	} else {
		// The 400-line window around the seed, as before X1 …
		lo := max(1, seed-seedReadRadius)
		hi := min(total, lo+readDefaultLines-1)
		if hi-lo+1 < readDefaultLines {
			lo = max(1, hi-readDefaultLines+1)
		}
		// … narrowed to the char cap around the seed.
		start, end = seed, seed
		size := len(lines[seed-1])
		for grew := true; grew; {
			grew = false
			if end+1 <= hi && size+len(lines[end]) <= maxChars {
				size += len(lines[end])
				end++
				grew = true
			}
			if start-1 >= lo && size+len(lines[start-2]) <= maxChars {
				size += len(lines[start-2])
				start--
				grew = true
			}
		}
	}
	out := make(map[string]interface{}, len(args)+2)
	for k, v := range args {
		out[k] = v
	}
	out["start_line"], out["end_line"] = float64(start), float64(end)
	if seed <= 0 {
		return out, "", true
	}
	// The note always starts "[lines a-b of N", the hint the ledger reads,
	// so a seeded read is pinned as this window, not as the whole file.
	note := fmt.Sprintf("[lines %d-%d of %d; opened around the Where-to-edit line %d; read_file with start_line=1 for the top", start, end, total, seed)
	if end < total {
		note += fmt.Sprintf(", start_line=%d for more", end+1)
	}
	note += "]"
	return out, note, true
}
