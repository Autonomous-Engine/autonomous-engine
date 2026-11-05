package prefill

import (
	"strings"
	"testing"
)

// F26: "test pattern in 39 files" is a statistic, not an answer an agent
// can follow. patterns_discovered states the naming rule the files show,
// per role, with the suffix form the repository uses; test and mock files
// belong to test_conventions, not here.
func TestPatternsDiscoveredStatesTheNamingRule(t *testing.T) {
	groups := []PatternGroupSummary{
		{Pattern: "handler", Count: 12, RefFiles: []string{"internal/api/user_handler.go", "internal/api/order_handler.go"}},
		{Pattern: "service", Count: 5, RefFiles: []string{"internal/billing/invoice_service.go"}},
		{Pattern: "test", Count: 39, RefFiles: []string{"internal/api/user_handler_test.go"}},
		{Pattern: "config", Count: 1, RefFiles: []string{"config.go"}},
	}
	value, evidence, why := patternsAnswer(groups)
	if why != "" {
		t.Fatalf("not detected: %s", why)
	}
	for _, want := range []string{"`*_handler.go`", "12 files", "user_handler.go", "`*_service.go`", "5 files", "same suffix"} {
		if !strings.Contains(value, want) {
			t.Errorf("patterns answer lacks %q:\n%s", want, value)
		}
	}
	for _, not := range []string{"test pattern", "_test.go", "pattern in"} {
		if strings.Contains(value, not) {
			t.Errorf("patterns answer still says %q:\n%s", not, value)
		}
	}
	if len(evidence) < 3 || evidence[0].Path != "internal/api/user_handler.go" {
		t.Fatalf("evidence = %+v", evidence)
	}
}

func TestPatternsDiscoveredWithOnlyTestFilesIsNotDetected(t *testing.T) {
	groups := []PatternGroupSummary{
		{Pattern: "test", Count: 39, RefFiles: []string{"a_test.go"}},
		{Pattern: "mock", Count: 3, RefFiles: []string{"a_mock.go"}},
	}
	if value, _, why := patternsAnswer(groups); why == "" || !strings.Contains(why, "test") {
		t.Fatalf("value=%q why=%q, want not detected because only test and mock files group by name", value, why)
	}
}

// test_conventions says what a test file is called, where it lives and how
// the suite runs, so an agent adding a test does it the repository's way.
func TestTestConventionsNameTheFilesPlacementAndCommand(t *testing.T) {
	groups := []PatternGroupSummary{
		{Pattern: "test", Count: 39, RefFiles: []string{"internal/parser_test.go", "internal/header_test.go", "mime_test.go"}},
		{Pattern: "mock", Count: 2, RefFiles: []string{"internal/store_mock.go"}},
	}
	value, evidence, why := testConventionsAnswer(groups, "go test ./...")
	if why != "" {
		t.Fatalf("not detected: %s", why)
	}
	for _, want := range []string{"`*_test.go`", "beside the code", "39", "parser_test.go", "`go test ./...`", "`*_mock.go`"} {
		if !strings.Contains(value, want) {
			t.Errorf("test conventions lack %q:\n%s", want, value)
		}
	}
	if strings.Contains(value, "suffix convention across") {
		t.Errorf("still the statistic:\n%s", value)
	}
	if len(evidence) == 0 || evidence[0].Path != "internal/parser_test.go" {
		t.Fatalf("evidence = %+v", evidence)
	}
}

func TestTestConventionsForJestStyleTestsInATestsDirectory(t *testing.T) {
	groups := []PatternGroupSummary{
		{Pattern: "test", Count: 8, RefFiles: []string{"src/__tests__/app.test.tsx", "src/__tests__/api.test.ts"}},
	}
	value, _, why := testConventionsAnswer(groups, "npx vitest run")
	if why != "" {
		t.Fatalf("not detected: %s", why)
	}
	for _, want := range []string{"`*.test.ts`", "`__tests__`", "`npx vitest run`"} {
		if !strings.Contains(value, want) {
			t.Errorf("test conventions lack %q:\n%s", want, value)
		}
	}
	if strings.Contains(value, "_test.") || strings.Contains(value, "beside the code") {
		t.Errorf("wrong convention for a Jest-style repository:\n%s", value)
	}
}
