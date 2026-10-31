package products_test

import (
	"context"
	"errors"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/evidence"
	"github.com/autonomousengine/autonomous-engine/internal/models"
	"github.com/autonomousengine/autonomous-engine/internal/products"
	"github.com/google/uuid"
)

type fakeProducts struct {
	byID map[uuid.UUID]*products.Product
	err  error
}

func (f fakeProducts) GetProduct(_ context.Context, id uuid.UUID) (*products.Product, error) {
	if f.err != nil {
		return nil, f.err
	}
	p, ok := f.byID[id]
	if !ok {
		return nil, errors.New("product " + id.String() + " not found")
	}
	return p, nil
}

type fakeItems struct {
	byID map[uuid.UUID]*models.WorkItem
	err  error
}

func (f fakeItems) GetWorkItem(_ context.Context, id uuid.UUID) (*models.WorkItem, error) {
	if f.err != nil {
		return nil, f.err
	}
	it, ok := f.byID[id]
	if !ok {
		return nil, errors.New("work item " + id.String() + " not found")
	}
	return it, nil
}

func TestLoadReleasePolicy(t *testing.T) {
	ctx := context.Background()
	optIn := uuid.New()
	noOptIn := uuid.New()
	store := fakeProducts{byID: map[uuid.UUID]*products.Product{
		optIn:   {ID: optIn, PartialVerificationOptIn: true},
		noOptIn: {ID: noOptIn, PartialVerificationOptIn: false},
	}}

	cases := []struct {
		name    string
		store   products.PolicyStore
		id      uuid.UUID
		wantOpt bool
		wantErr bool
	}{
		{"opted in", store, optIn, true, false},
		{"not opted in", store, noOptIn, false, false},
		{"missing product fails closed", store, uuid.New(), false, true},
		{"store error fails closed", fakeProducts{err: errors.New("db down")}, optIn, false, true},
		{"nil store fails closed", nil, optIn, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			policy, err := products.LoadReleasePolicy(ctx, tc.store, tc.id)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if policy != (evidence.ReleasePolicy{PartialVerificationOptIn: tc.wantOpt}) {
				t.Fatalf("policy = %+v, want opt-in %v", policy, tc.wantOpt)
			}
		})
	}
}

func TestLoadReleasePolicyForItem(t *testing.T) {
	ctx := context.Background()
	optIn := uuid.New()
	noOptIn := uuid.New()
	store := fakeProducts{byID: map[uuid.UUID]*products.Product{
		optIn:   {ID: optIn, PartialVerificationOptIn: true},
		noOptIn: {ID: noOptIn, PartialVerificationOptIn: false},
	}}
	parentID := uuid.New()
	orphanParentID := uuid.New()
	items := fakeItems{byID: map[uuid.UUID]*models.WorkItem{
		parentID:       {ID: parentID, ProductID: &optIn},
		orphanParentID: {ID: orphanParentID},
	}}
	missingParent := uuid.New()

	cases := []struct {
		name    string
		items   products.WorkItemLookup
		item    *models.WorkItem
		wantOpt bool
		wantErr error // nil means "no error"; products.ErrNoProduct or errAny
	}{
		{"own product opted in", items, &models.WorkItem{ID: uuid.New(), ProductID: &optIn}, true, nil},
		{"own product not opted in", items, &models.WorkItem{ID: uuid.New(), ProductID: &noOptIn}, false, nil},
		{"inherits parent's product", items, &models.WorkItem{ID: uuid.New(), ParentID: &parentID}, true, nil},
		{"own product wins over parent", items, &models.WorkItem{ID: uuid.New(), ProductID: &noOptIn, ParentID: &parentID}, false, nil},
		{"no product anywhere", items, &models.WorkItem{ID: uuid.New()}, false, products.ErrNoProduct},
		{"parent without product", items, &models.WorkItem{ID: uuid.New(), ParentID: &orphanParentID}, false, products.ErrNoProduct},
		{"parent lookup fails", items, &models.WorkItem{ID: uuid.New(), ParentID: &missingParent}, false, errAny},
		{"parent lookup unavailable", nil, &models.WorkItem{ID: uuid.New(), ParentID: &parentID}, false, products.ErrNoProduct},
		{"nil item", items, nil, false, errAny},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			policy, err := products.LoadReleasePolicyForItem(ctx, store, tc.items, tc.item)
			switch {
			case tc.wantErr == nil && err != nil:
				t.Fatalf("unexpected err %v", err)
			case tc.wantErr == errAny && err == nil:
				t.Fatalf("expected an error")
			case tc.wantErr != nil && tc.wantErr != errAny && !errors.Is(err, tc.wantErr):
				t.Fatalf("err = %v, want errors.Is %v", err, tc.wantErr)
			}
			if policy != (evidence.ReleasePolicy{PartialVerificationOptIn: tc.wantOpt}) {
				t.Fatalf("policy = %+v, want opt-in %v", policy, tc.wantOpt)
			}
		})
	}
}

var errAny = errors.New("any error")
