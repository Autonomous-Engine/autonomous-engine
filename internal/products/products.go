package products

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/gitremote"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Product represents a logical grouping of repositories that together form one product.
type Product struct {
	ID          uuid.UUID `json:"id" db:"id"`
	Name        string    `json:"name" db:"name"`
	Description string    `json:"description" db:"description"`
	// PartialVerificationOptIn is the D2 policy flag (migration 045): when
	// true, partially_verified implementation evidence may advance to
	// mandatory human review instead of being blocked. It never permits
	// automatic release. Default false.
	PartialVerificationOptIn bool      `json:"partial_verification_opt_in" db:"partial_verification_opt_in"`
	CreatedAt                time.Time `json:"created_at" db:"created_at"`
	UpdatedAt                time.Time `json:"updated_at" db:"updated_at"`
}

// Repository represents an individual git repository belonging to a product.
type Repository struct {
	ID                uuid.UUID  `json:"id" db:"id"`
	ProductID         uuid.UUID  `json:"product_id" db:"product_id"`
	Name              string     `json:"name" db:"name"`
	GitHubURL         string     `json:"github_url" db:"github_url"`
	BaseBranch        string     `json:"base_branch" db:"base_branch"`
	MergeStrategy     string     `json:"merge_strategy" db:"merge_strategy"`
	BranchPrefix      string     `json:"branch_prefix" db:"branch_prefix"`
	PRTemplate        string     `json:"pr_template" db:"pr_template"`
	TokenOverride     string     `json:"token_override,omitempty" db:"token_override"`
	ClonePath         string     `json:"clone_path" db:"clone_path"`
	LastIndexedAt     *time.Time `json:"last_indexed_at,omitempty" db:"last_indexed_at"`
	LastIndexedCommit string     `json:"last_indexed_commit" db:"last_indexed_commit"`
	Enabled           bool       `json:"enabled" db:"enabled"`
	CreatedAt         time.Time  `json:"created_at" db:"created_at"`
	UpdatedAt         time.Time  `json:"updated_at" db:"updated_at"`
	// RemoteKind is the gitremote.Kind recorded when the repository was
	// added (migration 057). It is set on rows this store creates; reads do
	// not load it. Decisions use Kind(), which derives it from GitHubURL.
	RemoteKind string `json:"remote_kind,omitempty" db:"remote_kind"`
}

// Kind is the kind of remote GitHubURL names, derived from the URL itself
// (never from a stored label), so the PAT and GitHub API decisions cannot
// be steered by a stale or missing remote_kind.
func (r *Repository) Kind() gitremote.Kind {
	k, _ := gitremote.Classify(r.GitHubURL)
	return k
}

// InvalidRemoteError is returned when a repository's github_url is not a
// remote git can be given safely (gitremote.KindInvalid). Handlers map it to
// 400.
type InvalidRemoteError struct{ URL string }

func (e *InvalidRemoteError) Error() string {
	return fmt.Sprintf("invalid repository remote %q: use owner/repo, an https:// or ssh GitHub URL, another https:// git host, a file:// URL or an absolute path", e.URL)
}

// ClassifyRemote validates a github_url for storage: it returns the trimmed
// value and its kind, or an *InvalidRemoteError.
func ClassifyRemote(githubURL string) (string, gitremote.Kind, error) {
	k, _ := gitremote.Classify(githubURL)
	if k == gitremote.KindInvalid {
		return "", k, &InvalidRemoteError{URL: githubURL}
	}
	return strings.TrimSpace(githubURL), k, nil
}

// RepoDependency records a directional dependency between two repositories.
type RepoDependency struct {
	ID             uuid.UUID `json:"id" db:"id"`
	SourceRepoID   uuid.UUID `json:"source_repo_id" db:"source_repo_id"`
	TargetRepoID   uuid.UUID `json:"target_repo_id" db:"target_repo_id"`
	DependencyType string    `json:"dependency_type" db:"dependency_type"`
	Description    string    `json:"description" db:"description"`
	DiscoveredBy   string    `json:"discovered_by" db:"discovered_by"`
}

// OnboardingSession tracks the progress of a product onboarding workflow.
type OnboardingSession struct {
	ID            uuid.UUID              `json:"id" db:"id"`
	ProductID     uuid.UUID              `json:"product_id" db:"product_id"`
	Phase         string                 `json:"phase" db:"phase"`
	Prerequisites map[string]interface{} `json:"prerequisites" db:"prerequisites"`
	StartedAt     time.Time              `json:"started_at" db:"started_at"`
	CompletedAt   *time.Time             `json:"completed_at,omitempty" db:"completed_at"`
	StartedBy     *uuid.UUID             `json:"started_by,omitempty" db:"started_by"`
}

// ProductStore provides CRUD operations for products, repositories, dependencies, and onboarding sessions.
type ProductStore struct {
	db *pgxpool.Pool
}

// NewStore creates a new ProductStore backed by the provided connection pool.
func NewStore(db *pgxpool.Pool) *ProductStore {
	return &ProductStore{db: db}
}

// ─── Products ─────────────────────────────────────────────────────────────────

// CreateProduct inserts a new product and returns it with generated id and timestamps.
func (s *ProductStore) CreateProduct(ctx context.Context, name, description string) (*Product, error) {
	p := &Product{}
	err := s.db.QueryRow(ctx, `
		INSERT INTO products (name, description)
		VALUES ($1, $2)
		RETURNING id, name, description, partial_verification_opt_in, created_at, updated_at
	`, name, description).Scan(&p.ID, &p.Name, &p.Description, &p.PartialVerificationOptIn, &p.CreatedAt, &p.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("create product: %w", err)
	}
	return p, nil
}

// GetProduct retrieves a product by its ID.
func (s *ProductStore) GetProduct(ctx context.Context, id uuid.UUID) (*Product, error) {
	p := &Product{}
	err := s.db.QueryRow(ctx, `
		SELECT id, name, description, partial_verification_opt_in, created_at, updated_at
		FROM products
		WHERE id = $1
	`, id).Scan(&p.ID, &p.Name, &p.Description, &p.PartialVerificationOptIn, &p.CreatedAt, &p.UpdatedAt)
	if err != nil {
		if err == pgx.ErrNoRows {
			return nil, fmt.Errorf("product %s not found", id)
		}
		return nil, fmt.Errorf("get product: %w", err)
	}
	return p, nil
}

// ListProducts returns all products ordered by name.
func (s *ProductStore) ListProducts(ctx context.Context) ([]*Product, error) {
	rows, err := s.db.Query(ctx, `
		SELECT id, name, description, partial_verification_opt_in, created_at, updated_at
		FROM products
		ORDER BY name
	`)
	if err != nil {
		return nil, fmt.Errorf("list products: %w", err)
	}
	defer rows.Close()

	var products []*Product
	for rows.Next() {
		p := &Product{}
		if err := rows.Scan(&p.ID, &p.Name, &p.Description, &p.PartialVerificationOptIn, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan product: %w", err)
		}
		products = append(products, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate products: %w", err)
	}
	return products, nil
}

// UpdateProduct updates the name and description of an existing product.
func (s *ProductStore) UpdateProduct(ctx context.Context, id uuid.UUID, name, description string) error {
	_, err := s.db.Exec(ctx, `
		UPDATE products
		SET name = $1, description = $2, updated_at = NOW()
		WHERE id = $3
	`, name, description, id)
	if err != nil {
		return fmt.Errorf("update product: %w", err)
	}
	return nil
}

// SetPartialVerificationOptIn records the D2 policy decision for a product:
// whether partially_verified implementation evidence may advance to human
// review (true) or is blocked (false, the default). It is the only writer of
// products.partial_verification_opt_in.
func (s *ProductStore) SetPartialVerificationOptIn(ctx context.Context, id uuid.UUID, optIn bool) error {
	tag, err := s.db.Exec(ctx, `
		UPDATE products
		SET partial_verification_opt_in = $1, updated_at = NOW()
		WHERE id = $2
	`, optIn, id)
	if err != nil {
		return fmt.Errorf("set partial verification opt-in: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("product %s not found", id)
	}
	return nil
}

// DeleteProduct removes a product by ID (cascades to repositories and onboarding sessions).
func (s *ProductStore) DeleteProduct(ctx context.Context, id uuid.UUID) error {
	_, err := s.db.Exec(ctx, `DELETE FROM products WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("delete product: %w", err)
	}
	return nil
}

// ─── Repositories ─────────────────────────────────────────────────────────────

// AddRepository inserts a new repository into a product.
// clone_path is automatically set to /repo-cache/{productID}/{name}/.
func (s *ProductStore) AddRepository(ctx context.Context, productID uuid.UUID, name, githubURL, baseBranch, mergeStrategy, branchPrefix string) (*Repository, error) {
	return addRepository(ctx, s.db, productID, name, githubURL, baseBranch, mergeStrategy, branchPrefix)
}

// AddRepositoryTx is AddRepository inside the caller's transaction, so a
// caller can write rows that belong to the repository (its verification
// policy, migration 051) atomically with it.
func (s *ProductStore) AddRepositoryTx(ctx context.Context, tx pgx.Tx, productID uuid.UUID, name, githubURL, baseBranch, mergeStrategy, branchPrefix string) (*Repository, error) {
	if tx == nil {
		return nil, fmt.Errorf("add repository: nil transaction")
	}
	return addRepository(ctx, tx, productID, name, githubURL, baseBranch, mergeStrategy, branchPrefix)
}

// rowQuerier is the read surface shared by *pgxpool.Pool and pgx.Tx.
type rowQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func addRepository(ctx context.Context, q rowQuerier, productID uuid.UUID, name, githubURL, baseBranch, mergeStrategy, branchPrefix string) (*Repository, error) {
	githubURL, kind, err := ClassifyRemote(githubURL)
	if err != nil {
		return nil, err
	}
	clonePath := fmt.Sprintf("/repo-cache/%s/%s/", productID, name)
	r := &Repository{RemoteKind: string(kind)}
	err = q.QueryRow(ctx, `
		INSERT INTO repositories
			(product_id, name, github_url, base_branch, merge_strategy, branch_prefix, clone_path, remote_kind)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		RETURNING id, product_id, name, github_url, base_branch, merge_strategy, branch_prefix,
		          pr_template, token_override, clone_path, last_indexed_at, last_indexed_commit,
		          enabled, created_at, updated_at
	`, productID, name, githubURL, baseBranch, mergeStrategy, branchPrefix, clonePath, string(kind)).Scan(
		&r.ID, &r.ProductID, &r.Name, &r.GitHubURL, &r.BaseBranch, &r.MergeStrategy, &r.BranchPrefix,
		&r.PRTemplate, &r.TokenOverride, &r.ClonePath, &r.LastIndexedAt, &r.LastIndexedCommit,
		&r.Enabled, &r.CreatedAt, &r.UpdatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("add repository: %w", err)
	}
	return r, nil
}

// GetRepository retrieves a single repository by ID.
func (s *ProductStore) GetRepository(ctx context.Context, id uuid.UUID) (*Repository, error) {
	r := &Repository{}
	err := s.db.QueryRow(ctx, `
		SELECT id, product_id, name, github_url, base_branch, merge_strategy, branch_prefix,
		       pr_template, token_override, clone_path, last_indexed_at, last_indexed_commit,
		       enabled, created_at, updated_at
		FROM repositories
		WHERE id = $1
	`, id).Scan(
		&r.ID, &r.ProductID, &r.Name, &r.GitHubURL, &r.BaseBranch, &r.MergeStrategy, &r.BranchPrefix,
		&r.PRTemplate, &r.TokenOverride, &r.ClonePath, &r.LastIndexedAt, &r.LastIndexedCommit,
		&r.Enabled, &r.CreatedAt, &r.UpdatedAt,
	)
	if err != nil {
		if err == pgx.ErrNoRows {
			return nil, fmt.Errorf("repository %s not found", id)
		}
		return nil, fmt.Errorf("get repository: %w", err)
	}
	return r, nil
}

// GetRepositoriesForProduct returns all repositories belonging to the given product.
func (s *ProductStore) GetRepositoriesForProduct(ctx context.Context, productID uuid.UUID) ([]*Repository, error) {
	rows, err := s.db.Query(ctx, `
		SELECT id, product_id, name, github_url, base_branch, merge_strategy, branch_prefix,
		       pr_template, token_override, clone_path, last_indexed_at, last_indexed_commit,
		       enabled, created_at, updated_at
		FROM repositories
		WHERE product_id = $1
		ORDER BY name
	`, productID)
	if err != nil {
		return nil, fmt.Errorf("get repositories for product: %w", err)
	}
	defer rows.Close()

	return scanRepositories(rows)
}

// RepositoryUpdate carries optional field overrides for UpdateRepository.
// Only non-nil fields are applied.
type RepositoryUpdate struct {
	Name          *string
	GitHubURL     *string
	BaseBranch    *string
	MergeStrategy *string
	BranchPrefix  *string
	PRTemplate    *string
	TokenOverride *string
	ClonePath     *string
	Enabled       *bool
}

// UpdateRepository applies any combination of field updates to an existing repository.
func (s *ProductStore) UpdateRepository(ctx context.Context, id uuid.UUID, upd RepositoryUpdate) error {
	// Build SET clause dynamically
	setClauses := []string{"updated_at = NOW()"}
	args := []interface{}{id}
	argIdx := 2

	if upd.Name != nil {
		setClauses = append(setClauses, fmt.Sprintf("name = $%d", argIdx))
		args = append(args, *upd.Name)
		argIdx++
	}
	if upd.GitHubURL != nil {
		u, kind, err := ClassifyRemote(*upd.GitHubURL)
		if err != nil {
			return err
		}
		setClauses = append(setClauses, fmt.Sprintf("github_url = $%d", argIdx))
		args = append(args, u)
		argIdx++
		setClauses = append(setClauses, fmt.Sprintf("remote_kind = $%d", argIdx))
		args = append(args, string(kind))
		argIdx++
	}
	if upd.BaseBranch != nil {
		setClauses = append(setClauses, fmt.Sprintf("base_branch = $%d", argIdx))
		args = append(args, *upd.BaseBranch)
		argIdx++
	}
	if upd.MergeStrategy != nil {
		setClauses = append(setClauses, fmt.Sprintf("merge_strategy = $%d", argIdx))
		args = append(args, *upd.MergeStrategy)
		argIdx++
	}
	if upd.BranchPrefix != nil {
		setClauses = append(setClauses, fmt.Sprintf("branch_prefix = $%d", argIdx))
		args = append(args, *upd.BranchPrefix)
		argIdx++
	}
	if upd.PRTemplate != nil {
		setClauses = append(setClauses, fmt.Sprintf("pr_template = $%d", argIdx))
		args = append(args, *upd.PRTemplate)
		argIdx++
	}
	if upd.TokenOverride != nil {
		setClauses = append(setClauses, fmt.Sprintf("token_override = $%d", argIdx))
		args = append(args, *upd.TokenOverride)
		argIdx++
	}
	if upd.ClonePath != nil {
		setClauses = append(setClauses, fmt.Sprintf("clone_path = $%d", argIdx))
		args = append(args, *upd.ClonePath)
		argIdx++
	}
	if upd.Enabled != nil {
		setClauses = append(setClauses, fmt.Sprintf("enabled = $%d", argIdx))
		args = append(args, *upd.Enabled)
		argIdx++
	}

	query := "UPDATE repositories SET "
	for i, clause := range setClauses {
		if i > 0 {
			query += ", "
		}
		query += clause
	}
	query += " WHERE id = $1"

	_, err := s.db.Exec(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("update repository: %w", err)
	}
	return nil
}

// CopyRepoConfig copies base_branch, merge_strategy, branch_prefix, and pr_template
// from sourceRepoID to targetRepoID.
func (s *ProductStore) CopyRepoConfig(ctx context.Context, sourceRepoID, targetRepoID uuid.UUID) error {
	_, err := s.db.Exec(ctx, `
		UPDATE repositories AS target
		SET base_branch    = source.base_branch,
		    merge_strategy = source.merge_strategy,
		    branch_prefix  = source.branch_prefix,
		    pr_template    = source.pr_template,
		    updated_at     = NOW()
		FROM repositories AS source
		WHERE source.id = $1
		  AND target.id = $2
	`, sourceRepoID, targetRepoID)
	if err != nil {
		return fmt.Errorf("copy repo config: %w", err)
	}
	return nil
}

// DeleteRepository removes a repository by ID.
func (s *ProductStore) DeleteRepository(ctx context.Context, id uuid.UUID) error {
	_, err := s.db.Exec(ctx, `DELETE FROM repositories WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("delete repository: %w", err)
	}
	return nil
}

// UpdateIndexStatus marks a repository as indexed at the current time with the given commit hash.
func (s *ProductStore) UpdateIndexStatus(ctx context.Context, repoID uuid.UUID, commit string) error {
	_, err := s.db.Exec(ctx, `
		UPDATE repositories
		SET last_indexed_at = NOW(), last_indexed_commit = $1, updated_at = NOW()
		WHERE id = $2
	`, commit, repoID)
	if err != nil {
		return fmt.Errorf("update index status: %w", err)
	}
	return nil
}

// ─── Dependencies ─────────────────────────────────────────────────────────────

// AddDependency inserts a dependency between two repositories.
// If the pair already exists the insert is silently ignored.
func (s *ProductStore) AddDependency(ctx context.Context, sourceRepoID, targetRepoID uuid.UUID, depType, description, discoveredBy string) error {
	_, err := s.db.Exec(ctx, `
		INSERT INTO repo_dependencies (source_repo_id, target_repo_id, dependency_type, description, discovered_by)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (source_repo_id, target_repo_id) DO NOTHING
	`, sourceRepoID, targetRepoID, depType, description, discoveredBy)
	if err != nil {
		return fmt.Errorf("add dependency: %w", err)
	}
	return nil
}

// GetDependencies returns all dependency records where either the source or target
// repository belongs to the given product.
func (s *ProductStore) GetDependencies(ctx context.Context, productID uuid.UUID) ([]*RepoDependency, error) {
	rows, err := s.db.Query(ctx, `
		SELECT DISTINCT rd.id, rd.source_repo_id, rd.target_repo_id, rd.dependency_type, rd.description, rd.discovered_by
		FROM repo_dependencies rd
		JOIN repositories r ON r.id = rd.source_repo_id OR r.id = rd.target_repo_id
		WHERE r.product_id = $1
		ORDER BY rd.id
	`, productID)
	if err != nil {
		return nil, fmt.Errorf("get dependencies: %w", err)
	}
	defer rows.Close()

	return scanDependencies(rows)
}

// GetDependenciesForRepo returns all dependencies where the given repository is the source.
func (s *ProductStore) GetDependenciesForRepo(ctx context.Context, repoID uuid.UUID) ([]*RepoDependency, error) {
	rows, err := s.db.Query(ctx, `
		SELECT id, source_repo_id, target_repo_id, dependency_type, description, discovered_by
		FROM repo_dependencies
		WHERE source_repo_id = $1
		ORDER BY id
	`, repoID)
	if err != nil {
		return nil, fmt.Errorf("get dependencies for repo: %w", err)
	}
	defer rows.Close()

	return scanDependencies(rows)
}

// DeleteDependency removes a dependency record by ID.
func (s *ProductStore) DeleteDependency(ctx context.Context, id uuid.UUID) error {
	_, err := s.db.Exec(ctx, `DELETE FROM repo_dependencies WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("delete dependency: %w", err)
	}
	return nil
}

// ─── Onboarding Sessions ──────────────────────────────────────────────────────

// CreateSession starts a new onboarding session for a product.
func (s *ProductStore) CreateSession(ctx context.Context, productID uuid.UUID, startedBy *uuid.UUID) (*OnboardingSession, error) {
	sess := &OnboardingSession{}
	var prereqsJSON []byte

	err := s.db.QueryRow(ctx, `
		INSERT INTO onboarding_sessions (product_id, started_by)
		VALUES ($1, $2)
		RETURNING id, product_id, phase, prerequisites, started_at, completed_at, started_by
	`, productID, startedBy).Scan(
		&sess.ID, &sess.ProductID, &sess.Phase, &prereqsJSON,
		&sess.StartedAt, &sess.CompletedAt, &sess.StartedBy,
	)
	if err != nil {
		return nil, fmt.Errorf("create session: %w", err)
	}
	if err := json.Unmarshal(prereqsJSON, &sess.Prerequisites); err != nil {
		sess.Prerequisites = map[string]interface{}{}
	}
	return sess, nil
}

// GetActiveSession returns the most recent incomplete onboarding session for a product.
func (s *ProductStore) GetActiveSession(ctx context.Context, productID uuid.UUID) (*OnboardingSession, error) {
	sess := &OnboardingSession{}
	var prereqsJSON []byte

	err := s.db.QueryRow(ctx, `
		SELECT id, product_id, phase, prerequisites, started_at, completed_at, started_by
		FROM onboarding_sessions
		WHERE product_id = $1 AND completed_at IS NULL
		ORDER BY started_at DESC
		LIMIT 1
	`, productID).Scan(
		&sess.ID, &sess.ProductID, &sess.Phase, &prereqsJSON,
		&sess.StartedAt, &sess.CompletedAt, &sess.StartedBy,
	)
	if err != nil {
		if err == pgx.ErrNoRows {
			return nil, fmt.Errorf("no active onboarding session for product %s", productID)
		}
		return nil, fmt.Errorf("get active session: %w", err)
	}
	if err := json.Unmarshal(prereqsJSON, &sess.Prerequisites); err != nil {
		sess.Prerequisites = map[string]interface{}{}
	}
	return sess, nil
}

// UpdateSessionPhase advances the phase of an onboarding session and updates prerequisites.
func (s *ProductStore) UpdateSessionPhase(ctx context.Context, sessionID uuid.UUID, phase string, prerequisites map[string]interface{}) error {
	prereqsJSON, err := json.Marshal(prerequisites)
	if err != nil {
		return fmt.Errorf("marshal prerequisites: %w", err)
	}
	_, err = s.db.Exec(ctx, `
		UPDATE onboarding_sessions
		SET phase = $1, prerequisites = $2
		WHERE id = $3
	`, phase, prereqsJSON, sessionID)
	if err != nil {
		return fmt.Errorf("update session phase: %w", err)
	}
	return nil
}

// CompleteSession marks an onboarding session as finished.
func (s *ProductStore) CompleteSession(ctx context.Context, sessionID uuid.UUID) error {
	_, err := s.db.Exec(ctx, `
		UPDATE onboarding_sessions
		SET completed_at = NOW()
		WHERE id = $1
	`, sessionID)
	if err != nil {
		return fmt.Errorf("complete session: %w", err)
	}
	return nil
}

// ─── Helpers ──────────────────────────────────────────────────────────────────

func scanRepositories(rows pgx.Rows) ([]*Repository, error) {
	var repos []*Repository
	for rows.Next() {
		r := &Repository{}
		if err := rows.Scan(
			&r.ID, &r.ProductID, &r.Name, &r.GitHubURL, &r.BaseBranch, &r.MergeStrategy, &r.BranchPrefix,
			&r.PRTemplate, &r.TokenOverride, &r.ClonePath, &r.LastIndexedAt, &r.LastIndexedCommit,
			&r.Enabled, &r.CreatedAt, &r.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan repository: %w", err)
		}
		repos = append(repos, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate repositories: %w", err)
	}
	return repos, nil
}

func scanDependencies(rows pgx.Rows) ([]*RepoDependency, error) {
	var deps []*RepoDependency
	for rows.Next() {
		d := &RepoDependency{}
		if err := rows.Scan(&d.ID, &d.SourceRepoID, &d.TargetRepoID, &d.DependencyType, &d.Description, &d.DiscoveredBy); err != nil {
			return nil, fmt.Errorf("scan dependency: %w", err)
		}
		deps = append(deps, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate dependencies: %w", err)
	}
	return deps, nil
}
