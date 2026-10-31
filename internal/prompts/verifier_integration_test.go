package prompts

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/db"
	"github.com/jackc/pgx/v5/pgxpool"
)

// M3-07: verify_implementation is a late seed — ensured on every
// SeedDefaults call, so a database seeded before it existed receives it, and
// an operator's edit is never overwritten.
func TestIntegration_VerifyImplementationSeed(t *testing.T) {
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN not set — skipping DB-integration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := db.Migrate(ctx, pool, filepath.Join("..", "..", "migrations")); err != nil {
		t.Fatal(err)
	}
	ps := NewStore(pool)
	if err := ps.SeedDefaults(ctx); err != nil {
		t.Fatal(err)
	}
	// An already-seeded database without the late seed (as before M3-07).
	if _, err := pool.Exec(ctx, `DELETE FROM prompt_templates WHERE intent_type = $1`, IntentVerifyImplementation); err != nil {
		t.Fatal(err)
	}
	if err := ps.SeedDefaults(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := ps.Resolve(ctx, IntentVerifyImplementation, VerifierAgentType, "any", "model", "standard", "system")
	if err != nil || got != VerifyImplementationSystemPrompt {
		t.Fatalf("resolve = %.60q, %v", got, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE prompt_templates SET content = 'operator edit' WHERE intent_type = $1`, IntentVerifyImplementation); err != nil {
		t.Fatal(err)
	}
	if err := ps.SeedDefaults(ctx); err != nil {
		t.Fatal(err)
	}
	got, _ = ps.Resolve(ctx, IntentVerifyImplementation, VerifierAgentType, "", "", "", "system")
	if got != "operator edit" {
		t.Fatalf("operator edit overwritten: %.60q", got)
	}
	_, _ = pool.Exec(ctx, `UPDATE prompt_templates SET content = $2 WHERE intent_type = $1`, IntentVerifyImplementation, VerifyImplementationSystemPrompt)
}
