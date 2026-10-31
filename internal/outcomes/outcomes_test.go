package outcomes

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestUpsertArgs(t *testing.T) {
	id := uuid.New()
	args, err := UpsertArgs(id, Facts{})
	if err != nil {
		t.Fatal(err)
	}
	if want := []any{id, nil, nil, nil, nil, nil, nil, nil, nil}; !reflect.DeepEqual(args, want) {
		t.Fatalf("zero facts = %#v", args)
	}
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.FixedZone("x", 3600))
	args, err = UpsertArgs(id, Facts{TaskID: "t1", Decision: DecisionHumanReview, QAResult: QAFail,
		Merge: &Merge{PRNumber: 9, Repository: "o/r", HeadSHA: "h", MergeSHA: " ", MergedAt: &at}})
	if err != nil {
		t.Fatal(err)
	}
	want := []any{id, "t1", DecisionHumanReview, QAFail, 9, "o/r", "h", nil, at.UTC()}
	if !reflect.DeepEqual(args, want) {
		t.Fatalf("args = %#v\nwant %#v", args, want)
	}
	for name, f := range map[string]Facts{
		"unknown decision": {TaskID: "t", Decision: "ship_it"},
		"decision no task": {Decision: DecisionRelease},
		"task no decision": {TaskID: "t"},
		"unknown qa":       {QAResult: "flaky"},
	} {
		if _, err := UpsertArgs(id, f); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := UpsertArgs(uuid.Nil, Facts{}); err == nil {
		t.Error("nil work item accepted")
	}
}

// The statement never writes user text and never turns unknown cost into 0.
func TestUpsertSQL_Shape(t *testing.T) {
	for _, forbidden := range []string{"title", "description", "feedback", "content->>'feedback'"} {
		if strings.Contains(UpsertSQL, forbidden) {
			t.Errorf("UpsertSQL references %q (outcomes store no user text)", forbidden)
		}
	}
	if !strings.Contains(UpsertSQL, "cost_usd IS NULL AND input_tokens + output_tokens > 0) > 0 THEN NULL") {
		t.Error("an unpriced call must make the item's cost NULL")
	}
	// verified_head_sha comes from verified evidence only; the review head is
	// a separate column (a reviewed item is not a verified one).
	if !strings.Contains(UpsertSQL, "outcome = 'verified' AND head_sha <> ''") ||
		!strings.Contains(UpsertSQL, "NULLIF((SELECT content->>'head_sha' FROM review), '') AS reviewed") ||
		!strings.Contains(UpsertSQL, "v.sha, v.reviewed,") {
		t.Error("verified_head_sha must be evidence-only and reviewed_head_sha the review head")
	}
	if strings.Contains(UpsertSQL, "COALESCE(SUM(cost_usd)") {
		t.Error("cost must never COALESCE to zero")
	}
}

func TestRevertedPRNumbers(t *testing.T) {
	cases := []struct {
		title, body string
		self        int
		want        []int
	}{
		{`Revert "Add login"`, "Reverts acme/app#123", 130, []int{123}},
		{"Reverting #5 and #6", "", 7, []int{5, 6}},
		{"revert: bad change", "This reverts PR #12. See also acme/app#12", 13, []int{12}},
		{"Fix #5", "closes #6", 7, nil},                     // no revert word
		{"Revert flaky test", "no reference", 7, nil},       // revert without a PR reference: missed
		{"Revert #7", "", 7, nil},                           // self-reference ignored
		{"Irreverent title #3", "", 9, nil},                 // word boundary
		{"Revert \"x\"", "see http://host/page#12", 9, nil}, // URL fragment is not a PR reference
	}
	for _, c := range cases {
		got := RevertedPRNumbers(c.title, c.body, c.self)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("RevertedPRNumbers(%q, %q) = %v, want %v", c.title, c.body, got, c.want)
		}
	}
}
