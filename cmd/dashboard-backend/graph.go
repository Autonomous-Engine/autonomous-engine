package main

import (
	"encoding/json"
	"sort"
	"strings"

	"github.com/autonomousengine/autonomous-engine/internal/models"
)

// GraphMaxNodes is the hard cap on nodes in a single response. Above this
// threshold we sort by in-degree + out-degree (connectedness) and keep the
// top N; meta.truncated flips to true.
const GraphMaxNodes = 500

// GraphNode is one vertex in the response.
type GraphNode struct {
	ID                     string   `json:"id"`
	Name                   string   `json:"name"`
	Purpose                string   `json:"purpose"`
	RiskLevel              string   `json:"risk_level"`
	OwnerDomain            string   `json:"owner_domain"`
	TestCoverageNotes      string   `json:"test_coverage_notes"`
	PublicInterfaceSummary string   `json:"public_interface_summary"`
	Dependencies           []string `json:"dependencies"`
	Dependents             []string `json:"dependents"`
	DependenciesCount      int      `json:"dependencies_count"`
	DependentsCount        int      `json:"dependents_count"`
	RepositoryID           string   `json:"repository_id,omitempty"`
	External               bool     `json:"external"`
}

// GraphEdge is one directed edge (source depends_on target).
type GraphEdge struct {
	ID       string `json:"id"`
	Source   string `json:"source"`
	Target   string `json:"target"`
	Kind     string `json:"kind"`
	Resolved bool   `json:"resolved"`
}

// GraphMeta carries counts and banners.
type GraphMeta struct {
	NodeCount     int    `json:"node_count"`
	EdgeCount     int    `json:"edge_count"`
	TotalModules  int    `json:"total_modules"`
	TotalExternal int    `json:"total_external"`
	Truncated     bool   `json:"truncated"`
	EmptyReason   string `json:"empty_reason,omitempty"`
}

// GraphResponse is the full JSON body returned by /api/codex/graph.
type GraphResponse struct {
	ProductID   string      `json:"product_id"`
	ProductName string      `json:"product_name"`
	GeneratedAt string      `json:"generated_at"`
	Meta        GraphMeta   `json:"meta"`
	Nodes       []GraphNode `json:"nodes"`
	Edges       []GraphEdge `json:"edges"`
}

// summarizePublicInterface flattens a public_interface JSONB blob into a
// single line, capped at 200 chars. Accepts a JSON object, array, string,
// or null; anything else falls through to a safe empty string.
func summarizePublicInterface(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var decoded interface{}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return ""
	}
	var b strings.Builder
	flatten(&b, decoded)
	s := strings.TrimSpace(b.String())
	runes := []rune(s)
	if len(runes) > 200 {
		return string(runes[:200]) + "…"
	}
	return s
}

func flatten(b *strings.Builder, v interface{}) {
	switch t := v.(type) {
	case string:
		b.WriteString(t)
		b.WriteString(" ")
	case []interface{}:
		for _, x := range t {
			flatten(b, x)
		}
	case map[string]interface{}:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			b.WriteString(k)
			b.WriteString(": ")
			flatten(b, t[k])
		}
	}
}

// BuildGraph is the pure function under test: given a slice of modules for
// one product, it returns a populated GraphResponse (minus product-level
// metadata like ProductID/Name/GeneratedAt — the handler fills those).
//
// Rules:
//   - one node per module
//   - for each module M and each name N in M.Dependencies:
//     - if N resolves to another real module within this product → emit a
//       resolved edge
//     - otherwise → emit an edge to a synthetic node with id "external:<N>"
//       and mark it External=true; dedupe synthetic nodes across modules
//   - if the result exceeds GraphMaxNodes, keep the top-N by in+out degree;
//     edges whose source or target was dropped are also dropped; Truncated
//     flips to true
//   - nodes sorted by name for stable render order
func BuildGraph(modules []*models.CodexModule) GraphResponse {
	if len(modules) == 0 {
		return GraphResponse{
			Nodes: []GraphNode{},
			Edges: []GraphEdge{},
			Meta:  GraphMeta{EmptyReason: "no_modules_indexed"},
		}
	}

	// Index modules by name for O(N) resolution.
	byName := make(map[string]*models.CodexModule, len(modules))
	for _, m := range modules {
		byName[m.Name] = m
	}

	// Build real nodes.
	nodes := make([]GraphNode, 0, len(modules))
	for _, m := range modules {
		n := GraphNode{
			ID:                     m.ID.String(),
			Name:                   m.Name,
			Purpose:                m.Purpose,
			RiskLevel:              strings.ToLower(strings.TrimSpace(m.RiskLevel)),
			OwnerDomain:            m.OwnerDomain,
			TestCoverageNotes:      m.TestCoverageNotes,
			PublicInterfaceSummary: summarizePublicInterface(m.PublicInterface),
			Dependencies:           append([]string{}, m.Dependencies...),
			Dependents:             append([]string{}, m.Dependents...),
			DependenciesCount:      len(m.Dependencies),
			DependentsCount:        len(m.Dependents),
		}
		if n.RiskLevel == "" {
			n.RiskLevel = "unknown"
		}
		if m.RepositoryID != nil {
			n.RepositoryID = m.RepositoryID.String()
		}
		nodes = append(nodes, n)
	}

	// Build edges + synthetic external nodes.
	externals := make(map[string]bool)
	var externalCount int
	var edges []GraphEdge
	for _, m := range modules {
		srcID := m.ID.String()
		for _, dep := range m.Dependencies {
			dep = strings.TrimSpace(dep)
			if dep == "" {
				continue
			}
			if target, ok := byName[dep]; ok {
				edges = append(edges, GraphEdge{
					ID:       "e:" + srcID + "->" + target.ID.String(),
					Source:   srcID,
					Target:   target.ID.String(),
					Kind:     "depends_on",
					Resolved: true,
				})
				continue
			}
			extID := "external:" + dep
			if !externals[extID] {
				externals[extID] = true
				externalCount++
				nodes = append(nodes, GraphNode{
					ID:        extID,
					Name:      dep,
					Purpose:   "External dependency — not indexed in this product.",
					RiskLevel: "unknown",
					External:  true,
				})
			}
			edges = append(edges, GraphEdge{
				ID:       "e:" + srcID + "->" + extID,
				Source:   srcID,
				Target:   extID,
				Kind:     "depends_on",
				Resolved: false,
			})
		}
	}

	// Stable order.
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].Name < nodes[j].Name })

	meta := GraphMeta{
		TotalModules:  len(modules),
		TotalExternal: externalCount,
	}

	// Truncate if over the cap.
	if len(nodes) > GraphMaxNodes {
		degree := make(map[string]int, len(nodes))
		for _, e := range edges {
			degree[e.Source]++
			degree[e.Target]++
		}
		sort.SliceStable(nodes, func(i, j int) bool {
			return degree[nodes[i].ID] > degree[nodes[j].ID]
		})
		nodes = nodes[:GraphMaxNodes]
		keep := make(map[string]bool, len(nodes))
		for _, n := range nodes {
			keep[n.ID] = true
		}
		filtered := edges[:0]
		for _, e := range edges {
			if keep[e.Source] && keep[e.Target] {
				filtered = append(filtered, e)
			}
		}
		edges = filtered
		// Re-sort alphabetically for render stability.
		sort.Slice(nodes, func(i, j int) bool { return nodes[i].Name < nodes[j].Name })
		meta.Truncated = true
	}

	if edges == nil {
		edges = []GraphEdge{}
	}

	meta.NodeCount = len(nodes)
	meta.EdgeCount = len(edges)

	return GraphResponse{
		Nodes: nodes,
		Edges: edges,
		Meta:  meta,
	}
}
