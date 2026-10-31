package main

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
)

const (
	pinA = "7af0d7084cd8b7c8a96b50da4311b560bebcb8e8"
	pinB = "0123456789abcdef0123456789abcdef01234567"
)

func TestResearchResult_DecodesBaseSHAsAndCodexVersion(t *testing.T) {
	var r researchResultMessage
	if err := json.Unmarshal([]byte(`{"task_id":"t","status":"ok","base_shas":{"repo-1":"`+pinA+`"},"codex_version":7}`), &r); err != nil {
		t.Fatal(err)
	}
	if r.BaseSHAs["repo-1"] != pinA || r.CodexVersion != 7 {
		t.Fatalf("decoded %+v", r)
	}
}

func TestBriefCodexPin_HighestSuccessfulOnTheBranchResearchRead(t *testing.T) {
	work, product := uuid.New(), uuid.New()
	noFallback := func() (string, error) { t.Fatal("fallback branch consulted"); return "", nil }
	noCurrent := func(string) (int, error) { t.Fatal("current version consulted"); return 0, nil }
	pin, ok := briefCodexPin(work, product, []researchResultMessage{
		{Status: ResearchStatusOK, CodexVersion: 4, CodexBranch: "develop"},
		{Status: ResearchStatusError, CodexVersion: 9, CodexBranch: "develop"},
		{Status: ResearchStatusOK, CodexVersion: 6, CodexBranch: "develop"},
		{Status: ResearchStatusOK, CodexBranch: "develop"},
	}, []string{"b", "a"}, noFallback, noCurrent)
	if !ok || pin.WorkItemID != work || pin.ProductID != product || pin.Branch != "develop" || pin.ProductVersion != 6 || strings.Join(pin.Modules, ",") != "a,b" {
		t.Fatalf("pin = %+v, %v; want develop v6 (failed results do not count), modules a,b", pin, ok)
	}
}

// Results from a pre-060 research-agent carry a global version_number and
// no codex_branch: it is not a product_version, so the pin takes the
// item's codex branch and that branch's current version instead.
func TestBriefCodexPin_LegacyResultsUseTheCurrentVersion(t *testing.T) {
	var askedBranch string
	pin, ok := briefCodexPin(uuid.New(), uuid.New(), []researchResultMessage{
		{Status: ResearchStatusOK, CodexVersion: 812},
	}, nil, func() (string, error) { return "main", nil }, func(b string) (int, error) { askedBranch = b; return 3, nil })
	if !ok || pin.Branch != "main" || pin.ProductVersion != 3 || askedBranch != "main" || pin.Modules == nil {
		t.Fatalf("pin = %+v, %v (asked %q)", pin, ok, askedBranch)
	}

	// Results that disagree on the branch: the item's codex branch decides,
	// and only results on it count.
	pin, ok = briefCodexPin(uuid.New(), uuid.New(), []researchResultMessage{
		{Status: ResearchStatusOK, CodexVersion: 5, CodexBranch: "main"},
		{Status: ResearchStatusOK, CodexVersion: 9, CodexBranch: "feature"},
	}, nil, func() (string, error) { return "main", nil }, func(string) (int, error) { t.Fatal("current consulted"); return 0, nil })
	if !ok || pin.Branch != "main" || pin.ProductVersion != 5 {
		t.Fatalf("disagreeing results: pin = %+v, %v", pin, ok)
	}
}

func TestBriefCodexPin_NoPinWithoutProductOrBranch(t *testing.T) {
	if _, ok := briefCodexPin(uuid.New(), uuid.Nil, nil, nil, nil, nil); ok {
		t.Error("pinned without a product")
	}
	_, ok := briefCodexPin(uuid.New(), uuid.New(), []researchResultMessage{{Status: ResearchStatusOK}}, nil,
		func() (string, error) { return "", errors.New("no default branch") }, func(string) (int, error) { return 1, nil })
	if ok {
		t.Error("pinned without a branch")
	}
	_, ok = briefCodexPin(uuid.New(), uuid.New(), []researchResultMessage{{Status: ResearchStatusOK}}, nil,
		func() (string, error) { return "main", nil }, func(string) (int, error) { return 0, errors.New("db down") })
	if ok {
		t.Error("pinned without a version")
	}
}

func TestTechnicalBrief_RecordsBaseSHAsAndCodexVersion(t *testing.T) {
	b, err := json.Marshal(technicalBrief{BaseSHAs: map[string]string{"repo-1": pinA}, CodexVersion: 6})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"base_shas":{"repo-1":"` + pinA + `"}`, `"codex_version":6`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("brief JSON %s lacks %s", b, want)
		}
	}
	legacy, _ := json.Marshal(technicalBrief{})
	if strings.Contains(string(legacy), "codex_version") || strings.Contains(string(legacy), "base_shas") {
		t.Errorf("a legacy brief must omit both fields: %s", legacy)
	}
}

func TestCheckResultBases_CountsReadsOffThePin(t *testing.T) {
	pins := map[string]string{"repo-1": pinA}
	n := checkResultBases(uuid.New(), pins, []researchResultMessage{
		{TaskID: "ok-same", Status: ResearchStatusOK, BaseSHAs: map[string]string{"repo-1": pinA}},
		{TaskID: "ok-off", Status: ResearchStatusOK, BaseSHAs: map[string]string{"repo-1": pinB}},
		{TaskID: "failed-off", Status: ResearchStatusError, BaseSHAs: map[string]string{"repo-1": pinB}},
		{TaskID: "legacy", Status: ResearchStatusOK},
	})
	if n != 1 {
		t.Fatalf("mismatches = %d, want 1", n)
	}
}
