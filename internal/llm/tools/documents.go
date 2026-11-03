package tools

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/autonomousengine/autonomous-engine/internal/llm/adapter"
)

// A registered document (the PRD, later a brief or a plan) is retrieved by
// the model a section at a time instead of travelling whole on every
// call: the task message carries what the task must answer and the table
// of contents; read_document fetches the rest when it is needed.

// documentResultMax bounds one read_document result.
const documentResultMax = 4000

// ReadDocument is the tool definition.
func ReadDocument() adapter.Tool {
	return adapter.Tool{
		Name: "read_document",
		Description: "Read a section of a registered document (e.g. the PRD) by heading, search its lines, or get its table of contents " +
			"(no section or query). Use it for the parts of the request your message does not carry.",
		Parameters: json.RawMessage(`{"type":"object","properties":{"name":{"type":"string","description":"Document name, e.g. prd"},"section":{"type":"string","description":"A heading (case-insensitive, substring)"},"query":{"type":"string","description":"Text to search for"}},"required":["name"]}`),
	}
}

// SetDocuments registers the documents read_document can serve.
func (e *Executor) SetDocuments(docs map[string]string) {
	e.observedMu.Lock()
	defer e.observedMu.Unlock()
	e.documents = map[string]string{}
	for k, v := range docs {
		e.documents[strings.ToLower(strings.TrimSpace(k))] = v
	}
}

func (e *Executor) document(name string) (string, []string) {
	e.observedMu.Lock()
	defer e.observedMu.Unlock()
	names := make([]string, 0, len(e.documents))
	for k := range e.documents {
		names = append(names, k)
	}
	sort.Strings(names)
	doc, ok := e.documents[strings.ToLower(strings.TrimSpace(name))]
	if !ok {
		return "", names
	}
	return doc, names
}

// readDocument executes read_document.
func (e *Executor) readDocument(callID string, args map[string]interface{}) adapter.ToolResult {
	refuse := func(msg string) adapter.ToolResult {
		return adapter.ToolResult{ToolCallID: callID, Content: "read_document: " + msg, IsError: true}
	}
	name := stringMapField(args, "name")
	doc, names := e.document(name)
	if doc == "" {
		return refuse(fmt.Sprintf("no document %q; available: %s", name, strings.Join(names, ", ")))
	}
	var out string
	switch {
	case strings.TrimSpace(stringMapField(args, "section")) != "":
		s, ok := documentSection(doc, stringMapField(args, "section"))
		if !ok {
			return refuse(fmt.Sprintf("no heading matches %q; headings:\n%s", stringMapField(args, "section"), documentTOC(doc)))
		}
		out = s
	case strings.TrimSpace(stringMapField(args, "query")) != "":
		out = documentSearch(doc, stringMapField(args, "query"))
	default:
		out = documentTOC(doc)
	}
	if len(out) > documentResultMax {
		out = out[:documentResultMax] + "\n[truncated: ask for a smaller section or search]"
	}
	return adapter.ToolResult{ToolCallID: callID, Content: out}
}

// docHeading reports a markdown heading line's level and title.
func docHeading(line string) (int, string) {
	t := strings.TrimRight(line, "\r")
	level := 0
	for level < len(t) && t[level] == '#' {
		level++
	}
	if level == 0 || level > 6 || level >= len(t) || t[level] != ' ' {
		return 0, ""
	}
	return level, strings.TrimSpace(t[level+1:])
}

// documentSection returns the section whose heading contains heading
// (case-insensitive), subsections included, up to the next heading of the
// same or a higher level.
func documentSection(doc, heading string) (string, bool) {
	want := strings.ToLower(strings.TrimSpace(heading))
	lines := strings.Split(doc, "\n")
	start, level := -1, 0
	for i, l := range lines {
		lv, title := docHeading(l)
		if lv > 0 && strings.Contains(strings.ToLower(title), want) {
			start, level = i, lv
			break
		}
	}
	if start < 0 {
		return "", false
	}
	end := len(lines)
	for i := start + 1; i < len(lines); i++ {
		if lv, _ := docHeading(lines[i]); lv > 0 && lv <= level {
			end = i
			break
		}
	}
	return strings.TrimSpace(strings.Join(lines[start:end], "\n")), true
}

// documentTOC lists the document's headings.
func documentTOC(doc string) string {
	var out []string
	for _, l := range strings.Split(doc, "\n") {
		if lv, title := docHeading(l); lv > 0 {
			out = append(out, strings.Repeat("#", lv)+" "+title)
		}
	}
	if len(out) == 0 {
		return "(no headings)"
	}
	return strings.Join(out, "\n")
}

// documentSearch returns the lines containing query (case-insensitive),
// each prefixed with the section it is in.
func documentSearch(doc, query string) string {
	q := strings.ToLower(strings.TrimSpace(query))
	section := ""
	var out []string
	for _, l := range strings.Split(doc, "\n") {
		if lv, title := docHeading(l); lv > 0 {
			section = title
			continue
		}
		if strings.Contains(strings.ToLower(l), q) {
			out = append(out, fmt.Sprintf("[%s] %s", section, strings.TrimSpace(l)))
		}
	}
	if len(out) == 0 {
		return fmt.Sprintf("no lines match %q", query)
	}
	return strings.Join(out, "\n")
}
