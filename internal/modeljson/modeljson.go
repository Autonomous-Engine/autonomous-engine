// Package modeljson finds the JSON value in a model's free-text answer.
// Models wrap the value in prose, code fences or <think> blocks, and
// sometimes emit a stray closing brace or a second value after it; slicing
// from the first opening bracket to the last closing one turns all of that
// into a decode error. The value is the first balanced top-level span
// (bracket matching that respects strings and escapes) that is valid JSON;
// anything after it is ignored.
package modeljson

import (
	"encoding/json"
	"errors"
	"io"
	"strings"
)

// StripThinking removes <think>...</think> blocks; an unterminated block
// removes the rest of the text.
func StripThinking(s string) string {
	for {
		i := strings.Index(s, "<think>")
		if i < 0 {
			return s
		}
		j := strings.Index(s[i:], "</think>")
		if j < 0 {
			return s[:i]
		}
		s = s[:i] + s[i+j+len("</think>"):]
	}
}

// FirstObject returns the first balanced top-level {...} in s that is valid
// JSON. When no candidate is valid it returns the first candidate (or, if
// that never closes, the text from its opening brace to the end) so the
// caller's decoder reports the actual syntax error; "" when s has no '{'.
func FirstObject(s string) string {
	vals, first := scan(s, '{')
	if len(vals) > 0 {
		return vals[0]
	}
	return first
}

// Arrays returns every balanced top-level [...] in s that is valid JSON, in
// order, so a caller can take the first that decodes to what it expects.
func Arrays(s string) []string {
	vals, _ := scan(s, '[')
	return vals
}

// scan walks s for spans opening with open. A valid span is collected and
// scanning resumes after it; an invalid balanced span is skipped whole (its
// inner values are not top-level); after an unterminated one the scan goes
// on from the next character.
// first is the first span seen, valid or not.
func scan(s string, open byte) (valid []string, first string) {
	for i := 0; i < len(s); {
		k := strings.IndexByte(s[i:], open)
		if k < 0 {
			break
		}
		start := i + k
		end := balancedEnd(s, start)
		if end < 0 {
			if first == "" {
				first = s[start:]
			}
			// A valid JSON prefix that was cut off (a truncated answer):
			// stop, so the caller reports the failure rather than taking
			// an inner value for the whole. Only a stray opener that is
			// not JSON (prose "stray {") is skipped.
			var v any
			if err := json.NewDecoder(strings.NewReader(s[start:])).Decode(&v); errors.Is(err, io.ErrUnexpectedEOF) {
				break
			}
			i = start + 1
			continue
		}
		span := s[start : end+1]
		if first == "" {
			first = span
		}
		if json.Valid([]byte(span)) {
			valid = append(valid, span)
		}
		i = end + 1
	}
	return valid, first
}

// balancedEnd returns the index of the bracket closing the one at start,
// counting { and [ as openers and } and ] as closers outside JSON strings,
// or -1 when it never closes.
func balancedEnd(s string, start int) int {
	depth := 0
	inString := false
	for i := start; i < len(s); i++ {
		c := s[i]
		if inString {
			switch c {
			case '\\':
				i++
			case '"':
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
		case '{', '[':
			depth++
		case '}', ']':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}
