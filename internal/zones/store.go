package zones

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/codex"
	"github.com/autonomousengine/autonomous-engine/internal/models"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	// ErrNotFound: no zone (or repository) with that id.
	ErrNotFound = errors.New("zone not found")
	// ErrDuplicate: the repository already has a zone with that name.
	ErrDuplicate = errors.New("zone name already used in this repository")
	// ErrNoBranch: the repository records no base branch, so the zone
	// change has no codex version to write.
	ErrNoBranch = errors.New("repository has no base branch")
)

// ZonesSection is the typed sections_changed entry every zone write
// records (codex.SectionChange("zones")).
var ZonesSection = codex.SectionChange("zones")

// Store reads and writes code_zones. Every write runs in one transaction
// with a codex version (section:zones) for the repository's product on its
// base branch, so a zone never changes without a version announcing it.
type Store struct {
	pool *pgxpool.Pool
}

// NewStore returns a Store on pool.
func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// columnsOf is the scanZone column list, each qualified with prefix ("" or
// "z.").
func columnsOf(prefix string) string {
	p := prefix
	return p + "id, COALESCE(" + p + "repository_id, '00000000-0000-0000-0000-000000000000'::uuid), " +
		p + "name, " + p + "description, " + p + "patterns, " + p + "module_paths, " +
		p + "policy::text, " + p + "status::text, " + p + "frozen_reason, " + p + "frozen_at, " +
		p + "source, " + p + "created_at, " + p + "updated_at"
}

var zoneColumns = columnsOf("")

type rowScanner interface {
	Scan(dest ...any) error
}

func scanZone(r rowScanner) (Zone, error) {
	var z Zone
	var policy, status, source string
	err := r.Scan(&z.ID, &z.RepositoryID, &z.Name, &z.Description, &z.Patterns, &z.ModulePaths,
		&policy, &status, &z.FrozenReason, &z.FrozenAt, &source, &z.CreatedAt, &z.UpdatedAt)
	if err != nil {
		return Zone{}, err
	}
	z.Policy, z.Status, z.Source = Policy(policy), models.ZoneStatus(status), Source(source)
	if z.Patterns == nil {
		z.Patterns = []string{}
	}
	if z.ModulePaths == nil {
		z.ModulePaths = []string{}
	}
	return z, nil
}

type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

func listZones(ctx context.Context, q querier, sql string, args ...any) ([]Zone, error) {
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Zone{}
	for rows.Next() {
		z, err := scanZone(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, z)
	}
	return out, rows.Err()
}

// ListForRepository returns a repository's zones by name.
func (s *Store) ListForRepository(ctx context.Context, repoID uuid.UUID) ([]Zone, error) {
	zs, err := listZones(ctx, s.pool, `SELECT `+zoneColumns+` FROM code_zones WHERE repository_id = $1 ORDER BY name`, repoID)
	if err != nil {
		return nil, fmt.Errorf("list zones of repository %s: %w", repoID, err)
	}
	return zs, nil
}

// ListForProduct returns the zones of every repository of a product, by
// repository then name.
func (s *Store) ListForProduct(ctx context.Context, productID uuid.UUID) ([]Zone, error) {
	zs, err := listZones(ctx, s.pool, `
		SELECT `+columnsOf("z.")+`
		  FROM code_zones z
		  JOIN repositories r ON r.id = z.repository_id
		 WHERE r.product_id = $1
		 ORDER BY z.repository_id, z.name`, productID)
	if err != nil {
		return nil, fmt.Errorf("list zones of product %s: %w", productID, err)
	}
	return zs, nil
}

// Get returns one zone.
func (s *Store) Get(ctx context.Context, id uuid.UUID) (Zone, error) {
	z, err := scanZone(s.pool.QueryRow(ctx, `SELECT `+zoneColumns+` FROM code_zones WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Zone{}, ErrNotFound
	}
	if err != nil {
		return Zone{}, fmt.Errorf("get zone %s: %w", id, err)
	}
	return z, nil
}

// Update is a partial edit; nil / nil-slice fields are left unchanged. An
// edited zone's source becomes dashboard.
type Update struct {
	Patterns    []string
	Policy      *Policy
	Description *string
}

// Create inserts a zone for repoID from t.
func (s *Store) Create(ctx context.Context, repoID uuid.UUID, t Template) (Zone, error) {
	var out Zone
	err := s.withVersion(ctx, repoID, versionSource(t.Source), func(tx pgx.Tx) (string, error) {
		z, err := insertTx(ctx, tx, repoID, t)
		out = z
		return fmt.Sprintf("zone %s created (%s)", z.Name, z.Policy), err
	})
	return out, err
}

// insertTx creates one zone; it never overwrites (a duplicate name is
// ErrDuplicate).
func insertTx(ctx context.Context, tx pgx.Tx, repoID uuid.UUID, t Template) (Zone, error) {
	t.Name = strings.TrimSpace(t.Name)
	t.Patterns = NormalizePatterns(t.Patterns)
	if err := Validate(t.Name, t.Patterns, t.Policy); err != nil {
		return Zone{}, err
	}
	if !t.Source.Valid() {
		return Zone{}, fmt.Errorf("%w: source %q", ErrInvalid, t.Source)
	}
	sql := `INSERT INTO code_zones (repository_id, name, description, patterns, module_paths, policy, source, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6::zone_policy, $7, now())`
	z, err := scanZone(tx.QueryRow(ctx, sql+` RETURNING `+zoneColumns,
		repoID, t.Name, strings.TrimSpace(t.Description), t.Patterns, DeriveModulePaths(t.Patterns), string(t.Policy), string(t.Source)))
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return Zone{}, fmt.Errorf("%w: %s", ErrDuplicate, t.Name)
		}
		return Zone{}, fmt.Errorf("insert zone %s: %w", t.Name, err)
	}
	return z, nil
}

// lockZone reads one zone FOR UPDATE inside tx, so a read-modify-write
// cannot lose a concurrent edit.
func lockZone(ctx context.Context, tx pgx.Tx, id uuid.UUID) (Zone, error) {
	z, err := scanZone(tx.QueryRow(ctx, `SELECT `+zoneColumns+` FROM code_zones WHERE id = $1 FOR UPDATE`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Zone{}, ErrNotFound
	}
	if err != nil {
		return Zone{}, fmt.Errorf("lock zone %s: %w", id, err)
	}
	return z, nil
}

// repositoryOf is the repository of zone id (to scope its version).
func (s *Store) repositoryOf(ctx context.Context, id uuid.UUID) (uuid.UUID, error) {
	z, err := s.Get(ctx, id)
	if err != nil {
		return uuid.Nil, err
	}
	return z.RepositoryID, nil
}

// Update edits patterns, policy and description (the admin dashboard).
// The row is read FOR UPDATE in the write's transaction.
func (s *Store) Update(ctx context.Context, id uuid.UUID, u Update) (Zone, error) {
	repoID, err := s.repositoryOf(ctx, id)
	if err != nil {
		return Zone{}, err
	}
	var out Zone
	err = s.withVersion(ctx, repoID, models.CodexChangeSourceHumanUpdate, func(tx pgx.Tx) (string, error) {
		cur, err := lockZone(ctx, tx, id)
		if err != nil {
			return "", err
		}
		next := cur
		if u.Patterns != nil {
			next.Patterns = NormalizePatterns(u.Patterns)
		}
		if u.Policy != nil {
			next.Policy = *u.Policy
		}
		if u.Description != nil {
			next.Description = strings.TrimSpace(*u.Description)
		}
		if err := Validate(next.Name, next.Patterns, next.Policy); err != nil {
			return "", err
		}
		z, err := scanZone(tx.QueryRow(ctx, `
			UPDATE code_zones
			   SET patterns = $2, module_paths = $3, policy = $4::zone_policy, description = $5,
			       source = 'dashboard', updated_at = now()
			 WHERE id = $1
			RETURNING `+zoneColumns, id, next.Patterns, DeriveModulePaths(next.Patterns), string(next.Policy), next.Description))
		out = z
		return fmt.Sprintf("zone %s edited (%s, %d pattern(s))", cur.Name, next.Policy, len(next.Patterns)), err
	})
	return out, err
}

// Freeze stops new work in the zone; reason is required.
func (s *Store) Freeze(ctx context.Context, id uuid.UUID, reason string) (Zone, error) {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return Zone{}, fmt.Errorf("%w: freezing a zone needs a reason", ErrInvalid)
	}
	return s.setStatus(ctx, id, fmt.Sprintf("frozen: %s", reason), `
		UPDATE code_zones SET status = 'frozen', frozen_reason = $2, frozen_at = now(), updated_at = now()
		 WHERE id = $1 RETURNING `+zoneColumns, reason)
}

// Thaw makes a frozen zone active again. (The thawing status of migration
// 003 is kept in the enum but not used: a thaw takes effect at once.)
func (s *Store) Thaw(ctx context.Context, id uuid.UUID) (Zone, error) {
	return s.setStatus(ctx, id, "thawed", `
		UPDATE code_zones SET status = 'active', frozen_reason = NULL, frozen_at = NULL, updated_at = now()
		 WHERE id = $1 RETURNING `+zoneColumns)
}

func (s *Store) setStatus(ctx context.Context, id uuid.UUID, what, sql string, args ...any) (Zone, error) {
	repoID, err := s.repositoryOf(ctx, id)
	if err != nil {
		return Zone{}, err
	}
	var out Zone
	err = s.withVersion(ctx, repoID, models.CodexChangeSourceHumanUpdate, func(tx pgx.Tx) (string, error) {
		cur, err := lockZone(ctx, tx, id)
		if err != nil {
			return "", err
		}
		z, err := scanZone(tx.QueryRow(ctx, sql, append([]any{id}, args...)...))
		out = z
		return fmt.Sprintf("zone %s %s", cur.Name, what), err
	})
	return out, err
}

// Delete removes a zone (the admin dashboard).
func (s *Store) Delete(ctx context.Context, id uuid.UUID) error {
	repoID, err := s.repositoryOf(ctx, id)
	if err != nil {
		return err
	}
	return s.withVersion(ctx, repoID, models.CodexChangeSourceHumanUpdate, func(tx pgx.Tx) (string, error) {
		cur, err := lockZone(ctx, tx, id)
		if err != nil {
			return "", err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM code_zones WHERE id = $1`, id); err != nil {
			return "", fmt.Errorf("delete zone %s: %w", id, err)
		}
		return fmt.Sprintf("zone %s deleted", cur.Name), nil
	})
}

// Confirmed reports whether onboarding has set the repository's zones
// (repositories.zones_confirmed_at), an explicit "no zones" choice
// included.
func (s *Store) Confirmed(ctx context.Context, repoID uuid.UUID) (bool, error) {
	var at *time.Time
	err := s.pool.QueryRow(ctx, `SELECT zones_confirmed_at FROM repositories WHERE id = $1`, repoID).Scan(&at)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, fmt.Errorf("%w: repository %s", ErrNotFound, repoID)
	}
	if err != nil {
		return false, err
	}
	return at != nil, nil
}

// lockRepositoryZones locks the repository row (serialising onboarding
// writes for it) and reports whether its zones were already set: confirmed
// by onboarding, or any zone present (e.g. created in the dashboard).
func lockRepositoryZones(ctx context.Context, tx pgx.Tx, repoID uuid.UUID) (bool, error) {
	var confirmed bool
	err := tx.QueryRow(ctx, `
		SELECT r.zones_confirmed_at IS NOT NULL
		       OR EXISTS (SELECT 1 FROM code_zones z WHERE z.repository_id = r.id)
		  FROM repositories r WHERE r.id = $1 FOR UPDATE OF r`, repoID).Scan(&confirmed)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, fmt.Errorf("%w: repository %s", ErrNotFound, repoID)
	}
	return confirmed, err
}

func markConfirmed(ctx context.Context, tx pgx.Tx, repoID uuid.UUID) error {
	_, err := tx.Exec(ctx, `UPDATE repositories SET zones_confirmed_at = COALESCE(zones_confirmed_at, now()) WHERE id = $1`, repoID)
	return err
}

// ApplyIfEmpty seeds a repository whose zones were never set (onboarding
// approval: the codex-config defaults and the risk_zones proposals), in one
// transaction with one codex version, and records the repository as
// confirmed. A repository already confirmed (by the wizard, an explicit
// "no zones" choice included) or with any zone is left alone and 0 is
// returned.
func (s *Store) ApplyIfEmpty(ctx context.Context, repoID uuid.UUID, templates []Template, summary string) (int, error) {
	created := 0
	err := s.withVersion(ctx, repoID, models.CodexChangeSourceOnboarding, func(tx pgx.Tx) (string, error) {
		confirmed, err := lockRepositoryZones(ctx, tx, repoID)
		if err != nil || confirmed {
			return "", err
		}
		seen := map[string]bool{}
		for _, t := range templates {
			if seen[strings.TrimSpace(t.Name)] {
				continue
			}
			seen[strings.TrimSpace(t.Name)] = true
			if _, err := insertTx(ctx, tx, repoID, t); err != nil {
				return "", err
			}
			created++
		}
		if err := markConfirmed(ctx, tx, repoID); err != nil {
			return "", err
		}
		if created == 0 {
			return "", nil
		}
		return summary, nil
	})
	if err != nil {
		return 0, err
	}
	return created, nil
}

// OnboardingResult is what ConfirmOnboarding did.
type OnboardingResult struct {
	// FirstTime: the repository's zones were not set before; the
	// confirmation set them to exactly the listed zones.
	FirstTime bool `json:"first_time"`
	// Added are the zones created; Kept the listed zones that already
	// existed and were left exactly as they are.
	Added []string `json:"added"`
	Kept  []string `json:"kept"`
	// Zones are the repository's zones afterwards.
	Zones []Zone `json:"zones"`
}

// ConfirmOnboarding is the onboarding wizard's confirmation. Onboarding is
// unauthenticated, so it may never relax state (the M3-03 rule of
// verifypolicy.UpsertOnboardingTx):
//
//   - the first confirmation of a repository whose zones were never set
//     writes exactly the listed zones (none: an explicit "no zones" choice,
//     recorded so approval seeding does not re-apply the defaults);
//   - after that it may only ADD zones whose name is new. A listed zone that
//     already exists is left untouched whatever the request says — no
//     policy change, no unfreeze, no pattern change, no source rewrite —
//     and no zone is ever deleted. Edits, weakening and deletion go through
//     the admin dashboard (Update, Freeze/Thaw, Delete).
//
// One transaction; a codex version (section:zones) only when a zone was
// written.
func (s *Store) ConfirmOnboarding(ctx context.Context, repoID uuid.UUID, templates []Template, summary string) (OnboardingResult, error) {
	var res OnboardingResult
	err := s.withVersion(ctx, repoID, models.CodexChangeSourceOnboarding, func(tx pgx.Tx) (string, error) {
		confirmed, err := lockRepositoryZones(ctx, tx, repoID)
		if err != nil {
			return "", err
		}
		res.FirstTime = !confirmed
		existing := map[string]bool{}
		zs, err := listZones(ctx, tx, `SELECT `+zoneColumns+` FROM code_zones WHERE repository_id = $1`, repoID)
		if err != nil {
			return "", err
		}
		for _, z := range zs {
			existing[z.Name] = true
		}
		res.Added, res.Kept = []string{}, []string{}
		for _, t := range templates {
			name := strings.TrimSpace(t.Name)
			if existing[name] {
				res.Kept = append(res.Kept, name)
				continue
			}
			if t.Source != SourceConfig {
				t.Source = SourceOnboarding
			}
			if _, err := insertTx(ctx, tx, repoID, t); err != nil {
				return "", err
			}
			existing[name] = true
			res.Added = append(res.Added, name)
		}
		sort.Strings(res.Added)
		sort.Strings(res.Kept)
		if err := markConfirmed(ctx, tx, repoID); err != nil {
			return "", err
		}
		res.Zones, err = listZones(ctx, tx, `SELECT `+zoneColumns+` FROM code_zones WHERE repository_id = $1 ORDER BY name`, repoID)
		if err != nil || len(res.Added) == 0 {
			return "", err
		}
		return fmt.Sprintf("%s: %d zone(s) added", summary, len(res.Added)), nil
	})
	return res, err
}

func versionSource(s Source) models.CodexChangeSource {
	if s == SourceDashboard {
		return models.CodexChangeSourceHumanUpdate
	}
	return models.CodexChangeSourceOnboarding
}

// withVersion runs write and, when it reports a change (a non-empty
// summary), records the zones codex version for the repository's product on
// its base branch in the same transaction. An empty summary commits the
// write with no version (nothing about the zones changed).
func (s *Store) withVersion(ctx context.Context, repoID uuid.UUID, source models.CodexChangeSource, write func(tx pgx.Tx) (summary string, err error)) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("zones: begin: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op once committed

	var productID uuid.UUID
	var branch string
	err = tx.QueryRow(ctx, `SELECT product_id, base_branch FROM repositories WHERE id = $1`, repoID).Scan(&productID, &branch)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: repository %s", ErrNotFound, repoID)
	}
	if err != nil {
		return fmt.Errorf("zones: look up repository %s: %w", repoID, err)
	}
	if strings.TrimSpace(branch) == "" {
		return fmt.Errorf("%w: repository %s", ErrNoBranch, repoID)
	}
	summary, err := write(tx)
	if err != nil {
		return err
	}
	if summary != "" {
		if _, err := codex.RecordVersionTx(ctx, tx, codex.NewVersion{
			ProductID:       productID,
			Branch:          branch,
			SectionsChanged: []string{ZonesSection},
			Summary:         summary,
			Source:          source,
			Trigger:         codex.TriggerZoneChange,
		}); err != nil {
			return err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("zones: commit: %w", err)
	}
	return nil
}
