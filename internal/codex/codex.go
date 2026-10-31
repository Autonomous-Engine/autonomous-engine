package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/metrics"
	"github.com/autonomousengine/autonomous-engine/internal/models"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// emptyContextTotal counts every BuildContextForIntentBranch call whose
// payload has no codex module. Patterns and anti-patterns are not
// considered: a payload with patterns but no module still increments it.
// Zero modules means the downstream LLM call runs with no grounding — a
// signal worth alerting on. The pipeline stages refuse such work before
// building the context (guards.CheckCodexGrounded), so an increment comes
// from a caller without that gate.
// The project's in-tree metrics package is label-less; per-product
// attribution is still logged alongside the increment for ad-hoc
// drill-down via `docker logs | grep`.
var emptyContextTotal = metrics.NewCounter(
	"ae_codex_empty_context_total",
	"Total BuildContextForIntentBranch calls whose payload has no codex module",
)

// Codex provides access to the project's Codex knowledge base stored in Postgres.
type Codex struct {
	pool *pgxpool.Pool
}

// Pool returns the underlying connection pool so callers that need direct
// SQL access (e.g. materializeCodex, codex_contract_edges) can query
// commit-linked tables without routing every new query through a Codex method.
func (c *Codex) Pool() *pgxpool.Pool {
	return c.pool
}

// New creates a new Codex backed by the provided connection pool.
func New(pool *pgxpool.Pool) *Codex {
	return &Codex{pool: pool}
}

// ResolveDefaultBranch reads the default branch for the given product from
// the repositories table. The value is populated at onboarding time from
// `git ls-remote --symref <url> HEAD` (the actual remote HEAD — no guessing)
// and is the single source of truth for branch-scoped codex lookups when a
// caller has no work-item context.
//
// Returns an error — NOT a silent "main" default — when no enabled repo
// exists for the product or the stored base_branch is empty. A bogus "main"
// fallback silently broke codex lookups for every repo whose default branch
// is master/develop/trunk, so the policy is: surface the missing config
// instead of papering over it.
//
// When a product has multiple repositories the first one ordered by
// created_at is used. Today codex rows carry a single branch per product;
// per-repo branches are a future concern tracked separately.
func ResolveDefaultBranch(ctx context.Context, db *pgxpool.Pool, productID uuid.UUID) (string, error) {
	return resolveDefaultBranch(ctx, db, productID)
}

// ErrNoDefaultBranch is wrapped by ResolveDefaultBranch when the product has
// no enabled repository with a base branch (as opposed to a read error).
var ErrNoDefaultBranch = errors.New("no default branch configured")

// resolveDefault is a method-style convenience that routes through
// ResolveDefaultBranch using the Codex's own pool. Internal callers that
// need the product's default branch without plumbing in a pool use this.
func (c *Codex) resolveDefault(ctx context.Context, productID uuid.UUID) (string, error) {
	return ResolveDefaultBranch(ctx, c.pool, productID)
}

// RepoEntry identifies an enabled repository for a product so callers
// (e.g. codex_read_file / codex_grep) can resolve a filesystem path on
// the shared repo-cache mount without pulling in the full products
// package dependency.
type RepoEntry struct {
	Name      string
	ClonePath string
}

// ListEnabledRepos returns the enabled repositories for the product,
// ordered by created_at (the same ordering ResolveDefaultBranch uses to
// pick the default). Callers pass the name (optional) to scope the
// read/grep tools to one repo; when no name is given the first entry
// is the sensible default.
func (c *Codex) ListEnabledRepos(ctx context.Context, productID uuid.UUID) ([]RepoEntry, error) {
	rows, err := c.pool.Query(ctx, `
		SELECT name, clone_path
		FROM repositories
		WHERE product_id = $1 AND enabled = true
		ORDER BY created_at
	`, productID)
	if err != nil {
		return nil, fmt.Errorf("list enabled repos for product %s: %w", productID, err)
	}
	defer rows.Close()

	out := []RepoEntry{}
	for rows.Next() {
		var r RepoEntry
		if err := rows.Scan(&r.Name, &r.ClonePath); err != nil {
			return nil, fmt.Errorf("scan repo row: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ─── Sections ────────────────────────────────────────────────────────────────

// GetSection retrieves a codex section by its unique name.
func (c *Codex) GetSection(ctx context.Context, name string) (*models.CodexSection, error) {
	row := c.pool.QueryRow(ctx, `
		SELECT id, section_type, name, content, version, updated_at
		FROM codex_sections
		WHERE name = $1
	`, name)

	s := &models.CodexSection{}
	err := row.Scan(&s.ID, &s.SectionType, &s.Name, &s.Content, &s.Version, &s.UpdatedAt)
	if err != nil {
		if err == pgx.ErrNoRows {
			return nil, fmt.Errorf("codex section %q not found", name)
		}
		return nil, fmt.Errorf("get section: %w", err)
	}
	return s, nil
}

// GetSectionsByType returns all codex sections of the given type.
func (c *Codex) GetSectionsByType(ctx context.Context, sType models.CodexSectionType) ([]*models.CodexSection, error) {
	rows, err := c.pool.Query(ctx, `
		SELECT id, section_type, name, content, version, updated_at
		FROM codex_sections
		WHERE section_type = $1
		ORDER BY name
	`, sType)
	if err != nil {
		return nil, fmt.Errorf("get sections by type: %w", err)
	}
	defer rows.Close()

	return scanSections(rows)
}

// UpsertSection inserts or updates a codex section identified by its name.
// On conflict, the content, section_type, product_id, repository_id, and
// updated_at are updated and the version is incremented.
//
// Branch must be populated on section.Branch by the caller — callers that
// write product-scoped rows know their branch from git (knowledge-indexer
// resolves it via gitCurrentBranch; onboarding resolves via
// ResolveDefaultBranch). Empty branch on a product-scoped write is a caller
// bug and returns an error rather than silently landing on "main".
func (c *Codex) UpsertSection(ctx context.Context, section *models.CodexSection) error {
	if section.ID == uuid.Nil {
		section.ID = uuid.New()
	}
	section.UpdatedAt = time.Now().UTC()
	if section.ProductID != nil && section.Branch == "" {
		return fmt.Errorf("upsert section: product-scoped section %q has empty branch; caller must resolve branch before write", section.Name)
	}
	branch := section.Branch

	// Select the correct partial unique index based on whether we're
	// writing a product-scoped row or a legacy/global row. This mirrors
	// the split on codex_modules and codex_patterns.
	if section.ProductID != nil {
		_, err := c.pool.Exec(ctx, `
			INSERT INTO codex_sections (id, section_type, name, content, version, updated_at, product_id, repository_id, branch)
			VALUES ($1, $2, $3, $4, 1, $5, $6, $7, $8)
			ON CONFLICT (product_id, branch, name) WHERE product_id IS NOT NULL DO UPDATE
			SET section_type  = EXCLUDED.section_type,
			    content       = EXCLUDED.content,
			    version       = codex_sections.version + 1,
			    updated_at    = EXCLUDED.updated_at,
			    product_id    = EXCLUDED.product_id,
			    repository_id = EXCLUDED.repository_id
		`,
			section.ID,
			section.SectionType,
			section.Name,
			section.Content,
			section.UpdatedAt,
			section.ProductID,
			section.RepositoryID,
			branch,
		)
		if err != nil {
			return fmt.Errorf("upsert section (scoped): %w", err)
		}
		return nil
	}

	// Legacy / global (product_id IS NULL) path. The (name) unique index
	// from migration 020 stays in place; branch still gets recorded on the
	// row so a future backfill can promote legacy rows into the
	// product-scoped partition with their branch preserved.
	_, err := c.pool.Exec(ctx, `
		INSERT INTO codex_sections (id, section_type, name, content, version, updated_at, branch)
		VALUES ($1, $2, $3, $4, 1, $5, $6)
		ON CONFLICT (name) WHERE product_id IS NULL DO UPDATE
		SET section_type = EXCLUDED.section_type,
		    content      = EXCLUDED.content,
		    version      = codex_sections.version + 1,
		    updated_at   = EXCLUDED.updated_at,
		    branch       = EXCLUDED.branch
	`,
		section.ID,
		section.SectionType,
		section.Name,
		section.Content,
		section.UpdatedAt,
		branch,
	)
	if err != nil {
		return fmt.Errorf("upsert section: %w", err)
	}
	return nil
}

// ─── Patterns ────────────────────────────────────────────────────────────────

// GetPattern retrieves a codex pattern by name.
func (c *Codex) GetPattern(ctx context.Context, name string) (*models.CodexPattern, error) {
	row := c.pool.QueryRow(ctx, `
		SELECT id, name, category, when_to_use, reference_files, rationale, key_rules, common_mistakes
		FROM codex_patterns
		WHERE name = $1
	`, name)

	p := &models.CodexPattern{}
	err := row.Scan(
		&p.ID, &p.Name, &p.Category, &p.WhenToUse,
		&p.ReferenceFiles, &p.Rationale, &p.KeyRules, &p.CommonMistakes,
	)
	if err != nil {
		if err == pgx.ErrNoRows {
			return nil, fmt.Errorf("codex pattern %q not found", name)
		}
		return nil, fmt.Errorf("get pattern: %w", err)
	}
	return p, nil
}

// GetPatternsByCategory returns all patterns belonging to the given category.
func (c *Codex) GetPatternsByCategory(ctx context.Context, category string) ([]*models.CodexPattern, error) {
	rows, err := c.pool.Query(ctx, `
		SELECT id, name, category, when_to_use, reference_files, rationale, key_rules, common_mistakes
		FROM codex_patterns
		WHERE category = $1
		ORDER BY name
	`, category)
	if err != nil {
		return nil, fmt.Errorf("get patterns by category: %w", err)
	}
	defer rows.Close()

	return scanPatterns(rows)
}

// UpsertPattern inserts or updates a pattern by name. If pattern.ProductID
// is set, the row is written to the product-scoped partition (matching
// UpsertPatternForRepo semantics); otherwise it writes to the legacy/global
// partition. Callers with a product in scope should prefer
// UpsertPatternForRepo so the repository is also recorded.
//
// Branch must be populated by the caller on product-scoped writes.
// The product-scoped conflict target is (product_id, branch, name), so a
// pattern named "naming_pattern_camel" on one branch and the same pattern
// on another are distinct rows.
func (c *Codex) UpsertPattern(ctx context.Context, pattern *models.CodexPattern) error {
	if pattern.ID == uuid.Nil {
		pattern.ID = uuid.New()
	}
	if pattern.ProductID != nil && pattern.Branch == "" {
		return fmt.Errorf("upsert pattern: product-scoped pattern %q has empty branch; caller must resolve branch before write", pattern.Name)
	}
	branch := pattern.Branch

	if pattern.ProductID != nil {
		_, err := c.pool.Exec(ctx, `
			INSERT INTO codex_patterns
				(id, name, category, when_to_use, reference_files, rationale, key_rules, common_mistakes, product_id, repository_id, branch)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
			ON CONFLICT (product_id, branch, name) WHERE product_id IS NOT NULL DO UPDATE
			SET category        = EXCLUDED.category,
			    when_to_use     = EXCLUDED.when_to_use,
			    reference_files = EXCLUDED.reference_files,
			    rationale       = EXCLUDED.rationale,
			    key_rules       = EXCLUDED.key_rules,
			    common_mistakes = EXCLUDED.common_mistakes,
			    product_id      = EXCLUDED.product_id,
			    repository_id   = EXCLUDED.repository_id
		`,
			pattern.ID,
			pattern.Name,
			pattern.Category,
			pattern.WhenToUse,
			pattern.ReferenceFiles,
			pattern.Rationale,
			pattern.KeyRules,
			pattern.CommonMistakes,
			pattern.ProductID,
			pattern.RepositoryID,
			branch,
		)
		if err != nil {
			return fmt.Errorf("upsert pattern (scoped): %w", err)
		}
		return nil
	}

	// The ON CONFLICT target here matches the partial unique index
	// codex_patterns_name_global_key which covers only rows with
	// product_id IS NULL. This upsert path writes product_id=NULL, so
	// name collisions are constrained to the legacy/global namespace
	// and cannot clobber a product-scoped row in a different partition.
	_, err := c.pool.Exec(ctx, `
		INSERT INTO codex_patterns (id, name, category, when_to_use, reference_files, rationale, key_rules, common_mistakes, branch)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		ON CONFLICT (name) WHERE product_id IS NULL DO UPDATE
		SET category        = EXCLUDED.category,
		    when_to_use     = EXCLUDED.when_to_use,
		    reference_files = EXCLUDED.reference_files,
		    rationale       = EXCLUDED.rationale,
		    key_rules       = EXCLUDED.key_rules,
		    common_mistakes = EXCLUDED.common_mistakes,
		    branch          = EXCLUDED.branch
	`,
		pattern.ID,
		pattern.Name,
		pattern.Category,
		pattern.WhenToUse,
		pattern.ReferenceFiles,
		pattern.Rationale,
		pattern.KeyRules,
		pattern.CommonMistakes,
		branch,
	)
	if err != nil {
		return fmt.Errorf("upsert pattern: %w", err)
	}
	return nil
}

// UpsertPatternForRepo is the product-aware variant. It tags the pattern with
// product_id and repository_id so the dashboard's per-product stats queries
// can find it. Prefer UpsertPatternForRepoBranch when you also know the
// branch; this variant delegates with pattern.Branch (defaulting to "main").
func (c *Codex) UpsertPatternForRepo(ctx context.Context, productID, repoID uuid.UUID, pattern *models.CodexPattern) error {
	return c.UpsertPatternForRepoBranch(ctx, productID, repoID, pattern.Branch, pattern)
}

// UpsertPatternForRepoBranch tags the pattern with product_id, repository_id,
// and branch. Conflict target is (product_id, branch, name) matching the
// partial unique index codex_patterns_product_branch_name_key added in
// migration 021. A pattern named "api" on main and the same name on
// feature/add-x are now distinct rows.
func (c *Codex) UpsertPatternForRepoBranch(ctx context.Context, productID, repoID uuid.UUID, branch string, pattern *models.CodexPattern) error {
	if pattern.ID == uuid.Nil {
		pattern.ID = uuid.New()
	}
	pattern.ProductID = &productID
	pattern.RepositoryID = &repoID
	if branch == "" {
		return fmt.Errorf("upsert pattern for repo/branch: empty branch for product %s pattern %q; caller must resolve branch before write", productID, pattern.Name)
	}
	pattern.Branch = branch

	_, err := c.pool.Exec(ctx, `
		INSERT INTO codex_patterns
			(id, name, category, when_to_use, reference_files, rationale, key_rules, common_mistakes, product_id, repository_id, branch)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		ON CONFLICT (product_id, branch, name) WHERE product_id IS NOT NULL DO UPDATE
		SET category        = EXCLUDED.category,
		    when_to_use     = EXCLUDED.when_to_use,
		    reference_files = EXCLUDED.reference_files,
		    rationale       = EXCLUDED.rationale,
		    key_rules       = EXCLUDED.key_rules,
		    common_mistakes = EXCLUDED.common_mistakes,
		    product_id      = EXCLUDED.product_id,
		    repository_id   = EXCLUDED.repository_id
	`,
		pattern.ID,
		pattern.Name,
		pattern.Category,
		pattern.WhenToUse,
		pattern.ReferenceFiles,
		pattern.Rationale,
		pattern.KeyRules,
		pattern.CommonMistakes,
		productID,
		repoID,
		branch,
	)
	if err != nil {
		return fmt.Errorf("upsert pattern for repo/branch: %w", err)
	}
	return nil
}

// ─── Modules ─────────────────────────────────────────────────────────────────

// GetModule retrieves a codex module by name.
func (c *Codex) GetModule(ctx context.Context, name string) (*models.CodexModule, error) {
	row := c.pool.QueryRow(ctx, `
		SELECT id, name, purpose, public_interface, dependencies, dependents, internal_patterns, owner_domain, risk_level, test_coverage_notes
		FROM codex_modules
		WHERE name = $1
	`, name)

	m := &models.CodexModule{}
	err := row.Scan(
		&m.ID, &m.Name, &m.Purpose, &m.PublicInterface,
		&m.Dependencies, &m.Dependents, &m.InternalPatterns,
		&m.OwnerDomain, &m.RiskLevel, &m.TestCoverageNotes,
	)
	if err != nil {
		if err == pgx.ErrNoRows {
			return nil, fmt.Errorf("codex module %q not found", name)
		}
		return nil, fmt.Errorf("get module: %w", err)
	}
	return m, nil
}

// UpsertModule inserts or updates a module by name. If module.ProductID is
// set, the row is written to the product-scoped partition (matching
// UpsertModuleForRepo semantics); otherwise it writes to the legacy/global
// partition. Callers with both a product and a repository in scope should
// prefer UpsertModuleForRepo so repository_id is also recorded.
//
// Branch must be populated by the caller on product-scoped writes.
// The product-scoped conflict target is (product_id, branch, name).
func (c *Codex) UpsertModule(ctx context.Context, module *models.CodexModule) error {
	if module.ID == uuid.Nil {
		module.ID = uuid.New()
	}
	if module.ProductID != nil && module.Branch == "" {
		return fmt.Errorf("upsert module: product-scoped module %q has empty branch; caller must resolve branch before write", module.Name)
	}
	branch := module.Branch

	if module.ProductID != nil {
		_, err := c.pool.Exec(ctx, `
			INSERT INTO codex_modules
				(id, name, purpose, public_interface, dependencies, dependents, internal_patterns, owner_domain, risk_level, test_coverage_notes, product_id, repository_id, branch)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
			ON CONFLICT (product_id, branch, name) WHERE product_id IS NOT NULL DO UPDATE
			SET purpose             = EXCLUDED.purpose,
			    public_interface    = EXCLUDED.public_interface,
			    dependencies        = EXCLUDED.dependencies,
			    dependents          = EXCLUDED.dependents,
			    internal_patterns   = EXCLUDED.internal_patterns,
			    owner_domain        = EXCLUDED.owner_domain,
			    risk_level          = EXCLUDED.risk_level,
			    test_coverage_notes = EXCLUDED.test_coverage_notes,
			    product_id          = EXCLUDED.product_id,
			    repository_id       = EXCLUDED.repository_id
		`,
			module.ID,
			module.Name,
			module.Purpose,
			module.PublicInterface,
			module.Dependencies,
			module.Dependents,
			module.InternalPatterns,
			module.OwnerDomain,
			module.RiskLevel,
			module.TestCoverageNotes,
			module.ProductID,
			module.RepositoryID,
			branch,
		)
		if err != nil {
			return fmt.Errorf("upsert module (scoped): %w", err)
		}
		return nil
	}

	// The ON CONFLICT target here matches the partial unique index
	// codex_modules_name_global_key which covers only rows with
	// product_id IS NULL. This upsert path writes product_id=NULL, so
	// name collisions are constrained to the legacy/global namespace
	// and cannot clobber a product-scoped row in a different partition.
	_, err := c.pool.Exec(ctx, `
		INSERT INTO codex_modules
			(id, name, purpose, public_interface, dependencies, dependents, internal_patterns, owner_domain, risk_level, test_coverage_notes, branch)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		ON CONFLICT (name) WHERE product_id IS NULL DO UPDATE
		SET purpose            = EXCLUDED.purpose,
		    public_interface   = EXCLUDED.public_interface,
		    dependencies       = EXCLUDED.dependencies,
		    dependents         = EXCLUDED.dependents,
		    internal_patterns  = EXCLUDED.internal_patterns,
		    owner_domain       = EXCLUDED.owner_domain,
		    risk_level         = EXCLUDED.risk_level,
		    test_coverage_notes = EXCLUDED.test_coverage_notes,
		    branch             = EXCLUDED.branch
	`,
		module.ID,
		module.Name,
		module.Purpose,
		module.PublicInterface,
		module.Dependencies,
		module.Dependents,
		module.InternalPatterns,
		module.OwnerDomain,
		module.RiskLevel,
		module.TestCoverageNotes,
		branch,
	)
	if err != nil {
		return fmt.Errorf("upsert module: %w", err)
	}
	return nil
}

// ─── Anti-Patterns ───────────────────────────────────────────────────────────

// GetAntiPatterns returns all known anti-patterns in the codex.
//
// Deprecated: returns anti-patterns across ALL products. Use
// GetAntiPatternsForProduct for product isolation.
func (c *Codex) GetAntiPatterns(ctx context.Context) ([]*models.CodexAntiPattern, error) {
	rows, err := c.pool.Query(ctx, `
		SELECT id, name, why_wrong, what_instead, historical_context
		FROM codex_anti_patterns
		ORDER BY name
	`)
	if err != nil {
		return nil, fmt.Errorf("get anti-patterns: %w", err)
	}
	defer rows.Close()

	var aps []*models.CodexAntiPattern
	for rows.Next() {
		ap := &models.CodexAntiPattern{}
		if err := rows.Scan(&ap.ID, &ap.Name, &ap.WhyWrong, &ap.WhatInstead, &ap.HistoricalContext); err != nil {
			return nil, fmt.Errorf("scan anti-pattern: %w", err)
		}
		aps = append(aps, ap)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate anti-patterns: %w", err)
	}
	return aps, nil
}

// ─── Product-Scoped Queries ───────────────────────────────────────────────────

// GetModulesForProduct returns all codex modules for the product on its
// configured default branch (read from repositories.base_branch). Back-compat
// wrapper for callers without explicit branch context — new code should
// resolve the work-item branch and call GetModulesForProductBranch directly.
// Fails with an error if the default branch can't be resolved — no "main"
// guess.
func (c *Codex) GetModulesForProduct(ctx context.Context, productID uuid.UUID) ([]*models.CodexModule, error) {
	branch, err := c.resolveDefault(ctx, productID)
	if err != nil {
		return nil, err
	}
	return c.GetModulesForProductBranch(ctx, productID, branch)
}

// GetModulesForProductBranch returns codex modules for (productID, branch).
// Branch must be non-empty. If the branch-scoped query returns zero rows
// AND the requested branch is not the product's default, we fall back to
// the default branch's codex and log the fallback loudly so feature
// branches without a re-index behave as if they were on default, not empty.
func (c *Codex) GetModulesForProductBranch(ctx context.Context, productID uuid.UUID, branch string) ([]*models.CodexModule, error) {
	if branch == "" {
		return nil, fmt.Errorf("get modules for product/branch: empty branch for product %s; caller must resolve branch", productID)
	}
	rows, err := c.pool.Query(ctx, `
		SELECT id, name, purpose, public_interface, dependencies, dependents, internal_patterns, owner_domain, risk_level, test_coverage_notes
		FROM codex_modules
		WHERE product_id = $1 AND branch = $2
		ORDER BY name
	`, productID, branch)
	if err != nil {
		return nil, fmt.Errorf("get modules for product/branch: %w", err)
	}
	modules, scanErr := scanModules(rows)
	rows.Close()
	if scanErr != nil {
		return nil, scanErr
	}
	if len(modules) == 0 {
		defaultBranch, dbErr := c.resolveDefault(ctx, productID)
		if dbErr == nil && defaultBranch != branch {
			log.Printf("codex: no branch-specific data for %s on product %s, falling back to %s", branch, productID, defaultBranch)
			return c.GetModulesForProductBranch(ctx, productID, defaultBranch)
		}
	}
	return modules, nil
}

// GetModulesForRepo returns all codex modules associated with the given repository.
func (c *Codex) GetModulesForRepo(ctx context.Context, repoID uuid.UUID) ([]*models.CodexModule, error) {
	rows, err := c.pool.Query(ctx, `
		SELECT id, name, purpose, public_interface, dependencies, dependents, internal_patterns, owner_domain, risk_level, test_coverage_notes
		FROM codex_modules
		WHERE repository_id = $1
		ORDER BY name
	`, repoID)
	if err != nil {
		return nil, fmt.Errorf("get modules for repo: %w", err)
	}
	defer rows.Close()

	return scanModules(rows)
}

// GetPatternsForProduct returns all codex patterns for the product on its
// configured default branch (read from repositories.base_branch).
// Back-compat wrapper; prefer GetPatternsForProductBranch.
func (c *Codex) GetPatternsForProduct(ctx context.Context, productID uuid.UUID) ([]*models.CodexPattern, error) {
	branch, err := c.resolveDefault(ctx, productID)
	if err != nil {
		return nil, err
	}
	return c.GetPatternsForProductBranch(ctx, productID, branch)
}

// GetPatternsForProductBranch returns patterns for (productID, branch).
// Branch must be non-empty. Falls back to the product's default branch
// when the branch-specific lookup is empty so feature branches without a
// re-index pick up the default branch's codex.
func (c *Codex) GetPatternsForProductBranch(ctx context.Context, productID uuid.UUID, branch string) ([]*models.CodexPattern, error) {
	if branch == "" {
		return nil, fmt.Errorf("get patterns for product/branch: empty branch for product %s; caller must resolve branch", productID)
	}
	rows, err := c.pool.Query(ctx, `
		SELECT id, name, category, when_to_use, reference_files, rationale, key_rules, common_mistakes
		FROM codex_patterns
		WHERE product_id = $1 AND branch = $2
		ORDER BY name
	`, productID, branch)
	if err != nil {
		return nil, fmt.Errorf("get patterns for product/branch: %w", err)
	}
	patterns, scanErr := scanPatterns(rows)
	rows.Close()
	if scanErr != nil {
		return nil, scanErr
	}
	if len(patterns) == 0 {
		defaultBranch, dbErr := c.resolveDefault(ctx, productID)
		if dbErr == nil && defaultBranch != branch {
			log.Printf("codex: no branch-specific data for %s on product %s, falling back to %s", branch, productID, defaultBranch)
			return c.GetPatternsForProductBranch(ctx, productID, defaultBranch)
		}
	}
	return patterns, nil
}

// GetAntiPatternsForProduct returns anti-patterns for the product on its
// configured default branch. Back-compat wrapper.
func (c *Codex) GetAntiPatternsForProduct(ctx context.Context, productID uuid.UUID) ([]*models.CodexAntiPattern, error) {
	branch, err := c.resolveDefault(ctx, productID)
	if err != nil {
		return nil, err
	}
	return c.GetAntiPatternsForProductBranch(ctx, productID, branch)
}

// GetSectionsForProduct returns codex sections for the product on its
// configured default branch. Back-compat wrapper.
func (c *Codex) GetSectionsForProduct(ctx context.Context, productID uuid.UUID) ([]*models.CodexSection, error) {
	branch, err := c.resolveDefault(ctx, productID)
	if err != nil {
		return nil, err
	}
	return c.GetSectionsForProductBranch(ctx, productID, branch)
}

// GetSectionsForProductBranch returns sections for (productID, branch).
// Falls back to the product's default branch when the branch-specific
// query is empty.
func (c *Codex) GetSectionsForProductBranch(ctx context.Context, productID uuid.UUID, branch string) ([]*models.CodexSection, error) {
	if branch == "" {
		return nil, fmt.Errorf("get sections for product/branch: empty branch for product %s; caller must resolve branch", productID)
	}
	rows, err := c.pool.Query(ctx, `
		SELECT id, section_type, name, content, version, updated_at
		FROM codex_sections
		WHERE product_id = $1 AND branch = $2
		ORDER BY name
	`, productID, branch)
	if err != nil {
		return nil, fmt.Errorf("get sections for product/branch: %w", err)
	}
	sections, scanErr := scanSections(rows)
	rows.Close()
	if scanErr != nil {
		return nil, scanErr
	}
	if len(sections) == 0 {
		defaultBranch, dbErr := c.resolveDefault(ctx, productID)
		if dbErr == nil && defaultBranch != branch {
			log.Printf("codex: no branch-specific data for %s on product %s, falling back to %s", branch, productID, defaultBranch)
			return c.GetSectionsForProductBranch(ctx, productID, defaultBranch)
		}
	}
	return sections, nil
}

// GetAntiPatternsForProductBranch returns anti-patterns for (productID, branch).
// Falls back to the product's default branch when the branch-specific query is empty.
func (c *Codex) GetAntiPatternsForProductBranch(ctx context.Context, productID uuid.UUID, branch string) ([]*models.CodexAntiPattern, error) {
	if branch == "" {
		return nil, fmt.Errorf("get anti-patterns for product/branch: empty branch for product %s; caller must resolve branch", productID)
	}
	rows, err := c.pool.Query(ctx, `
		SELECT id, name, why_wrong, what_instead, historical_context
		FROM codex_anti_patterns
		WHERE product_id = $1 AND branch = $2
		ORDER BY name
	`, productID, branch)
	if err != nil {
		return nil, fmt.Errorf("get anti-patterns for product/branch: %w", err)
	}
	var aps []*models.CodexAntiPattern
	for rows.Next() {
		ap := &models.CodexAntiPattern{}
		if err := rows.Scan(&ap.ID, &ap.Name, &ap.WhyWrong, &ap.WhatInstead, &ap.HistoricalContext); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan anti-pattern: %w", err)
		}
		aps = append(aps, ap)
	}
	iterErr := rows.Err()
	rows.Close()
	if iterErr != nil {
		return nil, fmt.Errorf("iterate anti-patterns: %w", iterErr)
	}
	if len(aps) == 0 {
		defaultBranch, dbErr := c.resolveDefault(ctx, productID)
		if dbErr == nil && defaultBranch != branch {
			log.Printf("codex: no branch-specific data for %s on product %s, falling back to %s", branch, productID, defaultBranch)
			return c.GetAntiPatternsForProductBranch(ctx, productID, defaultBranch)
		}
	}
	return aps, nil
}

// UpsertModuleForRepo inserts or updates a module, associating it with both a
// product and a repository. Delegates to UpsertModuleForRepoBranch with the
// branch field from the input model (defaulting to "main").
func (c *Codex) UpsertModuleForRepo(ctx context.Context, productID, repoID uuid.UUID, module *models.CodexModule) error {
	return c.UpsertModuleForRepoBranch(ctx, productID, repoID, module.Branch, module)
}

// UpsertModuleForRepoBranch tags the module with product_id, repository_id,
// and branch. Conflict target is (product_id, branch, name) matching the
// partial unique index codex_modules_product_branch_name_key added in
// migration 021.
func (c *Codex) UpsertModuleForRepoBranch(ctx context.Context, productID, repoID uuid.UUID, branch string, module *models.CodexModule) error {
	_, err := c.UpsertModuleForRepoBranchChanged(ctx, productID, repoID, branch, module)
	return err
}

// UpsertModuleForRepoBranchChanged is UpsertModuleForRepoBranch that also
// reports whether the module changed: the row is new, or its public
// interface, dependencies, internal patterns, owner domain, risk level or
// test coverage notes differ from the stored row. A new purpose alone (an
// LLM summary, different on every run) or new dependents alone (derived
// from the other modules) is not a change; the row is rewritten either way.
// The indexer records the changed modules as the version's module: entries
// (migration 060), so this is what decides whether a work item pinned to the
// module is affected.
func (c *Codex) UpsertModuleForRepoBranchChanged(ctx context.Context, productID, repoID uuid.UUID, branch string, module *models.CodexModule) (bool, error) {
	if module.ID == uuid.Nil {
		module.ID = uuid.New()
	}
	module.ProductID = &productID
	module.RepositoryID = &repoID
	if branch == "" {
		return false, fmt.Errorf("upsert module for repo/branch: empty branch for product %s module %q; caller must resolve branch before write", productID, module.Name)
	}
	module.Branch = branch

	// prev is read from the statement's snapshot, before the upsert.
	var changed bool
	err := c.pool.QueryRow(ctx, `
		WITH prev AS (
			SELECT public_interface, dependencies, internal_patterns, owner_domain, risk_level, test_coverage_notes
			  FROM codex_modules
			 WHERE product_id = $11 AND branch = $13 AND name = $2
		), up AS (
		INSERT INTO codex_modules
			(id, name, purpose, public_interface, dependencies, dependents, internal_patterns, owner_domain, risk_level, test_coverage_notes, product_id, repository_id, branch)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
		ON CONFLICT (product_id, branch, name) WHERE product_id IS NOT NULL DO UPDATE
		SET purpose             = EXCLUDED.purpose,
		    public_interface    = EXCLUDED.public_interface,
		    dependencies        = EXCLUDED.dependencies,
		    dependents          = EXCLUDED.dependents,
		    internal_patterns   = EXCLUDED.internal_patterns,
		    owner_domain        = EXCLUDED.owner_domain,
		    risk_level          = EXCLUDED.risk_level,
		    test_coverage_notes = EXCLUDED.test_coverage_notes,
		    product_id          = EXCLUDED.product_id,
		    repository_id       = EXCLUDED.repository_id
		RETURNING 1
		)
		SELECT NOT EXISTS (SELECT 1 FROM prev)
		    OR EXISTS (
		       SELECT 1 FROM prev
		        WHERE (public_interface, dependencies, internal_patterns, owner_domain, risk_level, test_coverage_notes)
		              IS DISTINCT FROM ($4::jsonb, $5::text[], $7::text, $8::text, $9::text, $10::text))
	`,
		module.ID,
		module.Name,
		module.Purpose,
		module.PublicInterface,
		module.Dependencies,
		module.Dependents,
		module.InternalPatterns,
		module.OwnerDomain,
		module.RiskLevel,
		module.TestCoverageNotes,
		productID,
		repoID,
		branch,
	).Scan(&changed)
	if err != nil {
		return false, fmt.Errorf("upsert module for repo/branch: %w", err)
	}
	return changed, nil
}

// ─── Agent Context Helpers ───────────────────────────────────────────────────

// QueryForAgent returns a combined context map containing the module definition,
// all patterns from the same category as the module's internal_patterns field,
// and all anti-patterns. Used to build focused agent context.
func (c *Codex) QueryForAgent(ctx context.Context, moduleName string) (map[string]interface{}, error) {
	module, err := c.GetModule(ctx, moduleName)
	if err != nil {
		return nil, fmt.Errorf("query for agent - module: %w", err)
	}

	patterns, err := c.GetPatternsByCategory(ctx, module.InternalPatterns)
	if err != nil {
		// Non-fatal: patterns might not exist for every module category
		patterns = nil
	}

	antiPatterns, err := c.GetAntiPatterns(ctx)
	if err != nil {
		return nil, fmt.Errorf("query for agent - anti-patterns: %w", err)
	}

	return map[string]interface{}{
		"module":        module,
		"patterns":      patterns,
		"anti_patterns": antiPatterns,
	}, nil
}

// BuildContextForIntent is the back-compat wrapper that resolves the
// product's default branch from repositories and delegates to
// BuildContextForIntentBranch. Fails loudly when no default branch is
// configured rather than silently querying "main".
func (c *Codex) BuildContextForIntent(ctx context.Context, productID uuid.UUID) (string, error) {
	branch, err := c.resolveDefault(ctx, productID)
	if err != nil {
		return "", err
	}
	return c.BuildContextForIntentBranch(ctx, productID, branch)
}

// BuildContextForIntentBranch returns a JSON context blob containing modules,
// patterns, and anti-patterns for (productID, branch). The returned payload
// is shaped { "modules": [...], "patterns": [...], "anti_patterns": [...] }.
// On an empty product (no modules), returns a payload with empty arrays
// AND logs a warning so operators notice — do not silently return "null"s.
//
// Modules are ordered by dependents count (descending) and capped at 20 to
// keep the context budget under control. All patterns and all anti-patterns
// for (product, branch) are included.
//
// Branch fallback (GroundingBranch, shared with guards.CheckCodexGrounded):
// when the requested branch has no module, the product's resolved default
// branch is read instead (never the literal "main"), and the fallback is
// logged. An empty branch (a work item whose branch is not set yet, which is
// every item until its PR) is resolved there too, to the product's default
// branch; a product without one is an error wrapping ErrNoDefaultBranch.
func (c *Codex) BuildContextForIntentBranch(ctx context.Context, productID uuid.UUID, branch string) (string, error) {
	type contextPayload struct {
		Modules      []*models.CodexModule      `json:"modules"`
		Patterns     []*models.CodexPattern     `json:"patterns"`
		AntiPatterns []*models.CodexAntiPattern `json:"anti_patterns"`
	}

	// Start with empty (non-nil) slices so the JSON payload never contains
	// "modules":null — an empty product should serialize as "modules":[] and
	// the warning log below is the signal that grounding is absent.
	payload := contextPayload{
		Modules:      []*models.CodexModule{},
		Patterns:     []*models.CodexPattern{},
		AntiPatterns: []*models.CodexAntiPattern{},
	}

	// Branch fallback (GroundingBranch, the rule guards.CheckCodexGrounded
	// uses too): a branch with no module reads the product's resolved
	// default branch instead, never the literal "main".
	effective, _, err := GroundingBranch(ctx, c.pool, productID, branch)
	if err != nil {
		return "", fmt.Errorf("build context - %w", err)
	}
	if effective != branch {
		if branch != "" {
			log.Printf("codex: no branch-specific data for %s on product %s, falling back to %s", branch, productID, effective)
		}
		branch = effective
	}

	// Top modules by dependents count. cardinality(dependents) handles the
	// empty-array case cleanly (returns 0). We cap at 20 rows so the context
	// payload stays within the LLM budget on large products.
	rows, err := c.pool.Query(ctx, `
		SELECT id, name, purpose, public_interface, dependencies, dependents, internal_patterns, owner_domain, risk_level, test_coverage_notes
		FROM codex_modules
		WHERE product_id = $1 AND branch = $2
		ORDER BY cardinality(dependents) DESC, name ASC
		LIMIT 20
	`, productID, branch)
	if err != nil {
		return "", fmt.Errorf("build context - modules: %w", err)
	}
	modules, scanErr := scanModules(rows)
	rows.Close()
	if scanErr != nil {
		return "", fmt.Errorf("build context - scan modules: %w", scanErr)
	}

	if len(modules) > 0 {
		payload.Modules = modules
	}

	// Use the branch-aware Get* helpers so patterns/anti-patterns apply the
	// same fallback rule. If this stage is already on main (either because
	// the caller asked for main or because we just fell back above), the
	// branch-aware Get* calls are straight lookups with no recursion.
	patterns, err := c.GetPatternsForProductBranch(ctx, productID, branch)
	if err != nil {
		return "", fmt.Errorf("build context - patterns: %w", err)
	}
	if len(patterns) > 0 {
		payload.Patterns = patterns
	}

	antiPatterns, err := c.GetAntiPatternsForProductBranch(ctx, productID, branch)
	if err != nil {
		return "", fmt.Errorf("build context - anti-patterns: %w", err)
	}
	if len(antiPatterns) > 0 {
		payload.AntiPatterns = antiPatterns
	}

	// F.10 observability: an empty codex context means every downstream LLM
	// call will run without grounding. Surface this as a log line AND a
	// Prometheus counter so it's alertable rather than silently regressing.
	if len(payload.Modules) == 0 {
		emptyContextTotal.Inc()
		log.Printf("codex: BuildContextForIntentBranch returned empty for product %s branch %s — agent will run with no codex grounding", productID, branch)
	}

	data, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("marshal context: %w", err)
	}
	return string(data), nil
}

// ─── Helpers ─────────────────────────────────────────────────────────────────

func scanSections(rows pgx.Rows) ([]*models.CodexSection, error) {
	var sections []*models.CodexSection
	for rows.Next() {
		s := &models.CodexSection{}
		if err := rows.Scan(&s.ID, &s.SectionType, &s.Name, &s.Content, &s.Version, &s.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan section: %w", err)
		}
		sections = append(sections, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate sections: %w", err)
	}
	return sections, nil
}

func scanPatterns(rows pgx.Rows) ([]*models.CodexPattern, error) {
	var patterns []*models.CodexPattern
	for rows.Next() {
		p := &models.CodexPattern{}
		if err := rows.Scan(
			&p.ID, &p.Name, &p.Category, &p.WhenToUse,
			&p.ReferenceFiles, &p.Rationale, &p.KeyRules, &p.CommonMistakes,
		); err != nil {
			return nil, fmt.Errorf("scan pattern: %w", err)
		}
		patterns = append(patterns, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate patterns: %w", err)
	}
	return patterns, nil
}

func scanModules(rows pgx.Rows) ([]*models.CodexModule, error) {
	var modules []*models.CodexModule
	for rows.Next() {
		m := &models.CodexModule{}
		if err := rows.Scan(
			&m.ID, &m.Name, &m.Purpose, &m.PublicInterface,
			&m.Dependencies, &m.Dependents, &m.InternalPatterns,
			&m.OwnerDomain, &m.RiskLevel, &m.TestCoverageNotes,
		); err != nil {
			return nil, fmt.Errorf("scan module: %w", err)
		}
		modules = append(modules, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate modules: %w", err)
	}
	return modules, nil
}
