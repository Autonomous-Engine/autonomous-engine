package verifier

import (
	"context"
	"fmt"

	"github.com/google/uuid"
)

// Sources are the only things an Input is built from. Like Input, it has
// no field that could carry the author's prose: the requirements come from
// the planner's structured specs and the research brief's criteria, the
// evidence from the gate's record and the controller's event stream, the
// diff from git.
type Sources struct {
	WorkID       uuid.UUID
	Requirements Requirements
	Evidence     Evidence
	Diff         Diff
	// RepoTree is the scratch repository at head ("" when unavailable).
	RepoTree    string
	Unavailable string
}

// Build assembles the verifier's Input.
func Build(s Sources) Input {
	in := Input{
		WorkID:      s.WorkID.String(),
		Criteria:    s.Requirements.Criteria,
		TaskSpecs:   s.Requirements.TaskSpecs,
		Diff:        s.Diff,
		Evidence:    s.Evidence,
		RepoTree:    s.RepoTree,
		Unavailable: s.Unavailable,
		Tools:       ToolNames(),
	}
	if in.RepoTree == "" && in.Unavailable == "" {
		in.Unavailable = "no repository tree at the head commit"
	}
	return in
}

// Load reads a work item's requirements and recorded evidence.
func Load(ctx context.Context, q Querier, workID uuid.UUID) (Requirements, Evidence, error) {
	req, err := LoadRequirements(ctx, q, workID)
	if err != nil {
		return req, Evidence{}, fmt.Errorf("load requirements: %w", err)
	}
	ev, err := LoadEvidence(ctx, q, workID)
	if err != nil {
		return req, ev, fmt.Errorf("load evidence: %w", err)
	}
	return req, ev, nil
}
