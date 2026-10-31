package main

import (
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/plan"
)

// The same-quote merge threshold is inclusive and equal to plan.FoldJaccard:
// two user criteria from different results quoting the same words merge at
// exactly 0.6 (3 shared of 5 distinct words) and stay apart just below it.
func TestMergeBriefCriteriaSameQuoteExactThreshold(t *testing.T) {
	if quoteMergeJaccard != plan.FoldJaccard {
		t.Fatalf("quoteMergeJaccard %v != plan.FoldJaccard %v", quoteMergeJaccard, plan.FoldJaccard)
	}
	const quote = "the one quoted sentence"
	cases := []struct {
		second    string
		sim       float64
		wantMerge bool
	}{
		{"alpha beta gamma epsilon", 0.6, true},
		{"alpha beta gamma epsilon zeta", 0.5, false},
		{"alpha beta gamma delta epsilon zeta eta", 4.0 / 7.0, false},
	}
	for _, tc := range cases {
		t.Run(tc.second, func(t *testing.T) {
			if j := tokenJaccard("alpha beta gamma delta", plan.NormalizeText(tc.second)); j != tc.sim {
				t.Fatalf("similarity = %v, want %v", j, tc.sim)
			}
			got := mergeBriefCriteria([]researchResultMessage{
				okResult("code_trace", "a", userCrit("alpha beta gamma delta", quote, plan.RiskLow)),
				okResult("impact_assessment", "b", userCrit(tc.second, quote, plan.RiskHigh)),
			})
			want := 2
			if tc.wantMerge {
				want = 1
			}
			if len(got.Criteria) != want {
				t.Fatalf("got %d criteria, want %d: %+v", len(got.Criteria), want, got.Criteria)
			}
			if tc.wantMerge && got.Criteria[0].Risk != plan.RiskHigh {
				t.Fatalf("merged criterion keeps the higher risk: %+v", got.Criteria[0])
			}
		})
	}
}
