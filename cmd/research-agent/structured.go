package main

import (
	"context"
	"encoding/json"
	"log"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/autonomousengine/autonomous-engine/internal/zones"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// toolCallRecord is one entry in the side-channel history captured during
// the research tool loop. Approach A in the pivot-B plan: a deterministic
// capture of every tool invocation, used post-loop to derive structured
// fields (affected_files, affected_modules, ...) without asking the LLM
// for JSON summaries.
type toolCallRecord struct {
	// callID is the tool call id the model issued (M2-04: acceptance
	// criteria cite it as evidence). Empty when the provider sent none.
	callID    string
	tool      string
	arguments string
	result    string
	isError   bool
}

// toolHistory accumulates tool-call records in a goroutine-safe manner.
// The existing executor.SetCallback pattern streams events to the
// dashboard; we wrap that callback so both consumers see every call.
type toolHistory struct {
	mu    sync.Mutex
	calls []toolCallRecord
}

func newToolHistory() *toolHistory {
	return &toolHistory{}
}

func (h *toolHistory) add(tool, arguments, result string, isError bool) {
	h.record("", tool, arguments, result, isError)
}

// record is add with the tool call id.
func (h *toolHistory) record(callID, tool, arguments, result string, isError bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls = append(h.calls, toolCallRecord{
		callID:    callID,
		tool:      tool,
		arguments: arguments,
		result:    result,
		isError:   isError,
	})
}

// snapshot returns a copy of the records; the caller can iterate without
// holding the lock.
func (h *toolHistory) snapshot() []toolCallRecord {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]toolCallRecord, len(h.calls))
	copy(out, h.calls)
	return out
}

// grepLineRE matches the `path:line:content` prefix produced by
// `grep -rn` (see executor.searchCode). We only care about the path.
// Paths can contain any character except the leading `./` and colons,
// but grep emits a relative path — we strip a leading "./" when present.
var grepLineRE = regexp.MustCompile(`(?m)^([^\s:][^:\n]*):\d+:`)

// extractFilesFromSearchResult pulls file paths out of a grep-style
// search result. Deduplicates within the single result, but the caller
// does final deduplication across all calls.
func extractFilesFromSearchResult(result string) []string {
	if strings.TrimSpace(result) == "" {
		return nil
	}
	matches := grepLineRE.FindAllStringSubmatch(result, -1)
	seen := make(map[string]struct{}, len(matches))
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		if len(m) < 2 {
			continue
		}
		p := strings.TrimPrefix(m[1], "./")
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if _, ok := seen[p]; ok {
			continue
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	return out
}

// extractPathFromArgs decodes a tool's JSON arguments and returns the
// "path" field if present. Returns "" on any parse error — tool-arg
// shape changes are downstream tool-authoring concerns and a best-effort
// capture is acceptable (we never fail the research task because of a
// parse failure here).
func extractPathFromArgs(arguments string) string {
	if arguments == "" {
		return ""
	}
	var m map[string]interface{}
	if err := json.Unmarshal([]byte(arguments), &m); err != nil {
		return ""
	}
	if p, ok := m["path"].(string); ok {
		return strings.TrimSpace(strings.TrimPrefix(p, "./"))
	}
	return ""
}

// codexNameItem is the minimal shape every codex_* tool result contains
// when it returns "hits" — the `name` field identifies the module or
// pattern. Agent A's actual tool implementations (see
// internal/llm/tools/executor.go) emit one of:
//
//   - codex_find_module   → {"modules": [{"name":...}], "count":..., "query":...}
//   - codex_patterns_for  → {"query":..., "patterns": [{"name":...}], "anti_patterns": [{"name":...}]}
//   - codex_dependents_of → {"module":..., "dependents": [...]}   // strings, not {name:...}
//   - codex_similar_code  → {"results": [{"file":..., "function":...}], "count":...}
//
// extractCodexNames handles the name-bearing variants. Callers that need
// string-list fields (dependents, dependencies) or file-path fields use
// extractCodexStringList / extractCodexFiles instead.
type codexNameItem struct {
	Name string `json:"name"`
}

// extractCodexModuleNames pulls module names out of a codex_find_module
// result. Returns nil if the shape doesn't match — we never panic on a
// parse error because Agent A's tool may still be evolving.
func extractCodexModuleNames(result string) []string {
	result = strings.TrimSpace(result)
	if result == "" {
		return nil
	}
	var obj struct {
		Modules []codexNameItem `json:"modules"`
	}
	if err := json.Unmarshal([]byte(result), &obj); err == nil && len(obj.Modules) > 0 {
		out := make([]string, 0, len(obj.Modules))
		for _, it := range obj.Modules {
			if strings.TrimSpace(it.Name) != "" {
				out = append(out, it.Name)
			}
		}
		return out
	}
	// Bare-array fallback: some tool implementations may return [{"name":...}].
	return extractNameArray(result)
}

// extractCodexPatternNames pulls pattern names out of a codex_patterns_for
// result. Returns (patterns, antiPatterns). codex_patterns_for bundles
// both — splitting here keeps the caller code tidy.
func extractCodexPatternNames(result string) (patterns []string, antiPatterns []string) {
	result = strings.TrimSpace(result)
	if result == "" {
		return nil, nil
	}
	var obj struct {
		Patterns     []codexNameItem `json:"patterns"`
		AntiPatterns []codexNameItem `json:"anti_patterns"`
	}
	if err := json.Unmarshal([]byte(result), &obj); err != nil {
		// Regex fallback: pull every "name":"..." occurrence so weak LLMs
		// or a tool shape drift doesn't lose all signal.
		return extractNameArray(result), nil
	}
	for _, it := range obj.Patterns {
		if strings.TrimSpace(it.Name) != "" {
			patterns = append(patterns, it.Name)
		}
	}
	for _, it := range obj.AntiPatterns {
		if strings.TrimSpace(it.Name) != "" {
			antiPatterns = append(antiPatterns, it.Name)
		}
	}
	return patterns, antiPatterns
}

// extractCodexSimilarFiles pulls file paths out of a codex_similar_code
// result. The tool returns vector-store hits keyed by "file".
func extractCodexSimilarFiles(result string) []string {
	result = strings.TrimSpace(result)
	if result == "" {
		return nil
	}
	var obj struct {
		Results []struct {
			File string `json:"file"`
		} `json:"results"`
	}
	if err := json.Unmarshal([]byte(result), &obj); err != nil {
		return nil
	}
	out := make([]string, 0, len(obj.Results))
	for _, r := range obj.Results {
		if strings.TrimSpace(r.File) != "" {
			out = append(out, strings.TrimPrefix(r.File, "./"))
		}
	}
	return out
}

// extractCodexDependencyModules pulls a list of module names from
// codex_dependents_of / codex_dependencies_of results. Those tools return
// `{"module": "...", "dependents": [...]}` where dependents is a string
// array — so every name we see counts as an affected module.
func extractCodexDependencyModules(result string) []string {
	result = strings.TrimSpace(result)
	if result == "" {
		return nil
	}
	var obj struct {
		Module       string   `json:"module"`
		Dependents   []string `json:"dependents"`
		Dependencies []string `json:"dependencies"`
	}
	if err := json.Unmarshal([]byte(result), &obj); err != nil {
		return nil
	}
	var out []string
	if obj.Module != "" {
		out = append(out, obj.Module)
	}
	out = append(out, obj.Dependents...)
	out = append(out, obj.Dependencies...)
	return out
}

// extractNameArray is a last-ditch regex fallback that pulls "name":"..."
// occurrences from an arbitrary JSON blob. Used when the structured
// unmarshal above doesn't match — we'd rather capture partial signal than
// lose everything on a tool-shape drift.
func extractNameArray(result string) []string {
	re := regexp.MustCompile(`"name"\s*:\s*"([^"]+)"`)
	matches := re.FindAllStringSubmatch(result, -1)
	if len(matches) == 0 {
		return nil
	}
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		if len(m) >= 2 && m[1] != "" {
			out = append(out, m[1])
		}
	}
	return out
}

// dedupeSort returns a sorted deduplication of the input. Used to produce
// stable, compact structured-field values for the research.results payload.
func dedupeSort(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		s = strings.TrimSpace(s)
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

// structuredFields is the output of deriveStructuredFields: a bundle of the
// six new fields ready to assign onto researchResultMessage. Kept as a
// separate type so the derivation logic is testable without constructing
// a full researchResultMessage.
type structuredFields struct {
	AffectedFiles        []string
	AffectedModules      []string
	AffectedZones        []string
	CrossRepoTouchpoints []string
	RelevantPatterns     []string
	RelevantAntiPatterns []string
}

// deriveStructuredFields walks the tool-call history and extracts the
// structured research payload. DB-backed lookups (zones, repo touchpoints)
// are best-effort: a DB failure is logged but does NOT fail the research
// task, since Evidence is still valid even when the structured sidecar is
// partial.
//
// productID may be uuid.Nil when the work item has no product_id — in that
// case we skip code_zones lookup (it's scoped by repository, but zones
// require the repository set for this product to exist).
func deriveStructuredFields(
	ctx context.Context,
	pool *pgxpool.Pool,
	productID uuid.UUID,
	calls []toolCallRecord,
) structuredFields {
	var files, modules, patterns, antiPatterns []string

	for _, c := range calls {
		switch c.tool {
		case "read_file":
			// read_file errors still produce a path argument worth
			// recording — the agent TRIED to read something in this area
			// of the codebase even if the file didn't exist.
			if p := extractPathFromArgs(c.arguments); p != "" {
				files = append(files, p)
			}
		case "search_code":
			if c.isError {
				continue
			}
			files = append(files, extractFilesFromSearchResult(c.result)...)
		case "codex_find_module":
			if c.isError {
				continue
			}
			modules = append(modules, extractCodexModuleNames(c.result)...)
		case "codex_dependents_of", "codex_dependencies_of":
			if c.isError {
				continue
			}
			modules = append(modules, extractCodexDependencyModules(c.result)...)
		case "codex_similar_code":
			if c.isError {
				continue
			}
			files = append(files, extractCodexSimilarFiles(c.result)...)
		case "codex_patterns_for":
			if c.isError {
				continue
			}
			p, ap := extractCodexPatternNames(c.result)
			patterns = append(patterns, p...)
			antiPatterns = append(antiPatterns, ap...)
		}
	}

	files = dedupeSort(files)
	modules = dedupeSort(modules)
	patterns = dedupeSort(patterns)
	antiPatterns = dedupeSort(antiPatterns)

	var zones []string
	var touchpoints []string
	if pool != nil {
		zones = lookupZonesForModules(ctx, pool, productID, modules, files)
		touchpoints = lookupRepoTouchpoints(ctx, pool, productID, files)
	}

	return structuredFields{
		AffectedFiles:        files,
		AffectedModules:      modules,
		AffectedZones:        zones,
		CrossRepoTouchpoints: touchpoints,
		RelevantPatterns:     patterns,
		RelevantAntiPatterns: antiPatterns,
	}
}

// rootedZone is a code zone with the clone path of its repository ("" for
// a legacy zone with no repository).
type rootedZone struct {
	zone zones.Zone
	root string
}

// lookupZonesForModules maps affected modules / files to code_zones.name
// (codex zones, migration 063): the zones of the product's repositories
// (and legacy zones with no repository) whose patterns match an affected
// file, or whose patterns or module paths match an affected module
// (zonesForAffected). Without a product there is no zone to report (no
// cross-product lookup).
func lookupZonesForModules(
	ctx context.Context,
	pool *pgxpool.Pool,
	productID uuid.UUID,
	modules []string,
	files []string,
) []string {
	if (len(modules) == 0 && len(files) == 0) || productID == uuid.Nil {
		return nil
	}
	rows, err := pool.Query(ctx, `
		SELECT z.name, z.patterns, z.module_paths, COALESCE(r.clone_path, '')
		  FROM code_zones z
		  LEFT JOIN repositories r ON r.id = z.repository_id
		 WHERE z.repository_id IS NULL OR r.product_id = $1
	`, productID)
	if err != nil {
		log.Printf("research-agent: code_zones lookup failed: %v", err)
		return nil
	}
	defer rows.Close()

	var zs []rootedZone
	for rows.Next() {
		var rz rootedZone
		if scanErr := rows.Scan(&rz.zone.Name, &rz.zone.Patterns, &rz.zone.ModulePaths, &rz.root); scanErr != nil {
			log.Printf("research-agent: code_zones scan failed: %v", scanErr)
			continue
		}
		zs = append(zs, rz)
	}
	if rErr := rows.Err(); rErr != nil {
		log.Printf("research-agent: code_zones iterate failed: %v", rErr)
	}
	return zonesForAffected(zs, modules, files)
}

// zonesForAffected is the pure matching step of lookupZonesForModules. A
// file is matched relative to the zone's repository (zones.RepoRelative):
// an absolute path under the repository's clone path is made relative to
// it; any other absolute path — another repository's file, or any absolute
// path for a legacy zone with no repository — never matches; a relative
// path is matched as is. A module matches when the zone's
// patterns or module paths do (zones.Zone.MatchesModule). Sorted, distinct.
func zonesForAffected(zs []rootedZone, modules, files []string) []string {
	var out []string
	for _, rz := range zs {
		hit := false
		for _, m := range modules {
			if rz.zone.MatchesModule(m) {
				hit = true
				break
			}
		}
		for _, f := range files {
			if hit {
				break
			}
			rel, ok := zones.RepoRelative(f, rz.root)
			if ok && rz.zone.Matches(rel) {
				hit = true
			}
		}
		if hit {
			out = append(out, rz.zone.Name)
		}
	}
	return dedupeSort(out)
}

// lookupRepoTouchpoints returns the set of repository names whose
// clone_path is a prefix of at least one affected file. This is the
// signal research-coordinator aggregates across tasks into the brief's
// cross_repo_touchpoints — downstream planners can then know "this work
// spans repos X and Y" without re-inferring it from prose.
//
// We load all repositories for productID once, then match in-memory
// (there are typically 1–5 repos per product; a per-file query would
// be wasteful).
func lookupRepoTouchpoints(
	ctx context.Context,
	pool *pgxpool.Pool,
	productID uuid.UUID,
	files []string,
) []string {
	if len(files) == 0 || productID == uuid.Nil {
		return nil
	}
	rows, err := pool.Query(ctx, `
		SELECT name, clone_path
		FROM repositories
		WHERE product_id = $1 AND enabled = true
	`, productID)
	if err != nil {
		log.Printf("research-agent: repositories lookup failed: %v", err)
		return nil
	}
	defer rows.Close()

	type repoEntry struct {
		name      string
		clonePath string
	}
	var repos []repoEntry
	for rows.Next() {
		var r repoEntry
		if scanErr := rows.Scan(&r.name, &r.clonePath); scanErr != nil {
			log.Printf("research-agent: repositories scan failed: %v", scanErr)
			continue
		}
		if r.clonePath != "" {
			repos = append(repos, r)
		}
	}
	if rErr := rows.Err(); rErr != nil {
		log.Printf("research-agent: repositories iterate failed: %v", rErr)
	}
	if len(repos) == 0 {
		return nil
	}

	seen := make(map[string]struct{})
	for _, f := range files {
		abs := filepath.Clean(f)
		// Files coming from read_file / search_code are typically repo-
		// relative (no leading /). Match them against repo name via the
		// ambient executor rooted at clone_path — in practice the file
		// path alone doesn't tell us which repo. So: if the file is
		// absolute, prefix-match against clone_path; if relative, we
		// can only match when there is exactly ONE repo configured
		// (unambiguous) OR the path happens to start with a repo name
		// (e.g. "frontend/src/..." when frontend is a clone_path suffix).
		matched := false
		if filepath.IsAbs(abs) {
			for _, r := range repos {
				rp := filepath.Clean(r.clonePath)
				if rp != "" && (abs == rp || strings.HasPrefix(abs, rp+string(filepath.Separator)) || strings.HasPrefix(abs, rp+"/")) {
					seen[r.name] = struct{}{}
					matched = true
				}
			}
		}
		if !matched && len(repos) == 1 {
			// Single-repo product — every file belongs to it.
			seen[repos[0].name] = struct{}{}
		}
	}

	out := make([]string, 0, len(seen))
	for n := range seen {
		out = append(out, n)
	}
	return dedupeSort(out)
}
