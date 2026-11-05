package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// repoContext is what the chat model is told about a repository: its
// registered remote and the identity its own manifest declares. Without
// it the model guessed an owner and URL for the repository (F32).
type repoContext struct {
	Name   string
	URL    string
	Kind   string
	Module string
}

// moduleIdentity reads the identity a clone declares about itself: the
// go.mod module path, else the package.json name. "" when neither exists.
func moduleIdentity(clonePath string) string {
	if clonePath == "" {
		return ""
	}
	if f, err := os.Open(filepath.Join(clonePath, "go.mod")); err == nil {
		defer f.Close()
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if strings.HasPrefix(line, "module ") {
				return strings.TrimSpace(strings.TrimPrefix(line, "module "))
			}
		}
	}
	if b, err := os.ReadFile(filepath.Join(clonePath, "package.json")); err == nil {
		var pkg struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(b, &pkg) == nil {
			return strings.TrimSpace(pkg.Name)
		}
	}
	return ""
}

// formatRepoContext renders the repositories as context-block lines.
func formatRepoContext(repos []repoContext) string {
	var b strings.Builder
	for _, r := range repos {
		fmt.Fprintf(&b, "\n- Repository: %s — remote %s (%s)", r.Name, r.URL, r.Kind)
		if r.Module != "" {
			fmt.Fprintf(&b, "; declares module %s", r.Module)
		}
	}
	return b.String()
}

// loadRepoContext lists the product's enabled repositories with their
// declared identity. Errors yield "": the chat still works without it.
func loadRepoContext(ctx context.Context, pool *pgxpool.Pool, productID uuid.UUID) string {
	rows, err := pool.Query(ctx, `SELECT name, github_url, COALESCE(remote_kind::text, ''), COALESCE(clone_path, '')
		FROM repositories WHERE product_id = $1 AND enabled ORDER BY name`, productID)
	if err != nil {
		return ""
	}
	defer rows.Close()
	var repos []repoContext
	for rows.Next() {
		var r repoContext
		var clonePath string
		if err := rows.Scan(&r.Name, &r.URL, &r.Kind, &clonePath); err != nil {
			return ""
		}
		r.Module = moduleIdentity(clonePath)
		repos = append(repos, r)
	}
	return formatRepoContext(repos)
}
