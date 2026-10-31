package codex

import (
	"context"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/models"
	"github.com/google/uuid"
)

// The Codex type wraps a database pool, so we cannot instantiate it without a
// real Postgres connection. This file documents the interface contract and tests
// any pure helper logic that is accessible from the package.

// TestCodexInterfaceContract verifies that the Codex type exposes the expected
// methods at compile time. If any method signature changes, this file will fail
// to compile, alerting the developer.
func TestCodexInterfaceContract(t *testing.T) {
	// The test is a compile-time check: we declare function variables with the
	// exact signatures and assign method values. This will not compile if a
	// method is missing or has a wrong signature.

	var c *Codex

	_ = func(ctx context.Context, name string) (*models.CodexSection, error) {
		return c.GetSection(ctx, name)
	}

	_ = func(ctx context.Context, st models.CodexSectionType) ([]*models.CodexSection, error) {
		return c.GetSectionsByType(ctx, st)
	}

	_ = func(ctx context.Context, s *models.CodexSection) error {
		return c.UpsertSection(ctx, s)
	}

	_ = func(ctx context.Context, name string) (*models.CodexPattern, error) {
		return c.GetPattern(ctx, name)
	}

	_ = func(ctx context.Context, cat string) ([]*models.CodexPattern, error) {
		return c.GetPatternsByCategory(ctx, cat)
	}

	_ = func(ctx context.Context, p *models.CodexPattern) error {
		return c.UpsertPattern(ctx, p)
	}

	_ = func(ctx context.Context, name string) (*models.CodexModule, error) {
		return c.GetModule(ctx, name)
	}

	_ = func(ctx context.Context, m *models.CodexModule) error {
		return c.UpsertModule(ctx, m)
	}

	_ = func(ctx context.Context) ([]*models.CodexAntiPattern, error) {
		return c.GetAntiPatterns(ctx)
	}

	_ = func(ctx context.Context, moduleName string) (map[string]interface{}, error) {
		return c.QueryForAgent(ctx, moduleName)
	}

	_ = func(ctx context.Context, productID uuid.UUID) (string, error) {
		return c.BuildContextForIntent(ctx, productID)
	}

	_ = func(ctx context.Context, productID uuid.UUID) ([]*models.CodexPattern, error) {
		return c.GetPatternsForProduct(ctx, productID)
	}

	_ = func(ctx context.Context, productID uuid.UUID) ([]*models.CodexAntiPattern, error) {
		return c.GetAntiPatternsForProduct(ctx, productID)
	}

	_ = func(ctx context.Context, productID uuid.UUID) ([]*models.CodexModule, error) {
		return c.GetModulesForProduct(ctx, productID)
	}

	// Branch-aware variants added in migration 021. Compile-time only —
	// verifies that the new signatures match the spec.
	_ = func(ctx context.Context, productID uuid.UUID, branch string) ([]*models.CodexModule, error) {
		return c.GetModulesForProductBranch(ctx, productID, branch)
	}
	_ = func(ctx context.Context, productID uuid.UUID, branch string) ([]*models.CodexPattern, error) {
		return c.GetPatternsForProductBranch(ctx, productID, branch)
	}
	_ = func(ctx context.Context, productID uuid.UUID, branch string) ([]*models.CodexAntiPattern, error) {
		return c.GetAntiPatternsForProductBranch(ctx, productID, branch)
	}
	_ = func(ctx context.Context, productID uuid.UUID, branch string) (string, error) {
		return c.BuildContextForIntentBranch(ctx, productID, branch)
	}
	_ = func(ctx context.Context, productID uuid.UUID) ([]*models.CodexSection, error) {
		return c.GetSectionsForProduct(ctx, productID)
	}
	_ = func(ctx context.Context, productID uuid.UUID, branch string) ([]*models.CodexSection, error) {
		return c.GetSectionsForProductBranch(ctx, productID, branch)
	}
}

// TestNewCodexReturnsPtrWithPool verifies that New accepts a nil pool and
// returns a non-nil *Codex. A nil pool will panic only when a method is called,
// not at construction time.
func TestNewCodexReturnsNonNil(t *testing.T) {
	c := New(nil)
	if c == nil {
		t.Fatal("New(nil) returned nil")
	}
}

// TestUpsertSectionAssignsIDWhenNil verifies that UpsertSection assigns a new
// UUID when section.ID is the zero value. We cannot run the DB call, but we can
// verify the ID-assignment branch by inspecting the section value before and
// after — since the pool call will panic on nil, we use a defer/recover guard.
func TestUpsertSectionAssignsIDWhenNil(t *testing.T) {
	c := New(nil)
	section := &models.CodexSection{
		// ID deliberately left as zero value
		Name:    "test-section",
		Content: []byte(`"content"`),
	}

	if section.ID != uuid.Nil {
		t.Fatal("precondition: section.ID should be zero")
	}

	// The actual DB call will panic because pool is nil. We recover and then
	// verify that the ID was assigned before the panic occurred.
	defer func() {
		recover() //nolint:errcheck
		if section.ID == uuid.Nil {
			t.Error("expected UpsertSection to assign a non-zero UUID before the DB call")
		}
	}()

	_ = c.UpsertSection(context.Background(), section)
}
