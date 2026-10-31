package main

import (
	"fmt"
	"strings"
	"testing"
)

// Review minor 2: the reasons of subtasks whose verdict could not be read
// are bounded and counted, never cut silently.
func TestUnreadSectionCountsWhatDoesNotFit(t *testing.T) {
	var missing []string
	for i := 0; i < 20; i++ {
		missing = append(missing, fmt.Sprintf("Independent verifier requested changes on subtask %02d: %s", i, strings.Repeat("r", 500)))
	}
	got := unreadSection(missing, 2000)
	if len(got) > 2000 {
		t.Fatalf("section is %d bytes, over 2000", len(got))
	}
	if !strings.Contains(got, "more subtask(s) requested changes whose verdict could not be read") {
		t.Errorf("section does not count the reasons it dropped:\n%s", got)
	}
	if !strings.HasPrefix(got, "Independent verifier requested changes on subtask 00") {
		t.Errorf("section does not start with the first reason:\n%s", got)
	}
	if unreadSection(nil, 2000) != "" {
		t.Error("no missing reasons must give an empty section")
	}
	if one := unreadSection(missing[:1], 2000); one != missing[0] {
		t.Errorf("one reason = %q", one)
	}
}
