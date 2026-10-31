package products

import (
	"context"
	"errors"
	"fmt"

	"github.com/autonomousengine/autonomous-engine/internal/evidence"
	"github.com/autonomousengine/autonomous-engine/internal/models"
	"github.com/google/uuid"
)

// PolicyStore is the slice of *ProductStore a consumer of implementation
// evidence needs to read the D2 partial-verification opt-in.
type PolicyStore interface {
	GetProduct(ctx context.Context, id uuid.UUID) (*Product, error)
}

// WorkItemLookup is the slice of *workgraph.WorkGraph needed to resolve a
// subtask's product through its parent.
type WorkItemLookup interface {
	GetWorkItem(ctx context.Context, id uuid.UUID) (*models.WorkItem, error)
}

// ErrNoProduct is returned (wrapped) by LoadReleasePolicyForItem when neither
// the item nor its parent names a product.
var ErrNoProduct = errors.New("work item has no product")

// LoadReleasePolicy resolves the evidence.ReleasePolicy for a product. It is
// fail-closed: the returned policy is always safe to use, and on any error
// (store unavailable, product missing) it is the zero policy, i.e. not opted
// in to partial verification. The error says why the default applied so the
// caller can log it.
func LoadReleasePolicy(ctx context.Context, store PolicyStore, productID uuid.UUID) (evidence.ReleasePolicy, error) {
	if store == nil {
		return evidence.ReleasePolicy{}, errors.New("no product store")
	}
	p, err := store.GetProduct(ctx, productID)
	if err != nil {
		return evidence.ReleasePolicy{}, fmt.Errorf("load product %s: %w", productID, err)
	}
	if p == nil {
		return evidence.ReleasePolicy{}, fmt.Errorf("load product %s: not found", productID)
	}
	return evidence.ReleasePolicy{PartialVerificationOptIn: p.PartialVerificationOptIn}, nil
}

// LoadReleasePolicyForItem resolves the policy that governs a work item: its
// own product_id, else its parent's (a subtask inherits the parent's
// product). Same fail-closed contract as LoadReleasePolicy: any failure,
// including a missing product (ErrNoProduct), yields the zero policy plus an
// explanatory error. items may be nil when no parent lookup is possible.
func LoadReleasePolicyForItem(ctx context.Context, store PolicyStore, items WorkItemLookup, item *models.WorkItem) (evidence.ReleasePolicy, error) {
	if item == nil {
		return evidence.ReleasePolicy{}, errors.New("no work item")
	}
	productID := item.ProductID
	if productID == nil && item.ParentID != nil {
		if items == nil {
			return evidence.ReleasePolicy{}, fmt.Errorf("work_item=%s has no product and parent %s cannot be looked up: %w", item.ID, *item.ParentID, ErrNoProduct)
		}
		parent, err := items.GetWorkItem(ctx, *item.ParentID)
		if err != nil {
			return evidence.ReleasePolicy{}, fmt.Errorf("get parent %s of work_item=%s for product policy: %w", *item.ParentID, item.ID, err)
		}
		if parent != nil {
			productID = parent.ProductID
		}
	}
	if productID == nil {
		return evidence.ReleasePolicy{}, fmt.Errorf("work_item=%s: %w", item.ID, ErrNoProduct)
	}
	policy, err := LoadReleasePolicy(ctx, store, *productID)
	if err != nil {
		return evidence.ReleasePolicy{}, fmt.Errorf("work_item=%s: %w", item.ID, err)
	}
	return policy, nil
}
