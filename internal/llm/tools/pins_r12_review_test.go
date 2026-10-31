package tools

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/llm/adapter"
)

// widthFile is n lines of about w chars each ("line N xxxx…").
func widthFile(n, w int) string {
	var b strings.Builder
	for i := 1; i <= n; i++ {
		l := fmt.Sprintf("line %d ", i)
		b.WriteString(l + strings.Repeat("x", max(0, w-len(l))) + "\n")
	}
	return b.String()
}

var trimmedRE = regexp.MustCompile(`\[pin trimmed to lines (\d+)-(\d+)\]`)

// R12 review 1: the newest pin bigger than the budget is trimmed around
// the latest edit (else the Where-to-edit line) instead of dropped; an
// older pin that does not fit still falls back to its ledger line.
func TestNewestPinIsTrimmedNotDropped(t *testing.T) {
	big := widthFile(300, 40) // 12,000 chars
	l := newEvidenceLedger()
	l.enablePins([]string{"a.html", "b.html"}, nil)
	l.seedOf = func(p string) int {
		if p == "a.html" {
			return 150
		}
		return 0
	}
	l.recordRead(adapter.ToolCall{Name: "read_file", Arguments: `{"path":"b.html"}`}, big)
	l.recordRead(adapter.ToolCall{Name: "read_file", Arguments: `{"path":"a.html"}`}, big)
	sec := l.pinnedSection(nil)
	m := trimmedRE.FindStringSubmatch(sec)
	if m == nil {
		t.Fatalf("newest pin not trimmed:\n%.300s", sec)
	}
	a, _ := strconv.Atoi(m[1])
	b, _ := strconv.Atoi(m[2])
	if a > 150 || b < 150 || (150-a)-(b-150) > 1 || (150-a)-(b-150) < -1 {
		t.Fatalf("trimmed to %d-%d, not centred on the seed 150", a, b)
	}
	if !strings.Contains(sec, fmt.Sprintf("### a.html (lines %d-%d)\n", a, b)) || strings.Contains(sec, "### b.html") {
		t.Fatalf("section:\n%.400s", sec)
	}
	if !strings.Contains(sec, "1 older read(s) are only in the ledger") {
		t.Errorf("the older pin is not reported as omitted")
	}
	if block := sec[strings.Index(sec, "### a.html"):]; len(block) > pinnedMaxChars {
		t.Errorf("trimmed pin %d chars > %d", len(block), pinnedMaxChars)
	}
	if len(l.rendered) != 1 || l.rendered[0].start != a || l.rendered[0].end != b {
		t.Fatalf("rendered = %+v", l.rendered)
	}
	// The latest edit wins over the seed.
	l.lastEdit["a.html"] = 250
	m = trimmedRE.FindStringSubmatch(l.pinnedSection(nil))
	if a, _ := strconv.Atoi(m[1]); a > 250 {
		t.Fatalf("not centred on the edit at 250: %s", m[0])
	}
	if b, _ := strconv.Atoi(m[2]); b < 250 {
		t.Fatalf("not centred on the edit at 250: %s", m[0])
	}
}

// A default read now fits the pin budget whole (READ_RESULT_MAX_CHARS
// 4,800 < 5,000).
func TestDefaultReadFitsThePinBudget(t *testing.T) {
	root := writeTree(t, map[string]string{"public/index.html": longLineHTML(1000)})
	ex := NewExecutor(root)
	r := ex.Execute(context.Background(), adapter.ToolCall{ID: "r", Name: "read_file", Arguments: `{"path":"public/index.html"}`})
	l := newEvidenceLedger()
	l.enablePins([]string{"public/index.html"}, ex.currentLines)
	l.recordRead(adapter.ToolCall{Name: "read_file", Arguments: `{"path":"public/index.html"}`}, r.Content)
	sec := l.pinnedSection(nil)
	if strings.Contains(sec, "pin trimmed") || !strings.Contains(sec, "### public/index.html (lines 1-") {
		t.Fatalf("default read not pinned whole:\n%.300s", sec)
	}
}

// R12 review 2: a short "(pinned)" answer makes its pin the newest, so a
// newer read cannot evict it from the next request.
func TestPinRelieUponIsNotEvicted(t *testing.T) {
	body := widthFile(50, 40) // 2,000 chars each
	root := writeTree(t, map[string]string{"a.ts": body, "b.ts": body, "c.ts": body})
	ex := NewExecutor(root)
	l := newEvidenceLedger()
	l.enablePins([]string{"a.ts", "b.ts", "c.ts"}, ex.currentLines)
	read := func(p string) {
		l.recordRead(adapter.ToolCall{Name: "read_file", Arguments: `{"path":"` + p + `"}`}, body)
	}
	read("a.ts")
	read("b.ts")
	if sec := l.pinnedSection(nil); !strings.Contains(sec, "### a.ts") || !strings.Contains(sec, "### b.ts") {
		t.Fatalf("both pins should fit:\n%.200s", sec)
	}
	m := &readMemo{}
	short := m.answer(context.Background(), ex, []adapter.ToolCall{{ID: "x", Name: "read_file", Arguments: `{"path":"a.ts"}`}}, 5, 2, l)
	if r, ok := short[0]; !ok || !strings.Contains(r.Content, "(pinned)") {
		t.Fatalf("no pinned answer: %+v", short)
	}
	read("c.ts")
	sec := l.pinnedSection(nil)
	if !strings.Contains(sec, "### a.ts") || !strings.Contains(sec, "### c.ts") || strings.Contains(sec, "### b.ts") {
		t.Fatalf("the pin the answer relied on was evicted:\n%s", sec[:min(len(sec), 300)])
	}
}
