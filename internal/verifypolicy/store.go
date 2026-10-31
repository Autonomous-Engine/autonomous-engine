package verifypolicy

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/evidence"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Querier is the read surface shared by *pgxpool.Pool and pgx.Tx.
type Querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Execer is the write surface shared by *pgxpool.Pool and pgx.Tx.
type Execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// Store reads repository_verification_policies (migration 051).
type Store struct {
	db Querier
}

// NewStore binds a Store to a pool (or any Querier).
func NewStore(db Querier) *Store { return &Store{db: db} }

const selectPolicySQL = `
	SELECT test_command, required_checks, no_checks_policy, security_review_paths,
	       COALESCE(set_by, ''), set_at, COALESCE(source, 'default')
	FROM repository_verification_policies
	WHERE repository_id = $1`

// LoadVerificationPolicy returns the stored policy for repoID and found=true,
// or Default(repoID) and found=false when no row exists. It is fail closed:
// on any error (nil store, query failure, a stored row that no longer
// validates) the returned policy is Default(repoID) and err says why, so the
// caller can log it and still use the policy.
func (s *Store) LoadVerificationPolicy(ctx context.Context, repoID uuid.UUID) (Policy, bool, error) {
	if s == nil || s.db == nil {
		return Default(repoID), false, errors.New("no verification policy store")
	}
	return LoadVerificationPolicy(ctx, s.db, repoID)
}

// LoadVerificationPolicy is Store.LoadVerificationPolicy against any Querier
// (a pool or the caller's transaction).
func LoadVerificationPolicy(ctx context.Context, q Querier, repoID uuid.UUID) (Policy, bool, error) {
	if q == nil {
		return Default(repoID), false, errors.New("no verification policy store")
	}
	var (
		testCommand *string
		required    []string
		noChecks    string
		globs       []string
		setBy       string
		setAt       *time.Time
		source      string
	)
	err := q.QueryRow(ctx, selectPolicySQL, repoID).Scan(&testCommand, &required, &noChecks, &globs, &setBy, &setAt, &source)
	if errors.Is(err, pgx.ErrNoRows) {
		return Default(repoID), false, nil
	}
	if err != nil {
		return Default(repoID), false, fmt.Errorf("load verification policy for repository %s: %w", repoID, err)
	}
	p := Policy{
		RepositoryID:        repoID,
		TestCommand:         testCommand,
		NoChecksPolicy:      NoChecksPolicy(noChecks),
		SecurityReviewPaths: globs,
		SetBy:               setBy,
		SetAt:               setAt,
		Source:              Source(source),
	}
	for _, k := range required {
		p.RequiredChecks = append(p.RequiredChecks, evidence.CheckKind(k))
	}
	p = p.Normalize()
	if err := p.Validate(); err != nil {
		return Default(repoID), false, fmt.Errorf("stored verification policy for repository %s is invalid: %w", repoID, err)
	}
	return p, true, nil
}

const upsertPolicySQL = `
	INSERT INTO repository_verification_policies (
		repository_id, test_command, required_checks, no_checks_policy,
		security_review_paths, set_by, set_at, source, created_at, updated_at
	) VALUES ($1, $2, $3, $4, $5, NULLIF($6, ''), $7, $8, NOW(), NOW())
	ON CONFLICT (repository_id) DO UPDATE SET
		test_command          = EXCLUDED.test_command,
		required_checks       = EXCLUDED.required_checks,
		no_checks_policy      = EXCLUDED.no_checks_policy,
		security_review_paths = EXCLUDED.security_review_paths,
		set_by                = EXCLUDED.set_by,
		set_at                = EXCLUDED.set_at,
		source                = EXCLUDED.source,
		updated_at            = NOW()`

// UpsertTx normalises, validates and stores p inside the caller's
// transaction (or on a pool), returning the stored form. SetAt defaults to
// now. The caller's transaction lets onboarding write the policy together
// with the repository row it belongs to.
func UpsertTx(ctx context.Context, ex Execer, p Policy) (Policy, error) {
	if ex == nil {
		return Policy{}, errors.New("upsert verification policy: nil database")
	}
	p, args, err := prepareUpsert(p)
	if err != nil {
		return Policy{}, err
	}
	if _, err := ex.Exec(ctx, upsertPolicySQL, args...); err != nil {
		return Policy{}, fmt.Errorf("upsert verification policy for repository %s: %w", p.RepositoryID, err)
	}
	return p, nil
}

// ErrNotOnboardingOwned is returned (wrapped) by UpsertOnboardingTx when the
// repository already has a policy that onboarding does not own (an
// administrator set it, or it was written outside the onboarding flow).
var ErrNotOnboardingOwned = errors.New("repository verification policy is not owned by onboarding")

// upsertOnboardingSQL is upsertPolicySQL restricted to rows onboarding owns:
// it inserts a missing row, updates a row whose source is 'onboarding', and
// leaves any other row untouched (no row returned).
const upsertOnboardingSQL = upsertPolicySQL + `
	WHERE repository_verification_policies.source = 'onboarding'
	RETURNING repository_id`

// UpsertOnboardingTx is the only write the unauthenticated onboarding
// surface may make (M3-03 ownership rule): it may create a policy for a
// repository that has none, or replace one onboarding itself wrote. A row
// set by an administrator (or from any other source) is never changed here;
// the call returns ErrNotOnboardingOwned, naming the owning source, and the
// row is left as it was. Admin-owned rows change only through the AdminOnly
// dashboard-backend editor (UpsertTx with source admin). p.Source must be
// SourceOnboarding.
func UpsertOnboardingTx(ctx context.Context, q Querier, p Policy) (Policy, error) {
	if q == nil {
		return Policy{}, errors.New("upsert verification policy: nil database")
	}
	if p.Source != SourceOnboarding {
		return Policy{}, fmt.Errorf("upsert onboarding verification policy: source %q, want %q", p.Source, SourceOnboarding)
	}
	p, args, err := prepareUpsert(p)
	if err != nil {
		return Policy{}, err
	}
	var id uuid.UUID
	err = q.QueryRow(ctx, upsertOnboardingSQL, args...).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		owner := "unknown"
		var src *string
		if lookupErr := q.QueryRow(ctx, `SELECT source FROM repository_verification_policies WHERE repository_id = $1`, p.RepositoryID).Scan(&src); lookupErr == nil && src != nil {
			owner = *src
		}
		return Policy{}, fmt.Errorf("%w: repository %s has a policy set via %s; change it on the admin repository page", ErrNotOnboardingOwned, p.RepositoryID, owner)
	}
	if err != nil {
		return Policy{}, fmt.Errorf("upsert verification policy for repository %s: %w", p.RepositoryID, err)
	}
	return p, nil
}

// prepareUpsert normalises and validates p, defaults SetAt, and returns the
// upsert arguments.
func prepareUpsert(p Policy) (Policy, []any, error) {
	if p.RepositoryID == uuid.Nil {
		return Policy{}, nil, errors.New("upsert verification policy: repository_id is required")
	}
	p = p.Normalize()
	if err := p.Validate(); err != nil {
		return Policy{}, nil, err
	}
	if p.SetAt == nil {
		now := time.Now().UTC()
		p.SetAt = &now
	}
	required := make([]string, 0, len(p.RequiredChecks))
	for _, k := range p.RequiredChecks {
		required = append(required, string(k))
	}
	return p, []any{
		p.RepositoryID, p.TestCommand, required, string(p.NoChecksPolicy),
		p.SecurityReviewPaths, p.SetBy, *p.SetAt, string(p.Source),
	}, nil
}
