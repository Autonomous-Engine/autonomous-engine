package codex

import (
	"context"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/models"
	"github.com/google/uuid"
)

// UpsertModuleForRepoBranchChanged reports whether the module row is new or
// its structure changed; a rewrite with only a new purpose (an LLM summary)
// or new dependents (derived from other modules) is not a change.
func TestUpsertModuleForRepoBranchChanged(t *testing.T) {
	pool := integrationPool(t)
	cx := New(pool)
	ctx := context.Background()
	pid := newProduct(t, pool, "main")
	var rid uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT id FROM repositories WHERE product_id = $1`, pid).Scan(&rid); err != nil {
		t.Fatal(err)
	}
	mod := func() *models.CodexModule {
		return &models.CodexModule{
			Name: "api", Purpose: "p1", PublicInterface: []byte(`{"functions":["A"]}`),
			Dependencies: []string{"db"}, Dependents: []string{"web"}, InternalPatterns: "go",
			OwnerDomain: "core", RiskLevel: "low", TestCoverageNotes: "none",
		}
	}
	upsert := func(m *models.CodexModule, branch string) bool {
		t.Helper()
		changed, err := cx.UpsertModuleForRepoBranchChanged(ctx, pid, rid, branch, m)
		if err != nil {
			t.Fatal(err)
		}
		return changed
	}
	if !upsert(mod(), "main") {
		t.Error("a new module is a change")
	}
	same := mod()
	same.Purpose, same.Dependents = "a different summary", []string{"web", "cli"}
	if upsert(same, "main") {
		t.Error("only purpose and dependents differ: not a change")
	}
	var purpose string
	_ = pool.QueryRow(ctx, `SELECT purpose FROM codex_modules WHERE product_id = $1 AND branch = 'main' AND name = 'api'`, pid).Scan(&purpose)
	if purpose != "a different summary" {
		t.Errorf("the row was not rewritten: purpose %q", purpose)
	}
	iface := mod()
	iface.PublicInterface = []byte(`{"functions":["A","B"]}`)
	if !upsert(iface, "main") {
		t.Error("a public interface change is a change")
	}
	deps := mod()
	deps.PublicInterface = iface.PublicInterface
	deps.Dependencies = []string{"db", "cache"}
	if !upsert(deps, "main") {
		t.Error("a dependency change is a change")
	}
	if !upsert(mod(), "develop") {
		t.Error("the same module on another branch is a new row")
	}
}
