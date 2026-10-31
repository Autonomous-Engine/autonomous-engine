package db

import (
	"os"
	"path/filepath"
	"testing"
)

func TestUpMigrationFilesFindsAndSortsFiles(t *testing.T) {
	dir := t.TempDir()

	// Create files out of lexicographic order
	filenames := []string{
		"003_add_index.up.sql",
		"001_initial_schema.up.sql",
		"010_add_audit.up.sql",
		"002_add_users.up.sql",
	}
	for _, name := range filenames {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("SELECT 1;"), 0600); err != nil {
			t.Fatalf("write file %s: %v", name, err)
		}
	}

	files, err := upMigrationFiles(dir)
	if err != nil {
		t.Fatalf("upMigrationFiles error: %v", err)
	}

	if len(files) != 4 {
		t.Fatalf("got %d files, want 4", len(files))
	}

	expectedOrder := []string{
		"001_initial_schema.up.sql",
		"002_add_users.up.sql",
		"003_add_index.up.sql",
		"010_add_audit.up.sql",
	}
	for i, expected := range expectedOrder {
		got := filepath.Base(files[i])
		if got != expected {
			t.Errorf("files[%d] = %q, want %q", i, got, expected)
		}
	}
}

func TestUpMigrationFilesIgnoresNonUpSQLFiles(t *testing.T) {
	dir := t.TempDir()

	names := []string{
		"001_schema.up.sql",
		"001_schema.down.sql",
		"README.md",
		"002_users.up.sql",
		"not_a_migration.sql",
	}
	for _, name := range names {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(""), 0600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	files, err := upMigrationFiles(dir)
	if err != nil {
		t.Fatalf("upMigrationFiles error: %v", err)
	}

	if len(files) != 2 {
		t.Fatalf("got %d files, want 2; files: %v", len(files), files)
	}
}

func TestUpMigrationFilesEmptyDir(t *testing.T) {
	dir := t.TempDir()

	files, err := upMigrationFiles(dir)
	if err != nil {
		t.Fatalf("upMigrationFiles error: %v", err)
	}
	if len(files) != 0 {
		t.Errorf("expected 0 files in empty dir, got %d", len(files))
	}
}

func TestUpMigrationFilesNonExistentDir(t *testing.T) {
	_, err := upMigrationFiles("/nonexistent/path/to/migrations")
	if err == nil {
		t.Error("expected error for non-existent directory, got nil")
	}
}

func TestMigrationVersion(t *testing.T) {
	cases := []struct {
		path     string
		expected string
	}{
		{"/migrations/001_initial.up.sql", "001_initial"},
		{"/migrations/010_add_audit.up.sql", "010_add_audit"},
		{"relative/002_users.up.sql", "002_users"},
	}
	for _, tc := range cases {
		got := migrationVersion(tc.path)
		if got != tc.expected {
			t.Errorf("migrationVersion(%q) = %q, want %q", tc.path, got, tc.expected)
		}
	}
}
