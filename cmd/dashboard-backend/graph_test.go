package main

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/models"
	"github.com/google/uuid"
)

func mkMod(name string, deps []string, risk string) *models.CodexModule {
	return &models.CodexModule{
		ID:              uuid.New(),
		Name:            name,
		Purpose:         name + " purpose",
		Dependencies:    deps,
		Dependents:      nil,
		RiskLevel:       risk,
		PublicInterface: json.RawMessage(`{}`),
	}
}

// TestBuildGraph_EmptyInput: empty input → empty graph, empty_reason set.
func TestBuildGraph_EmptyInput(t *testing.T) {
	resp := BuildGraph(nil)
	if len(resp.Nodes) != 0 {
		t.Fatalf("want 0 nodes, got %d", len(resp.Nodes))
	}
	if len(resp.Edges) != 0 {
		t.Fatalf("want 0 edges, got %d", len(resp.Edges))
	}
	if resp.Meta.EmptyReason != "no_modules_indexed" {
		t.Fatalf("want empty_reason=no_modules_indexed, got %q", resp.Meta.EmptyReason)
	}
	if resp.Meta.Truncated {
		t.Fatal("empty graph should not be truncated")
	}
}

// TestBuildGraph_SingleNodeNoDeps: 1 module, no deps → 1 node, 0 edges.
func TestBuildGraph_SingleNodeNoDeps(t *testing.T) {
	mods := []*models.CodexModule{mkMod("alpha", nil, "low")}
	resp := BuildGraph(mods)
	if len(resp.Nodes) != 1 {
		t.Fatalf("want 1 node, got %d", len(resp.Nodes))
	}
	if resp.Nodes[0].Name != "alpha" {
		t.Fatalf("want name=alpha, got %q", resp.Nodes[0].Name)
	}
	if resp.Nodes[0].RiskLevel != "low" {
		t.Fatalf("want risk=low, got %q", resp.Nodes[0].RiskLevel)
	}
	if len(resp.Edges) != 0 {
		t.Fatalf("want 0 edges, got %d", len(resp.Edges))
	}
	if resp.Meta.Truncated {
		t.Fatal("should not be truncated")
	}
	if resp.Meta.TotalModules != 1 {
		t.Fatalf("want total_modules=1, got %d", resp.Meta.TotalModules)
	}
	if resp.Meta.TotalExternal != 0 {
		t.Fatalf("want total_external=0, got %d", resp.Meta.TotalExternal)
	}
}

// TestBuildGraph_InternalEdge: two modules, A depends on B → 2 nodes, 1 edge.
func TestBuildGraph_InternalEdge(t *testing.T) {
	a := mkMod("alpha", []string{"beta"}, "high")
	b := mkMod("beta", nil, "low")
	resp := BuildGraph([]*models.CodexModule{a, b})

	if len(resp.Nodes) != 2 {
		t.Fatalf("want 2 nodes, got %d", len(resp.Nodes))
	}
	if len(resp.Edges) != 1 {
		t.Fatalf("want 1 edge, got %d", len(resp.Edges))
	}
	e := resp.Edges[0]
	if !e.Resolved {
		t.Fatalf("edge should be resolved, got %+v", e)
	}
	if e.Source != a.ID.String() {
		t.Fatalf("edge source want %s, got %s", a.ID.String(), e.Source)
	}
	if e.Target != b.ID.String() {
		t.Fatalf("edge target want %s, got %s", b.ID.String(), e.Target)
	}
	if e.Kind != "depends_on" {
		t.Fatalf("edge kind want depends_on, got %q", e.Kind)
	}
	// Confirm no external nodes were synthesized.
	for _, n := range resp.Nodes {
		if n.External {
			t.Fatalf("no external node expected but got %+v", n)
		}
	}
}

// TestBuildGraph_ExternalDep: module with a dependency that doesn't resolve to
// another in-product module → real node + synthesized external node, 1 edge.
func TestBuildGraph_ExternalDep(t *testing.T) {
	a := mkMod("alpha", []string{"pgx"}, "medium")
	resp := BuildGraph([]*models.CodexModule{a})

	if len(resp.Nodes) != 2 {
		t.Fatalf("want 2 nodes (alpha + external pgx), got %d", len(resp.Nodes))
	}
	var extCount, realCount int
	for _, n := range resp.Nodes {
		if n.External {
			extCount++
			if n.ID != "external:pgx" {
				t.Fatalf("want external id=external:pgx, got %q", n.ID)
			}
			if n.Name != "pgx" {
				t.Fatalf("want external name=pgx, got %q", n.Name)
			}
		} else {
			realCount++
		}
	}
	if extCount != 1 {
		t.Fatalf("want 1 external node, got %d", extCount)
	}
	if realCount != 1 {
		t.Fatalf("want 1 real node, got %d", realCount)
	}
	if len(resp.Edges) != 1 {
		t.Fatalf("want 1 edge, got %d", len(resp.Edges))
	}
	if resp.Edges[0].Resolved {
		t.Fatalf("edge to external should be unresolved: %+v", resp.Edges[0])
	}
	if resp.Meta.TotalExternal != 1 {
		t.Fatalf("want total_external=1, got %d", resp.Meta.TotalExternal)
	}
}

// TestBuildGraph_DuplicateExternalsDeduped: two modules each depending on the
// same external name → only 1 external node, but 2 edges (one per source).
func TestBuildGraph_DuplicateExternalsDeduped(t *testing.T) {
	a := mkMod("alpha", []string{"pgx"}, "low")
	b := mkMod("beta", []string{"pgx"}, "low")
	resp := BuildGraph([]*models.CodexModule{a, b})

	// Expected: 2 real + 1 deduped external = 3
	if len(resp.Nodes) != 3 {
		t.Fatalf("want 3 nodes (2 real + 1 external), got %d", len(resp.Nodes))
	}
	var extCount int
	for _, n := range resp.Nodes {
		if n.External {
			extCount++
		}
	}
	if extCount != 1 {
		t.Fatalf("want 1 external node (deduped), got %d", extCount)
	}
	if len(resp.Edges) != 2 {
		t.Fatalf("want 2 edges, got %d", len(resp.Edges))
	}
	// Both edges must be unresolved and target the same external id.
	for _, e := range resp.Edges {
		if e.Resolved {
			t.Fatalf("edge should be unresolved: %+v", e)
		}
		if e.Target != "external:pgx" {
			t.Fatalf("edge target want external:pgx, got %q", e.Target)
		}
	}
}

// TestBuildGraph_TruncationAt500: > 500 modules → truncated, nodes capped at 500.
func TestBuildGraph_TruncationAt500(t *testing.T) {
	var mods []*models.CodexModule
	// 600 modules, each depending on the next; degree matters for ordering.
	for i := 0; i < 600; i++ {
		name := fmt.Sprintf("m%04d", i)
		var deps []string
		if i+1 < 600 {
			deps = []string{fmt.Sprintf("m%04d", i+1)}
		}
		mods = append(mods, mkMod(name, deps, "low"))
	}
	resp := BuildGraph(mods)
	if !resp.Meta.Truncated {
		t.Fatal("want truncated=true")
	}
	if len(resp.Nodes) != GraphMaxNodes {
		t.Fatalf("want %d nodes after truncation, got %d", GraphMaxNodes, len(resp.Nodes))
	}
	if resp.Meta.NodeCount != GraphMaxNodes {
		t.Fatalf("meta.NodeCount want %d, got %d", GraphMaxNodes, resp.Meta.NodeCount)
	}
	if resp.Meta.TotalModules != 600 {
		t.Fatalf("want total_modules=600, got %d", resp.Meta.TotalModules)
	}
	// Every remaining edge must point to a kept node.
	ids := make(map[string]bool, len(resp.Nodes))
	for _, n := range resp.Nodes {
		ids[n.ID] = true
	}
	for _, e := range resp.Edges {
		if !ids[e.Source] || !ids[e.Target] {
			t.Fatalf("edge refers to dropped node: %+v", e)
		}
	}
}

// --- Additional coverage from the plan ---

func TestBuildGraph_EmptyDependencyStringIgnored(t *testing.T) {
	a := mkMod("alpha", []string{"", "  ", "beta"}, "low")
	b := mkMod("beta", nil, "low")
	resp := BuildGraph([]*models.CodexModule{a, b})
	if len(resp.Edges) != 1 {
		t.Fatalf("want 1 edge (blank deps skipped), got %d", len(resp.Edges))
	}
}

func TestBuildGraph_RiskLevelDefaultsToUnknown(t *testing.T) {
	a := mkMod("alpha", nil, "")
	resp := BuildGraph([]*models.CodexModule{a})
	if resp.Nodes[0].RiskLevel != "unknown" {
		t.Fatalf("want risk=unknown, got %q", resp.Nodes[0].RiskLevel)
	}
}

func TestBuildGraph_NodesSortedByName(t *testing.T) {
	c := mkMod("charlie", nil, "low")
	a := mkMod("alpha", nil, "low")
	b := mkMod("bravo", nil, "low")
	resp := BuildGraph([]*models.CodexModule{c, a, b})
	if resp.Nodes[0].Name != "alpha" || resp.Nodes[1].Name != "bravo" || resp.Nodes[2].Name != "charlie" {
		t.Fatalf("nodes not sorted: %+v", resp.Nodes)
	}
}

func TestSummarizePublicInterface_CapsAt200(t *testing.T) {
	long := ""
	for i := 0; i < 300; i++ {
		long += "a"
	}
	raw := json.RawMessage(fmt.Sprintf(`{"doc":%q}`, long))
	out := summarizePublicInterface(raw)
	if len([]rune(out)) > 201 { // 200 + one ellipsis rune
		t.Fatalf("want <= 201 runes, got %d", len([]rune(out)))
	}
}
