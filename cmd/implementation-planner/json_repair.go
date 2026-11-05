package main

import (
	"encoding/json"
	"fmt"
	"strings"
)

// repairJSONStrings escapes double quotes that sit inside a JSON string
// value — code quoted in a description, live:
// `WithCharsetAliases({"gb2312": GB18030})` — and returns the text and how
// many it escaped. A quote closes a key when the next significant byte is
// ':' and a value when it is ',', '}' or ']' (or the end); any other quote
// inside a string is an inner one. Only tried once a direct parse failed,
// and the result is parsed again before it is trusted.
func repairJSONStrings(s string) (string, int) {
	var out strings.Builder
	out.Grow(len(s) + 16)
	n := 0
	inStr, isKey := false, false
	var stack []byte
	lastSig := byte(0)
	nextSig := func(i int) byte {
		for j := i; j < len(s); j++ {
			switch s[j] {
			case ' ', '\n', '\t', '\r':
				continue
			}
			return s[j]
		}
		return 0
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inStr {
			switch {
			case c == '\\' && i+1 < len(s):
				out.WriteByte(c)
				out.WriteByte(s[i+1])
				i++
			case c == '"':
				next := nextSig(i + 1)
				closes := next == ':'
				if !isKey {
					closes = next == ',' || next == '}' || next == ']' || next == 0
				}
				if closes {
					inStr = false
					lastSig = c
					out.WriteByte(c)
				} else {
					out.WriteString(`\"`)
					n++
				}
			default:
				out.WriteByte(c)
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
			isKey = len(stack) > 0 && stack[len(stack)-1] == '{' && (lastSig == '{' || lastSig == ',')
		case '{', '[':
			stack = append(stack, c)
		case '}', ']':
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
		}
		switch c {
		case ' ', '\n', '\t', '\r':
		default:
			lastSig = c
		}
		out.WriteByte(c)
	}
	return out.String(), n
}

// jsonErrorHint says where a task array stopped parsing, for the
// re-prompt: the line, the column and the text around it.
func jsonErrorHint(content string) string {
	start, end := indexOf(content, '['), lastIndexOf(content, ']')
	if start < 0 || end <= start {
		return "no JSON array found"
	}
	snippet := content[start : end+1]
	var v any
	err := json.Unmarshal([]byte(snippet), &v)
	if err == nil {
		return ""
	}
	var se *json.SyntaxError
	off := -1
	if ok := asSyntaxError(err, &se); ok {
		off = int(se.Offset)
	}
	if off < 0 || off > len(snippet) {
		return err.Error()
	}
	line := 1 + strings.Count(snippet[:off], "\n")
	lo, hi := off-60, off+30
	if lo < 0 {
		lo = 0
	}
	if hi > len(snippet) {
		hi = len(snippet)
	}
	return fmt.Sprintf("%v at line %d, near: %s", err, line, strings.TrimSpace(snippet[lo:hi]))
}

func asSyntaxError(err error, target **json.SyntaxError) bool {
	se, ok := err.(*json.SyntaxError)
	if ok {
		*target = se
	}
	return ok
}
