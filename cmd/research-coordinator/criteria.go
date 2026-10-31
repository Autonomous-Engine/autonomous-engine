package main

import (
	"fmt"
	"log"
	"sort"
	"strings"

	"github.com/autonomousengine/autonomous-engine/internal/evidence"
	"github.com/autonomousengine/autonomous-engine/internal/plan"
)

// Acceptance criteria merge (M2-04). Every research task (one per research
// type, across the item's repositories) extracts its own criteria from the
// same request; the brief carries one merged set:
//
//   - results are merged in (FindingType, TaskID) order, so the AC numbering
//     is reproducible whatever order the results arrived in;
//   - criteria with the same normalised text (plan.NormalizeText) are one
//     criterion;
//   - two user criteria quoting the same normalised words are one criterion
//     only when they come from DIFFERENT results and their normalised texts
//     overlap strongly: token Jaccard similarity >= quoteMergeJaccard (0.6).
//     One quote can carry two distinct requirements (one PRD sentence with
//     two clauses), and one task stating both must keep both; a weaker
//     overlap is kept apart rather than risk dropping a requirement.
//     Clustering derived paraphrases of a user criterion is M2-05's;
//   - of duplicates, the user-sourced one with a quote wins the source; the
//     evidence is the richest of all copies: files and symbols are unioned
//     by path / name keeping the strongest evidence (plan.Better), contracts
//     by type+name, required checks unioned, risk is the highest;
//   - ids are renumbered AC-1..n in order of first appearance;
//   - FileEvidence is rebuilt from the merged criteria.
//
// A result without criteria (a producer older than M2-04) contributes
// nothing; when no result carries criteria the brief has none (legacy).

// quoteMergeJaccard is the minimum token Jaccard similarity of two
// normalised criterion texts for a same-quote merge across results.
const quoteMergeJaccard = 0.6

// tokenJaccard is |A∩B| / |A∪B| over the word sets of two normalised texts.
func tokenJaccard(a, b string) float64 {
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

// mergeCluster is one merged criterion and where it came from.
type mergeCluster struct {
	texts   []string     // normalised texts merged into it
	origins map[int]bool // indices (in merge order) of the results that contributed
}

// mergeBriefCriteria merges the criteria of every ok result. nil when no
// result carries any.
func mergeBriefCriteria(results []researchResultMessage) *plan.BriefCriteria {
	ordered := append([]researchResultMessage(nil), results...)
	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].FindingType != ordered[j].FindingType {
			return ordered[i].FindingType < ordered[j].FindingType
		}
		return ordered[i].TaskID < ordered[j].TaskID
	})

	var merged []plan.Criterion
	var clusters []*mergeCluster
	byText := map[string]int{}
	byQuote := map[string][]int{}
	sawCriteria := false
	for ri, r := range ordered {
		if r.Status != ResearchStatusOK || r.Criteria == nil {
			continue
		}
		sawCriteria = true
		for _, c := range r.Criteria.Criteria {
			key := plan.NormalizeText(c.Text)
			if key == "" {
				continue
			}
			quote := ""
			if c.Source.Kind == plan.SourceUser {
				quote = plan.NormalizeText(c.Source.Quote)
			}
			i, ok := byText[key]
			if !ok && quote != "" {
				for _, cand := range byQuote[quote] {
					cl := clusters[cand]
					if cl.origins[ri] {
						continue // never quote-merge two criteria of one result
					}
					best := 0.0
					for _, t := range cl.texts {
						if j := tokenJaccard(key, t); j > best {
							best = j
						}
					}
					if best >= quoteMergeJaccard {
						i, ok = cand, true
						break
					}
				}
			}
			if ok {
				merged[i] = mergeCriterion(merged[i], c)
			} else {
				i = len(merged)
				merged = append(merged, cloneCriterion(c))
				clusters = append(clusters, &mergeCluster{origins: map[int]bool{}})
			}
			cl := clusters[i]
			cl.origins[ri] = true
			if _, seen := byText[key]; !seen {
				cl.texts = append(cl.texts, key)
				byText[key] = i
			}
			if quote != "" {
				listed := false
				for _, x := range byQuote[quote] {
					if x == i {
						listed = true
						break
					}
				}
				if !listed {
					byQuote[quote] = append(byQuote[quote], i)
				}
			}
		}
	}
	if !sawCriteria {
		return nil
	}
	for i := range merged {
		merged[i].ID = fmt.Sprintf("AC-%d", i+1)
	}
	out := &plan.BriefCriteria{
		SchemaVersion: plan.SchemaVersion,
		Criteria:      merged,
		FileEvidence:  plan.BuildFileEvidence(merged),
	}
	if out.Criteria == nil {
		out.Criteria = []plan.Criterion{}
	}
	if err := out.Validate(); err != nil {
		// Inputs were validated by the producers; a merge that breaks a rule
		// is a bug here. Keep the brief without criteria rather than write
		// an invalid set.
		log.Printf("research-coordinator: merged criteria are invalid (%v) — brief written without criteria", err)
		return nil
	}
	return out
}

func cloneCriterion(c plan.Criterion) plan.Criterion {
	c.Files = append([]plan.FileRef(nil), c.Files...)
	c.Symbols = append([]plan.SymbolRef(nil), c.Symbols...)
	c.Contracts = append([]plan.ContractRef(nil), c.Contracts...)
	c.RequiredChecks = append([]evidence.CheckKind(nil), c.RequiredChecks...)
	return c
}

func riskOrder(r plan.Risk) int {
	switch r {
	case plan.RiskHigh:
		return 3
	case plan.RiskMedium:
		return 2
	case plan.RiskLow:
		return 1
	}
	return 0
}

// mergeCriterion folds b into a (a was seen first).
func mergeCriterion(a, b plan.Criterion) plan.Criterion {
	if a.Source.Kind != plan.SourceUser && b.Source.Kind == plan.SourceUser {
		a.Source = b.Source
	}
	if riskOrder(b.Risk) > riskOrder(a.Risk) {
		a.Risk = b.Risk
	}
	for _, k := range b.RequiredChecks {
		if !containsCheck(a.RequiredChecks, k) {
			a.RequiredChecks = append(a.RequiredChecks, k)
		}
	}
	for _, f := range b.Files {
		found := false
		for i := range a.Files {
			if a.Files[i].Path == f.Path {
				found = true
				if plan.Better(f.Evidence, a.Files[i].Evidence) {
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
			if a.Symbols[i].Name == s.Name && (a.Symbols[i].File == s.File || a.Symbols[i].File == "" || s.File == "") {
				found = true
				if plan.Better(s.Evidence, a.Symbols[i].Evidence) {
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
			if a.Contracts[i].Name == k.Name && a.Contracts[i].Type == k.Type {
				found = true
				if plan.Better(k.Evidence, a.Contracts[i].Evidence) {
					a.Contracts[i] = k
				}
				break
			}
		}
		if !found {
			a.Contracts = append(a.Contracts, k)
		}
	}
	return a
}

func containsCheck(ks []evidence.CheckKind, k evidence.CheckKind) bool {
	for _, x := range ks {
		if x == k {
			return true
		}
	}
	return false
}
