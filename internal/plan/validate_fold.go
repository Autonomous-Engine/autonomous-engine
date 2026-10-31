package plan

import (
	"fmt"
	"strings"
)

// Folding derived duplicates of a user criterion (M2-05's pre-validation
// normalisation, handed over by M2-04).
//
// Research produces several criteria per requirement: the user's own (with
// a quote) and paraphrases derived from the codebase. A derived paraphrase
// that is not owned by a task would be reported as orphaned although the
// requirement it restates is covered. Before validation, a derived criterion
// whose normalised text has token Jaccard similarity >= FoldJaccard with a
// user criterion is folded into that user criterion: its files, symbols,
// contracts and required checks are merged in (keeping the stronger
// evidence, the higher risk), every task citation of its id is rewritten to
// the user criterion's id, and it is no longer a criterion of its own — so
// it is never counted as orphaned. The fold is logged by the caller.

// FoldJaccard is the token Jaccard similarity (over NormalizeText tokens) at
// or above which a derived criterion is folded into a user criterion. The
// same value as research-coordinator's quoteMergeJaccard.
const FoldJaccard = 0.6

// Fold records one derived criterion folded into a user criterion.
type Fold struct {
	From       string  `json:"from"`
	Into       string  `json:"into"`
	Similarity float64 `json:"similarity"`
}

func (f Fold) String() string {
	return fmt.Sprintf("%s folded into %s (token Jaccard %.2f)", f.From, f.Into, f.Similarity)
}

// TokenJaccard is the Jaccard similarity of the token sets of two
// normalised texts (1 when both are empty).
func TokenJaccard(a, b string) float64 {
	sa := map[string]bool{}
	for _, w := range strings.Fields(a) {
		sa[w] = true
	}
	sb := map[string]bool{}
	for _, w := range strings.Fields(b) {
		sb[w] = true
	}
	if len(sa) == 0 && len(sb) == 0 {
		return 1
	}
	inter := 0
	for w := range sa {
		if sb[w] {
			inter++
		}
	}
	return float64(inter) / float64(len(sa)+len(sb)-inter)
}

// FoldDerivedDuplicates folds every derived criterion that duplicates a user
// criterion (see FoldJaccard) into it. A derived criterion close to several
// user criteria goes to the most similar (the first on a tie). Derived
// criteria are never folded into each other, and user criteria are never
// folded. It returns the remaining criteria in their original order (the
// user criteria merged) and the folds in criterion order; the input is not
// modified.
func FoldDerivedDuplicates(criteria []Criterion) ([]Criterion, []Fold) {
	out := make([]Criterion, len(criteria))
	copy(out, criteria)
	var users []int
	for i, c := range out {
		if c.Source.Kind == SourceUser {
			users = append(users, i)
		}
	}
	if len(users) == 0 {
		return out, nil
	}
	userText := make(map[int]string, len(users))
	for _, u := range users {
		userText[u] = NormalizeText(out[u].Text)
	}

	removed := map[int]bool{}
	var folds []Fold
	for i, c := range out {
		if c.Source.Kind != SourceDerived {
			continue
		}
		text := NormalizeText(c.Text)
		best, bestSim := -1, 0.0
		for _, u := range users {
			if s := TokenJaccard(text, userText[u]); s >= FoldJaccard && s > bestSim {
				best, bestSim = u, s
			}
		}
		if best < 0 {
			continue
		}
		out[best] = MergeCriterionEvidence(out[best], c)
		removed[i] = true
		folds = append(folds, Fold{From: c.ID, Into: out[best].ID, Similarity: bestSim})
	}
	if len(folds) == 0 {
		return out, nil
	}
	kept := make([]Criterion, 0, len(out)-len(removed))
	for i, c := range out {
		if !removed[i] {
			kept = append(kept, c)
		}
	}
	return kept, folds
}

// RewriteCriterionIDs maps every folded id to the id it was folded into,
// keeping the first occurrence of each resulting id. ids is not modified.
func RewriteCriterionIDs(ids []string, folds []Fold) []string {
	if len(folds) == 0 || len(ids) == 0 {
		return ids
	}
	into := make(map[string]string, len(folds))
	for _, f := range folds {
		into[f.From] = f.Into
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if to, ok := into[id]; ok {
			id = to
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

// MergeCriterionEvidence merges b's evidence into a: files and symbols
// unioned keeping the stronger evidence (Better), contracts and required
// checks unioned, the higher risk. a's id, text and source are kept.
func MergeCriterionEvidence(a, b Criterion) Criterion {
	a.Files = append([]FileRef(nil), a.Files...)
	a.Symbols = append([]SymbolRef(nil), a.Symbols...)
	a.Contracts = append([]ContractRef(nil), a.Contracts...)
	a.RequiredChecks = append(a.RequiredChecks[:0:0], a.RequiredChecks...)
	if riskRank(b.Risk) > riskRank(a.Risk) {
		a.Risk = b.Risk
	}
	for _, k := range b.RequiredChecks {
		found := false
		for _, have := range a.RequiredChecks {
			if NormalizeCheckKind(have) == NormalizeCheckKind(k) {
				found = true
				break
			}
		}
		if !found {
			a.RequiredChecks = append(a.RequiredChecks, k)
		}
	}
	for _, f := range b.Files {
		found := false
		for i := range a.Files {
			if a.Files[i].Path == f.Path {
				found = true
				if Better(f.Evidence, a.Files[i].Evidence) {
					a.Files[i].Evidence = f.Evidence
				}
				break
			}
		}
		if !found {
			a.Files = append(a.Files, f)
		}
	}
	for _, s := range b.Symbols {
		found := false
		for i := range a.Symbols {
			if a.Symbols[i].Name == s.Name && a.Symbols[i].File == s.File {
				found = true
				if Better(s.Evidence, a.Symbols[i].Evidence) {
					a.Symbols[i] = s
				}
				break
			}
		}
		if !found {
			a.Symbols = append(a.Symbols, s)
		}
	}
	for _, k := range b.Contracts {
		found := false
		for i := range a.Contracts {
			if a.Contracts[i].Type == k.Type && a.Contracts[i].Name == k.Name {
				found = true
				if Better(k.Evidence, a.Contracts[i].Evidence) {
					a.Contracts[i] = k
				}
				break
			}
		}
		if !found {
			a.Contracts = append(a.Contracts, k)
		}
	}
	if len(a.Files) == 0 {
		a.Files = nil
	}
	if len(a.Symbols) == 0 {
		a.Symbols = nil
	}
	if len(a.Contracts) == 0 {
		a.Contracts = nil
	}
	if len(a.RequiredChecks) == 0 {
		a.RequiredChecks = nil
	}
	return a
}

func riskRank(r Risk) int {
	switch r {
	case RiskHigh:
		return 3
	case RiskMedium:
		return 2
	case RiskLow:
		return 1
	}
	return 0
}
