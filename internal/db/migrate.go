package db

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

var alterTypeAddValueRe = regexp.MustCompile(`(?im)^ALTER\s+TYPE\s+\S+\s+ADD\s+VALUE\b[^;]*;`)

// Migrate runs all unapplied .up.sql migration files found in migrationsDir
// against the database pool. Applied migrations are tracked in the
// schema_migrations table. Each migration is executed inside a transaction.
func Migrate(ctx context.Context, pool *pgxpool.Pool, migrationsDir string) error {
	if err := ensureMigrationsTable(ctx, pool); err != nil {
		return fmt.Errorf("ensure migrations table: %w", err)
	}

	applied, err := appliedMigrations(ctx, pool)
	if err != nil {
		return fmt.Errorf("query applied migrations: %w", err)
	}

	files, err := upMigrationFiles(migrationsDir)
	if err != nil {
		return fmt.Errorf("list migration files: %w", err)
	}

	for _, file := range files {
		version := migrationVersion(file)
		if applied[version] {
			continue
		}

		if err := applyMigration(ctx, pool, file, version); err != nil {
			return fmt.Errorf("apply migration %s: %w", version, err)
		}

		log.Printf("applied migration: %s", version)
	}

	return nil
}

// ensureMigrationsTable creates the schema_migrations table if it does not
// already exist.
func ensureMigrationsTable(ctx context.Context, pool *pgxpool.Pool) error {
	_, err := pool.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version    TEXT        PRIMARY KEY,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)
	`)
	return err
}

// appliedMigrations returns a set of migration versions that have already been
// recorded in the schema_migrations table.
func appliedMigrations(ctx context.Context, pool *pgxpool.Pool) (map[string]bool, error) {
	rows, err := pool.Query(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	applied := make(map[string]bool)
	for rows.Next() {
		var version string
		if err := rows.Scan(&version); err != nil {
			return nil, err
		}
		applied[version] = true
	}
	return applied, rows.Err()
}

// upMigrationFiles returns all *.up.sql file paths under migrationsDir, sorted
// lexicographically so that migrations run in numeric order.
func upMigrationFiles(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}

	var files []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".up.sql") {
			files = append(files, filepath.Join(dir, e.Name()))
		}
	}
	sort.Strings(files)
	return files, nil
}

// migrationVersion extracts the version string from a migration file path. The
// version is the base filename without the .up.sql suffix.
func migrationVersion(path string) string {
	base := filepath.Base(path)
	return strings.TrimSuffix(base, ".up.sql")
}

// applyMigration executes the SQL in path inside a transaction and records the
// version in schema_migrations on success.
//
// PostgreSQL forbids ALTER TYPE … ADD VALUE inside a transaction, so any such
// statements are extracted and executed first outside the transaction. The
// remaining SQL (if any) runs inside a transaction as usual.
func applyMigration(ctx context.Context, pool *pgxpool.Pool, path, version string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read file: %w", err)
	}

	sqlText := string(raw)

	// Run ALTER TYPE … ADD VALUE statements outside a transaction.
	enumStmts := alterTypeAddValueRe.FindAllString(sqlText, -1)
	for _, stmt := range enumStmts {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("execute enum statement: %w", err)
		}
	}
	remaining := strings.TrimSpace(alterTypeAddValueRe.ReplaceAllString(sqlText, ""))

	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	if remaining != "" {
		if _, err := tx.Exec(ctx, remaining); err != nil {
			return fmt.Errorf("execute SQL: %w", err)
		}
	}

	if _, err := tx.Exec(ctx,
		`INSERT INTO schema_migrations (version) VALUES ($1)`, version,
	); err != nil {
		return fmt.Errorf("record version: %w", err)
	}

	return tx.Commit(ctx)
}
