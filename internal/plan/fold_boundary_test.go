package plan

import "testing"

// The fold threshold is inclusive: a derived criterion whose token Jaccard
// with a user criterion is exactly FoldJaccard (3 shared of 5 distinct
// words = 0.6) is folded; one just below (3 of 6 = 0.5, and 4 of 7 ≈ 0.571)
// is not.
func TestFoldDerivedDuplicatesExactThresholdBoundary(t *testing.T) {
	user := Criterion{ID: "AC-1", Text: "alpha beta gamma delta", Source: Source{Kind: SourceUser, Quote: "q"}, Risk: RiskLow}
	cases := []struct {
		derived  string
		wantSim  float64
		wantFold bool
	}{
		{"alpha beta gamma epsilon", 0.6, true},
		{"Alpha, beta; GAMMA — epsilon!", 0.6, true}, // normalisation does not move the boundary
		{"alpha beta gamma epsilon zeta", 0.5, false},
		{"alpha beta gamma delta epsilon zeta eta", 4.0 / 7.0, false},
	}
	for _, tc := range cases {
		t.Run(tc.derived, func(t *testing.T) {
			if got := TokenJaccard(NormalizeText(tc.derived), NormalizeText(user.Text)); got != tc.wantSim {
				t.Fatalf("similarity = %v, want %v", got, tc.wantSim)
			}
			derived := Criterion{ID: "AC-2", Text: tc.derived, Source: Source{Kind: SourceDerived, Reason: "r"}, Risk: RiskLow}
			kept, folds := FoldDerivedDuplicates([]Criterion{user, derived})
			if tc.wantFold {
				if len(folds) != 1 || folds[0].Similarity != FoldJaccard || len(kept) != 1 {
					t.Fatalf("at exactly %v: folds %+v kept %d, want one fold", FoldJaccard, folds, len(kept))
				}
				return
			}
			if len(folds) != 0 || len(kept) != 2 {
				t.Fatalf("below %v: folds %+v kept %d, want none", FoldJaccard, folds, len(kept))
			}
		})
	}
}
