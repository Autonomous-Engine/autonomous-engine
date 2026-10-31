package tools

import (
	"fmt"
	"strings"
)

// retypedBlockMin is the shortest run of consecutive lines a re-typed block
// must have (R10 W1), and retypedBlockMeaningful how many of them must be
// meaningful lines (not blank, not pure punctuation such as `});`).
const (
	retypedBlockMin        = 3
	retypedBlockMeaningful = 2
	// retypedBlockGap is how far from old_string the re-typed original may
	// start (blank lines between the two are common).
	retypedBlockGap = 3
)

// detectRetypedBlock reports the failure mode the duplicate guard exists
// for (R10 W1): new_string re-types, next to the text it keeps, a block of
// at least three consecutive lines that already follows (or precedes)
// old_string in the file — so after the edit the block would appear twice.
// In a live run (attempt 9) call 11 replaced a test suite's opening
// `describe(…, () => {` with itself plus file lines 22-29 typed again. content is the file,
// idx the byte offset of old_string in it. The message names the repeated
// lines and the line numbers of both copies; "" when the edit is clean.
func detectRetypedBlock(content string, idx int, oldString, newString string) string {
	fileLines := strings.Split(content, "\n")
	oldStart := strings.Count(content[:idx], "\n")      // 0-based line of old_string's first line
	oldEnd := oldStart + strings.Count(oldString, "\n") // 0-based line of its last line
	delta := strings.Count(newString, "\n") - strings.Count(oldString, "\n")

	var before, after string
	keptOld := strings.Index(newString, oldString)
	if keptOld >= 0 {
		before, after = newString[:keptOld], newString[keptOld+len(oldString):]
	} else {
		before, after = newString, newString
	}
	newLineOf := func(off int) int { return strings.Count(newString[:off], "\n") } // 0-based line within new_string

	// After old_string: the added lines vs the file lines right after it.
	if after != "" && strings.Contains(after, "\n") {
		afterOff := len(newString) - len(after)
		lines := strings.Split(after, "\n")
		first := 0
		if keptOld >= 0 {
			first = 1 // the rest of old_string's last line
		}
		for s := first; s < len(lines); s++ {
			for f := oldEnd + 1; f <= oldEnd+1+retypedBlockGap && f < len(fileLines); f++ {
				n := matchRun(lines[s:], fileLines[f:])
				if n == 0 {
					continue
				}
				nl := newLineOf(afterOff) + s // 0-based line in new_string
				newCopy := oldStart + nl      // 0-based result line of the new copy
				oldCopy := f + delta          // the original block, shifted by the edit
				return retypedMessage(nl, f, n, newCopy, oldCopy, "follow", fileLines)
			}
		}
	}
	// Before old_string: the added lines vs the file lines right before it.
	if before != "" && strings.Contains(before, "\n") {
		lines := strings.Split(before, "\n")
		last := len(lines) - 1
		if keptOld >= 0 {
			last-- // the start of old_string's first line
		}
		for e := last; e >= 0; e-- {
			for f := oldStart - 1; f >= oldStart-1-retypedBlockGap && f >= 0; f-- {
				n := matchRunBack(lines[:e+1], fileLines[:f+1])
				if n == 0 {
					continue
				}
				nl := e - n + 1
				fs := f - n + 1
				newCopy := oldStart + nl
				return retypedMessage(nl, fs, n, newCopy, fs, "precede", fileLines)
			}
		}
	}
	return ""
}

// matchRun is the length of the qualifying run of equal (trimmed) lines at
// the start of a and b, trailing blank lines dropped; 0 when the run is too
// short or too thin to be a re-typed block.
func matchRun(a, b []string) int {
	n := 0
	for n < len(a) && n < len(b) && strings.TrimSpace(a[n]) == strings.TrimSpace(b[n]) {
		n++
	}
	return qualify(a[:n])
}

// matchRunBack is matchRun aligned at the ends of a and b.
func matchRunBack(a, b []string) int {
	n := 0
	for n < len(a) && n < len(b) && strings.TrimSpace(a[len(a)-1-n]) == strings.TrimSpace(b[len(b)-1-n]) {
		n++
	}
	run := a[len(a)-n:]
	// Leading blank lines do not count either.
	for len(run) > 0 && strings.TrimSpace(run[0]) == "" {
		run = run[1:]
	}
	if qualify(run) == 0 {
		return 0
	}
	return len(run)
}

func qualify(run []string) int {
	for len(run) > 0 && strings.TrimSpace(run[len(run)-1]) == "" {
		run = run[:len(run)-1]
	}
	if len(run) < retypedBlockMin {
		return 0
	}
	meaningful := 0
	for _, l := range run {
		t := strings.TrimSpace(l)
		if len(t) >= 8 && !isPurePunctuation(t) {
			meaningful++
		}
	}
	if meaningful < retypedBlockMeaningful {
		return 0
	}
	return len(run)
}

// retypedMessage renders the rejection: nl is the block's 0-based line in
// new_string, fl its 0-based line in the file, n its length, newCopy and
// oldCopy the 0-based result lines of the two copies.
func retypedMessage(nl, fl, n, newCopy, oldCopy int, where string, fileLines []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "new_string lines %d-%d re-type file lines %d-%d, which already %s old_string:\n", nl+1, nl+n, fl+1, fl+n, where)
	shown := 0
	for i := fl; i < fl+n && shown < 10; i++ {
		fmt.Fprintf(&b, "  %4d: %s\n", i+1, fileLines[i])
		shown++
	}
	if n > shown {
		fmt.Fprintf(&b, "  … (%d more)\n", n-shown)
	}
	a, c := newCopy, oldCopy
	if a > c {
		a, c = c, a
	}
	fmt.Fprintf(&b, "After this edit they would appear twice (lines %d-%d and %d-%d). Drop them from new_string, or extend old_string to include them.", a+1, a+n, c+1, c+n)
	return b.String()
}
