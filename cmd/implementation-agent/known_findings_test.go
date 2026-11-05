package main

import (
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/llm/tools"
)

// Live, the implementation agent read ten files (117k tokens) before its
// first edit while research had already recorded where the charset map
// and the lookups are. The task message carries the work item's findings.
func TestTaskMessageCarriesKnownFindings(t *testing.T) {
	task := implementationTaskMessage{TaskID: "t1", WorkID: "w1", Title: "Implement WithCharsetAliases", Description: "add the option"}
	fs := []tools.CitedFinding{
		{Claim: "the encodings map is keyed lower-case", Path: "internal/coding/charsets.go", StartLine: 23, EndLine: 24, Excerpt: "var encodings = map[string]encodingEntry{"},
		{Claim: "ConvertToUTF8String lowercases the charset", Path: "internal/coding/charsets.go", StartLine: 303, EndLine: 315, Excerpt: "charset = strings.ToLower(charset)"},
	}
	msg := taskMessageWithFindings(task, fs)
	if !strings.HasPrefix(msg, buildTaskMessage(task)) {
		t.Fatal("the task message itself must come first, unchanged")
	}
	for _, want := range []string{tools.KnownFindingsHeader, "(internal/coding/charsets.go:23-24)", "(internal/coding/charsets.go:303-315)", `"charset = strings.ToLower(charset)"`} {
		if !strings.Contains(msg, want) {
			t.Errorf("message lacks %q", want)
		}
	}
	if taskMessageWithFindings(task, nil) != buildTaskMessage(task) {
		t.Fatal("no findings: the message is unchanged")
	}
}
