package main

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/autonomousengine/autonomous-engine/internal/plan"
)

// carriedDerived are the derived criteria accepted in earlier planning
// rounds (R13 D2, attempt 13). The repair prompt quotes the previous plan,
// so a model that moves a derived id into criterion_ids and drops its
// definition means the criterion it declared before: round 1's task 9
// declared AC-11, the repair cited AC-11 without declaring it, and the plan
// was blocked for citing an unknown id. The repair round's known ids now
// include every earlier derived criterion, with its text:
//   - a citation of an earlier derived id that no task of this round
//     declares gets the earlier definition back (on the first task that
//     cites it);
//   - a redeclaration of an earlier derived id is normalised by R7's rule
//     (normaliseDerivedCriteria), the earlier text being the known one:
//     the same text (token Jaccard >= plan.FoldJaccard) is a citation and
//     the earlier definition stands; a different text keeps the earlier
//     criterion cited and renumbers the new one to a fresh id the task
//     also cites, so neither intent is lost.
type carriedDerived struct {
	byID  map[string]derivedCriterion
	order []string
	// reserved are the ids of every earlier round's criteria (brief,
	// derived and folded), which a fresh id must skip.
	reserved map[string]bool
}

func newCarriedDerived() *carriedDerived {
	return &carriedDerived{byID: map[string]derivedCriterion{}, reserved: map[string]bool{}}
}

// remember records a round's accepted derived criteria (the first
// definition of an id wins: a later round cannot change an earlier text,
// restore renumbers it instead) and every id the round's criteria used.
func (c *carriedDerived) remember(b *planBuild) {
	if c == nil || b == nil {
		return
	}
	for _, t := range b.raw {
		for _, d := range t.DerivedCriteria {
			if !plan.ValidCriterionID(d.ID) {
				continue
			}
			c.reserved[d.ID] = true
			if _, ok := c.byID[d.ID]; ok {
				continue
			}
			c.byID[d.ID] = d
			c.order = append(c.order, d.ID)
		}
	}
	if b.criteria != nil {
		for _, cr := range b.criteria.Criteria {
			c.reserved[cr.ID] = true
		}
	}
	for _, f := range b.folds {
		c.reserved[f.From] = true
		c.reserved[f.Into] = true
	}
}

// reservedIDs are the ids the earlier rounds used, sorted by number: a
// fresh derived id minted in a later round (R7's renumbering in
// normaliseDerivedCriteria, the prompt's "numbering from AC-n") must skip
// them, or it collides with an earlier derived criterion and restore reads
// the new one as a redeclaration of it (R13 review M1). nil before any
// round.
func (c *carriedDerived) reservedIDs() []string {
	if c == nil || len(c.reserved) == 0 {
		return nil
	}
	out := make([]string, 0, len(c.reserved))
	for id := range c.reserved {
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool {
		return criterionNumber(out[i]) < criterionNumber(out[j]) || (criterionNumber(out[i]) == criterionNumber(out[j]) && out[i] < out[j])
	})
	return out
}

// restore applies the earlier rounds' derived criteria to this round's
// tasks, in place, and returns one log line per change.
func (c *carriedDerived) restore(tasks []decomposedTask) []string {
	if c == nil || len(c.byID) == 0 {
		return nil
	}
	next := c.nextFree(tasks)
	var notes []string
	for i := range tasks {
		t := &tasks[i]
		if len(t.DerivedCriteria) == 0 {
			continue
		}
		label := fmt.Sprintf("task %d (%q)", i+1, t.Title)
		kept := t.DerivedCriteria[:0:0]
		for _, d := range t.DerivedCriteria {
			prior, ok := c.byID[d.ID]
			if !ok {
				kept = append(kept, d)
				continue
			}
			t.CriterionIDs = appendUnique(t.CriterionIDs, d.ID)
			sim := plan.TokenJaccard(plan.NormalizeText(d.Text), plan.NormalizeText(prior.Text))
			if sim >= plan.FoldJaccard {
				notes = append(notes, fmt.Sprintf("%s: derived criterion %s restates the one an earlier round declared (token Jaccard %.2f) — cited it with its earlier definition", label, d.ID, sim))
				continue
			}
			fresh := fmt.Sprintf("AC-%d", next)
			next++
			notes = append(notes, fmt.Sprintf("%s: derived criterion %s reuses the id of one an earlier round declared, with different text (token Jaccard %.2f) — cited %s and renumbered the new one %s", label, d.ID, sim, d.ID, fresh))
			d.ID = fresh
			kept = append(kept, d)
			t.CriterionIDs = appendUnique(t.CriterionIDs, fresh)
		}
		t.DerivedCriteria = kept
	}
	// Every earlier id is now declared by no task of this round: give each
	// cited one its earlier definition back.
	for _, id := range c.order {
		for i := range tasks {
			if !containsID(tasks[i].CriterionIDs, id) {
				continue
			}
			tasks[i].DerivedCriteria = append(tasks[i].DerivedCriteria, c.byID[id])
			notes = append(notes, fmt.Sprintf("task %d (%q) cites %s, a derived criterion an earlier round declared — restored its definition (%q)", i+1, tasks[i].Title, id, c.byID[id].Text))
			break
		}
	}
	return notes
}

// nextFree is one past every AC number the earlier rounds used and this
// round cites or declares.
func (c *carriedDerived) nextFree(tasks []decomposedTask) int {
	n := 0
	bump := func(id string) {
		if v, err := strconv.Atoi(strings.TrimPrefix(id, "AC-")); err == nil && plan.ValidCriterionID(id) && v > n {
			n = v
		}
	}
	for id := range c.reserved {
		bump(id)
	}
	for _, t := range tasks {
		for _, id := range t.CriterionIDs {
			bump(id)
		}
		for _, d := range t.DerivedCriteria {
			bump(d.ID)
		}
	}
	return n + 1
}

func containsID(ids []string, id string) bool {
	for _, x := range ids {
		if x == id {
			return true
		}
	}
	return false
}
