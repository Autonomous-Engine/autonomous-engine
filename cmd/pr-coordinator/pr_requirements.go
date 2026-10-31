package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"

	"github.com/autonomousengine/autonomous-engine/internal/plan"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Requirements stated for this change (R13 D3, attempt 13). A criterion
// that targets the PR text ("the pull request description MUST state …",
// plan.TargetsPRText) is satisfied by the description this service writes.
// When no kept code step owns it, the planner marks it non-code with
// plan.PRTextReason and records it on every subtask's plan_tasks message
// (non_code_criteria); review's verifier records it as not_verifiable with
// its gap line. When a code step owns it ("implement X and say so in the PR
// description"), it is one of that subtask's criteria and the verifier
// judges it against the code. Either way the PR description lists its text
// under "Requirements stated for this change", so the PR carries the
// statement and a human can check the two side by side. R15 T2: the
// section also lists, labelled Intake, the intake's own PR-text sentences
// (pr_requirements on the plan task message), so a requirement the brief
// never made a criterion (live-run attempt 13b) still reaches the PR.

// prRequirementsHeading is the PR description section's heading.
const prRequirementsHeading = "## Requirements stated for this change"

// planTaskRowQuerier is the pool surface loadPRTextRequirements needs.
type planTaskRowQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// loadPRTextRequirements reads the subtask's plan_tasks message and returns
// its PR-text criteria (prTextRequirements). A subtask with no plan_tasks
// row (a legacy item) has none; a read error is logged and yields none: the
// PR still opens, and the verifier's gap line still sends the criterion to
// a human.
func loadPRTextRequirements(ctx context.Context, db planTaskRowQuerier, taskID uuid.UUID) []plan.Criterion {
	var raw []byte
	err := db.QueryRow(ctx, `SELECT message FROM plan_tasks WHERE id = $1`, taskID).Scan(&raw)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return nil
	case err != nil:
		log.Printf("pr-coordinator: read plan task %s for the PR-text requirements: %v", taskID, err)
		return nil
	}
	return prTextRequirements(raw)
}

// prTextRequirements decodes a plan_tasks message (an implementation.tasks
// payload) and returns, de-duplicated by id in message order, the criteria
// whose text targets the PR text: the non-code ones the planner marked, and
// any the subtask cites (criteria) whose text targets the PR text all the
// same. Only the structured criteria are read, never the prose.
//
// R15 T2: then the intake's PR-text sentences (pr_requirements, which
// research extracts from the work item description and the PRD and the
// planner copies onto every subtask), each as a criterion with no id,
// verbatim; one that restates a listed criterion or an earlier sentence
// (plan.RestatesRequirement) is left out.
func prTextRequirements(raw []byte) []plan.Criterion {
	var m struct {
		Criteria        []plan.Criterion `json:"criteria"`
		NonCodeCriteria []plan.Criterion `json:"non_code_criteria"`
		PRRequirements  []string         `json:"pr_requirements"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		log.Printf("pr-coordinator: plan task message unreadable for the PR-text requirements: %v", err)
		return nil
	}
	seen := map[string]bool{}
	var out []plan.Criterion
	for _, c := range append(m.NonCodeCriteria, m.Criteria...) {
		if !plan.ValidCriterionID(c.ID) || strings.TrimSpace(c.Text) == "" || seen[c.ID] || !plan.IsPRTextCriterion(c) {
			continue
		}
		seen[c.ID] = true
		out = append(out, c)
	}
intake:
	for _, s := range m.PRRequirements {
		if s = strings.TrimSpace(s); s == "" {
			continue
		}
		for _, c := range out {
			if plan.RestatesRequirement(s, c.Text) {
				continue intake
			}
		}
		out = append(out, plan.Criterion{Text: s})
	}
	return out
}

// prRequirementsIntakeNote follows the intro when the section lists an
// intake sentence (R15 T2).
const prRequirementsIntakeNote = "Items marked Intake are quoted from the work item's request."

// prRequirementsIntakeLabel labels an intake sentence (a requirement with
// no criterion id).
const prRequirementsIntakeLabel = "Intake"

// prRequirementsIntro opens the section. It covers both kinds of PR-text
// criterion: one a code step owns is also verified against the code; one
// no code step owns is recorded as a gap (R13 ruling).
const prRequirementsIntro = "Requirements this change states in its PR text; those owned by a code step are also verified against the code, the rest are recorded as gaps for review."

// The section's bounds (R13 review minor 3): at most prRequirementsMaxItems
// criteria, each text at most prRequirementsMaxChars characters.
const (
	prRequirementsMaxItems = 20
	prRequirementsMaxChars = 500
)

// renderPRRequirements is the PR description section for reqs ("" when
// there are none): each criterion's id and its text, verbatim but with its
// whitespace and newlines collapsed to single spaces (one list item per
// criterion), cut at prRequirementsMaxChars characters; at most
// prRequirementsMaxItems items, then a line saying how many were left out.
func renderPRRequirements(reqs []plan.Criterion) string {
	if len(reqs) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString(prRequirementsHeading + "\n\n")
	sb.WriteString(prRequirementsIntro)
	for _, c := range reqs {
		if c.ID == "" {
			sb.WriteString(" " + prRequirementsIntakeNote)
			break
		}
	}
	sb.WriteString("\n\n")
	for i, c := range reqs {
		if i == prRequirementsMaxItems {
			fmt.Fprintf(&sb, "- … and %d more (see the plan's criteria)\n", len(reqs)-prRequirementsMaxItems)
			break
		}
		text := strings.Join(strings.Fields(c.Text), " ")
		if r := []rune(text); len(r) > prRequirementsMaxChars {
			text = string(r[:prRequirementsMaxChars]) + "…"
		}
		label := c.ID
		if label == "" {
			label = prRequirementsIntakeLabel
		}
		sb.WriteString("- **" + label + "**: " + text + "\n")
	}
	sb.WriteString("\n")
	return sb.String()
}
