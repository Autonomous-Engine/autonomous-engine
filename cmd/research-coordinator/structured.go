package main

import "sort"

// aggregatedFields holds the union-deduped structured fields across all
// research tasks for a single work item. Produced by
// aggregateStructuredFields and embedded into the technical brief artifact.
type aggregatedFields struct {
	AffectedFiles        []string
	AffectedModules      []string
	AffectedZones        []string
	CrossRepoTouchpoints []string
	RelevantPatterns     []string
	RelevantAntiPatterns []string
}

// aggregateStructuredFields unions and deduplicates the structured fields
// carried on every researchResultMessage. The six slices returned are
// always non-nil (empty slice rather than nil) so the JSON artifact has a
// stable shape downstream — planners don't need to branch on null vs [].
//
// Tasks with Status != "ok" are excluded from aggregation at the caller
// level (the coordinator blocks on any failure and never reaches this
// function with a failure mixed in). We still defensively skip nil
// messages here in case a future caller wants partial aggregation.
func aggregateStructuredFields(results []researchResultMessage) aggregatedFields {
	var (
		files        []string
		modules      []string
		zones        []string
		touchpoints  []string
		patterns     []string
		antiPatterns []string
	)
	for _, r := range results {
		files = append(files, r.AffectedFiles...)
		modules = append(modules, r.AffectedModules...)
		zones = append(zones, r.AffectedZones...)
		touchpoints = append(touchpoints, r.CrossRepoTouchpoints...)
		patterns = append(patterns, r.RelevantPatterns...)
		antiPatterns = append(antiPatterns, r.RelevantAntiPatterns...)
	}
	return aggregatedFields{
		AffectedFiles:        dedupeSorted(files),
		AffectedModules:      dedupeSorted(modules),
		AffectedZones:        dedupeSorted(zones),
		CrossRepoTouchpoints: dedupeSorted(touchpoints),
		RelevantPatterns:     dedupeSorted(patterns),
		RelevantAntiPatterns: dedupeSorted(antiPatterns),
	}
}

// dedupeSorted returns a sorted deduplication of in. Always returns a
// non-nil slice (empty if no input) so JSON serialization is predictable.
func dedupeSorted(in []string) []string {
	out := []string{}
	if len(in) == 0 {
		return out
	}
	seen := make(map[string]struct{}, len(in))
	for _, s := range in {
		if s == "" {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}
