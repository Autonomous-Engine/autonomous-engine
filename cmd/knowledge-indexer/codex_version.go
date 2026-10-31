package main

import (
	"encoding/json"
	"path"
	"path/filepath"
	"sort"

	"github.com/autonomousengine/autonomous-engine/internal/codex"
	"github.com/autonomousengine/autonomous-engine/internal/models"
	"github.com/google/uuid"
)

// Codex versions written by index runs (migration 060).
//
//   - A full index of a product's repository records one version per
//     repository and branch with section:codebase_architecture (rewritten
//     every run) and module:<name> for each module whose row changed
//     (codex.UpsertModuleForRepoBranchChanged). It always writes one, as
//     before: the first version is what guards.CheckOnboardingComplete looks
//     for, and a run with no module change carries no module: entry.
//   - An incremental index records a version only for a product's
//     repository (the trigger resolved the repository, so its product is
//     known) and only when a module row changed; the legacy REPO_PATH clone
//     has no product and writes codex rows but no version.
//
// Each version enqueues codex.indexed in its own transaction
// (codex.RecordVersion).

// architectureSection is the codex section every full index rewrites.
const architectureSection = "codebase_architecture"

// fullIndexVersion is the version a full index of one repository writes.
func fullIndexVersion(productID uuid.UUID, branch string, changedModules []string) codex.NewVersion {
	sections := []string{codex.SectionChange(architectureSection)}
	for _, m := range sortedCopy(changedModules) {
		sections = append(sections, codex.ModuleChange(m))
	}
	return codex.NewVersion{
		ProductID:       productID,
		Branch:          branch,
		SectionsChanged: sections,
		Summary:         "Automated codebase analysis by knowledge-indexer",
		Source:          models.CodexChangeSourceAutomatedReanalysis,
		Diff:            json.RawMessage(`{}`),
		Trigger:         codex.TriggerFullIndex,
	}
}

// incrementalIndexVersion is the version an incremental index of st writes,
// ok false when it writes none (no product, or no module row changed).
func incrementalIndexVersion(st indexStep, branch string, changedModules []string) (codex.NewVersion, bool) {
	if st.productID == uuid.Nil || len(changedModules) == 0 {
		return codex.NewVersion{}, false
	}
	sections := make([]string, 0, len(changedModules))
	for _, m := range sortedCopy(changedModules) {
		sections = append(sections, codex.ModuleChange(m))
	}
	return codex.NewVersion{
		ProductID:       st.productID,
		Branch:          branch,
		SectionsChanged: sections,
		Summary:         "Incremental codebase analysis by knowledge-indexer",
		Source:          models.CodexChangeSourceAutomatedReanalysis,
		Diff:            json.RawMessage(`{}`),
		Trigger:         codex.TriggerIncrementalIndex,
	}, true
}

// mergeDependents is the dependents written for module name. existing is
// nil for a full index (the computed list is complete); an incremental
// index sees only the changed modules, so its computed dependents are added
// to the stored ones (existing[name]) rather than replacing them. Never nil.
func mergeDependents(existing map[string][]string, name string, computed []string) []string {
	if existing == nil {
		return append([]string{}, computed...)
	}
	return mergeUnique(existing[name], computed)
}

// storedModuleNames is the names of stored, the import targets an
// incremental run resolves against besides its own modules.
func storedModuleNames(stored []*models.CodexModule) []string {
	out := make([]string, 0, len(stored))
	for _, m := range stored {
		out = append(out, m.Name)
	}
	return out
}

// storedDependentsOf is stored's dependents by module name (mergeDependents).
// Never nil, so an incremental run always merges.
func storedDependentsOf(stored []*models.CodexModule) map[string][]string {
	out := make(map[string][]string, len(stored))
	for _, m := range stored {
		out[m.Name] = m.Dependents
	}
	return out
}

// moduleFilesFor is every file of all in a directory one of changed is in
// (changed are git paths, relative with forward slashes, like
// fileInfo.Path). Modules are directories (analyzeModules), so an
// incremental run must analyse the whole directory: analysing the changed
// files alone would rewrite the module's row with only their functions.
func moduleFilesFor(all []fileInfo, changed []string) []fileInfo {
	dirs := make(map[string]bool, len(changed))
	for _, c := range changed {
		dirs[path.Dir(filepath.ToSlash(c))] = true
	}
	var out []fileInfo
	for _, f := range all {
		if dirs[path.Dir(f.Path)] {
			out = append(out, f)
		}
	}
	return out
}

// sortedCopy is in sorted and deduplicated, empty names dropped.
func sortedCopy(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}
