package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"

	"github.com/autonomousengine/autonomous-engine/internal/llm/strategy"
)

// Acceptance criteria that cannot both hold.
//
// Live (ciblame item 0f565594): the PRD said both "rows sorted ascending"
// and "`--top` keeps the newest days". The PRD's own self-review pass did
// not catch it, the planner could not give the ascending criterion to any
// task, and the verifier escalated it to a person — after research,
// planning and implementation had all run. A dedicated check asks the
// model, with the PRD alone in front of it, for the criteria that cannot
// both be satisfied; a conflicting draft is regenerated once with the
// conflicts as reviewer guidance, and whatever still conflicts is shown on
// the PRD approval prompt, the gate that already exists for a person to
// settle it before anything downstream spends a token on it. The check
// never blocks a PRD: a failed check keeps the draft as it is.

// prdConflict is a set of acceptance criteria that cannot all hold, as the
// check_prd_conflicts intent reports it.
type prdConflict struct {
	Criteria []string `json:"criteria"`
	Conflict string   `json:"conflict"`
}

// intentExecutor is the slice of *llm.Service the conflict check needs.
type intentExecutor interface {
	Execute(ctx context.Context, intent strategy.Intent) (*strategy.Result, error)
}

// conflictCheckFormat is the answer the check asks for.
const conflictCheckFormat = `JSON only, no prose: {"conflicts":[{"criteria":["AC-n","AC-m"],"conflict":"one sentence: why both cannot hold"}]}. Cite criteria by their ids when the PRD numbers them, otherwise by a short quote. An empty array when every criterion can hold at once.`

// parsePRDConflicts reads the check's answer: the outermost JSON object in
// it (fenced or bare, prose around it ignored). An entry naming fewer than
// two criteria, or saying nothing, is not a conflict.
func parsePRDConflicts(answer string) ([]prdConflict, error) {
	start, end := strings.Index(answer, "{"), strings.LastIndex(answer, "}")
	if start < 0 || end <= start {
		return nil, fmt.Errorf("no JSON object in the answer")
	}
	var out struct {
		Conflicts []prdConflict `json:"conflicts"`
	}
	if err := json.Unmarshal([]byte(answer[start:end+1]), &out); err != nil {
		return nil, fmt.Errorf("decode conflicts: %w", err)
	}
	var cs []prdConflict
	for _, c := range out.Conflicts {
		var ids []string
		seen := map[string]bool{}
		for _, id := range c.Criteria {
			id = strings.TrimSpace(id)
			if id == "" || seen[id] {
				continue
			}
			seen[id] = true
			ids = append(ids, id)
		}
		if len(ids) < 2 || strings.TrimSpace(c.Conflict) == "" {
			continue
		}
		cs = append(cs, prdConflict{Criteria: ids, Conflict: strings.TrimSpace(c.Conflict)})
	}
	return cs, nil
}

// checkPRDConflicts asks the model which of the PRD's acceptance criteria
// cannot both hold.
func checkPRDConflicts(ctx context.Context, llm intentExecutor, prd string) ([]prdConflict, error) {
	res, err := llm.Execute(ctx, strategy.Intent{
		Type:         strategy.IntentCheckPRDConflicts,
		Context:      map[string]string{"prd": prd},
		OutputFormat: conflictCheckFormat,
		MaxTokens:    1024,
	})
	if err != nil {
		return nil, err
	}
	return parsePRDConflicts(res.Content)
}

// joinIDs is "AC-3 and AC-5", "AC-1, AC-2 and AC-3".
func joinIDs(ids []string) string {
	switch len(ids) {
	case 0:
		return ""
	case 1:
		return ids[0]
	}
	return strings.Join(ids[:len(ids)-1], ", ") + " and " + ids[len(ids)-1]
}

// conflictGuidance is the reviewer guidance the regeneration carries.
func conflictGuidance(cs []prdConflict) string {
	var b strings.Builder
	b.WriteString("The previous draft's acceptance criteria conflict with each other. For each conflict below choose one behaviour, state it in the criteria, and rewrite or drop the criterion that contradicts it — never keep both. Keep everything else as it was.\n")
	for _, c := range cs {
		fmt.Fprintf(&b, "- %s: %s\n", joinIDs(c.Criteria), c.Conflict)
	}
	return strings.TrimSpace(b.String())
}

// conflictPromptBlock opens the PRD approval prompt when conflicts remain:
// the person settles them here, before research or code.
func conflictPromptBlock(cs []prdConflict) string {
	if len(cs) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("⚠ Conflicting acceptance criteria — the PRD cannot satisfy these together, and the model did not settle them. Reject with the behaviour you want, or approve and the planner will choose.\n\n")
	for _, c := range cs {
		fmt.Fprintf(&b, "- %s: %s\n", joinIDs(c.Criteria), c.Conflict)
	}
	b.WriteString("\n---\n\n")
	return b.String()
}

// reconcilePRD checks the draft's acceptance criteria for conflicts; when
// there are any it regenerates the PRD once with them as guidance and
// checks again. It returns the PRD to go on with and the conflicts a person
// still has to settle. Nothing here blocks the draft: a failed first check
// keeps it with no conflicts recorded; a failed or empty regeneration keeps
// it with the conflicts found; a failed second check keeps the regenerated
// PRD with the first check's conflicts, for the person to look at.
func reconcilePRD(ctx context.Context, llm intentExecutor, regenerate func(ctx context.Context, guidance string) (string, error), prd string) (string, []prdConflict) {
	cs, err := checkPRDConflicts(ctx, llm, prd)
	if err != nil {
		log.Printf("product-agent: PRD conflict check failed: %v — keeping the draft", err)
		return prd, nil
	}
	if len(cs) == 0 {
		return prd, nil
	}
	log.Printf("product-agent: the PRD's acceptance criteria conflict (%d) — regenerating once with the conflicts as guidance", len(cs))
	again, err := regenerate(ctx, conflictGuidance(cs))
	if err != nil {
		log.Printf("product-agent: PRD regeneration failed: %v — keeping the draft and its conflicts", err)
		return prd, cs
	}
	again = stripPRDPreamble(again)
	if strings.TrimSpace(again) == "" {
		log.Printf("product-agent: PRD regeneration returned nothing — keeping the draft and its conflicts")
		return prd, cs
	}
	remaining, err := checkPRDConflicts(ctx, llm, again)
	if err != nil {
		log.Printf("product-agent: PRD conflict re-check failed: %v — keeping the regenerated PRD with the conflicts first found", err)
		return again, cs
	}
	if len(remaining) > 0 {
		log.Printf("product-agent: %d acceptance-criteria conflict(s) remain after regeneration — shown on the approval prompt", len(remaining))
	}
	return again, remaining
}
