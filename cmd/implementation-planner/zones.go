package main

// Codex zones in planning (migration 063; codex coherence design, Part 2).
// The product's zones are read once per plan: the planner prompt lists them
// with their policies (zonesPromptText), every subtask message carries the
// zones its files fall in (attachZones), and a subtask whose files fall in a
// frozen zone is not planned — the item escalates through escalateBlock
// (blockFrozenZone) before anything is committed.

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/autonomousengine/autonomous-engine/internal/zones"
	"github.com/google/uuid"
)

// zoneLister is the slice of *zones.Store planning needs.
type zoneLister interface {
	ListForProduct(ctx context.Context, productID uuid.UUID) ([]zones.Zone, error)
}

// loadPlanningZones reads the product's zones; none without a product.
func loadPlanningZones(ctx context.Context, lister zoneLister, productID *uuid.UUID) ([]zones.Zone, error) {
	if lister == nil || productID == nil || *productID == uuid.Nil {
		return nil, nil
	}
	return lister.ListForProduct(ctx, *productID)
}

// attachZones sets each planned subtask's Zones to the zones (of its own
// repository) its files fall in, and returns the hits on frozen zones over
// all subtasks.
//
// Paths are matched relative to the repository (zones.RepoRelative with
// roots, repository id → clone path): an absolute path under the clone is
// made relative, any other absolute path never matches.
func attachZones(planned []plannedSubtask, zs []zones.Zone, roots map[string]string) []zones.Hit {
	var frozen []zones.Hit
	for i := range planned {
		msg := &planned[i].msg
		repo := strings.TrimSpace(msg.RepositoryID)
		var own []zones.Zone
		for _, z := range zs {
			if z.RepositoryID.String() == repo {
				own = append(own, z)
			}
		}
		var rel []string
		for _, f := range msg.FilesToChange {
			if r, ok := zones.RepoRelative(f, roots[repo]); ok {
				rel = append(rel, r)
			}
		}
		hits := zones.Match(own, rel)
		msg.Zones = nil
		if len(hits) > 0 {
			msg.Zones = zones.Refs(hits)
		}
		frozen = append(frozen, zones.FrozenHits(hits)...)
	}
	return frozen
}

// frozenZoneReason is the block reason (and escalation text) for a plan
// that touches frozen zones.
func frozenZoneReason(frozen []zones.Hit) string {
	parts := make([]string, 0, len(frozen))
	for _, h := range frozen {
		parts = append(parts, fmt.Sprintf("%s (planned files: %s)", h.Zone.FrozenText(), strings.Join(h.Files, ", ")))
	}
	return "Planning stopped: " + strings.Join(parts, "; ") +
		". No subtask was created and no implementation.tasks event was queued. " +
		"Thaw the zone (Admin → Products → Repository → Zones) or change the request so it stays out of the zone, then Retry to re-plan."
}

// zonesPromptText is the planner prompt's code_zones section: every zone of
// the product with its repository, policy, frozen state and patterns, and
// what each policy means. Empty when the product has no zone.
func zonesPromptText(zs []zones.Zone, repoNames map[string]string) string {
	if len(zs) == 0 {
		return ""
	}
	sorted := append([]zones.Zone{}, zs...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	var sb strings.Builder
	for _, z := range sorted {
		repo := repoNames[z.RepositoryID.String()]
		if repo == "" {
			repo = z.RepositoryID.String()
		}
		state := string(z.Policy)
		if z.Frozen() {
			reason := "no reason given"
			if z.FrozenReason != nil && strings.TrimSpace(*z.FrozenReason) != "" {
				reason = strings.TrimSpace(*z.FrozenReason)
			}
			state += ", FROZEN (" + reason + ")"
		}
		fmt.Fprintf(&sb, "- %s (%s): %s — %s\n", z.Name, repo, state, strings.Join(z.Patterns, ", "))
	}
	sb.WriteString("\nPolicies: autonomous = change freely; human_review = allowed, but a human must approve the PR; " +
		"escalate = the change stops for a human decision before verification; FROZEN = no task may change files there (the item is escalated instead of planned). " +
		"Prefer files outside escalate and frozen zones when the request can be met without them.")
	return sb.String()
}
