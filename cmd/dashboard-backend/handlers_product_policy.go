package main

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ─── Product verification policy ─────────────────────────────────────────────
//
// Partially verified work is blocked by default. A
// product owner may opt a product in, after which partially_verified
// implementations advance only to mandatory human review; they are never
// treated as verified or automatically released. The flag lives in
// products.partial_verification_opt_in (migration 045).

var (
	errProductNotFound = errors.New("product not found")
	// errPolicyColumnMissing is returned when the database predates migration
	// 045. The handler surfaces it as an operator-readable 503 rather than a
	// generic 500 so the fix (run the migration) is obvious.
	errPolicyColumnMissing = errors.New("products.partial_verification_opt_in column is missing; apply migration 045")
)

// ProductPolicyStore is the narrow persistence surface for the opt-in flag.
// Narrow so the handler can be unit-tested with a fake.
type ProductPolicyStore interface {
	SetPartialVerificationOptIn(ctx context.Context, productID uuid.UUID, optIn bool) error
}

// pgProductPolicyStore is the pgx-backed ProductPolicyStore.
type pgProductPolicyStore struct {
	db *pgxpool.Pool
}

// pgUndefinedColumn is the PostgreSQL SQLSTATE for "column does not exist".
const pgUndefinedColumn = "42703"

func (s *pgProductPolicyStore) SetPartialVerificationOptIn(ctx context.Context, productID uuid.UUID, optIn bool) error {
	tag, err := s.db.Exec(ctx,
		`UPDATE products SET partial_verification_opt_in = $1, updated_at = NOW() WHERE id = $2`,
		optIn, productID)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == pgUndefinedColumn {
			return errPolicyColumnMissing
		}
		return err
	}
	if tag.RowsAffected() == 0 {
		return errProductNotFound
	}
	return nil
}

// setProductPartialVerificationRequest is the JSON body for
// SetProductPartialVerification. opt_in is a pointer so an omitted field is a
// 400 instead of a silent opt-out.
type setProductPartialVerificationRequest struct {
	ProductID string `json:"product_id"`
	OptIn     *bool  `json:"opt_in"`
}

// SetProductPartialVerification handles POST /api/admin/products/partial-verification
// {product_id, opt_in}. Flips products.partial_verification_opt_in.
func (h *AdminHandlers) SetProductPartialVerification(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	var req setProductPartialVerificationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if req.ProductID == "" {
		writeError(w, http.StatusBadRequest, "product_id is required")
		return
	}
	productID, err := uuid.Parse(req.ProductID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid product_id")
		return
	}
	if req.OptIn == nil {
		writeError(w, http.StatusBadRequest, "opt_in is required")
		return
	}

	if h.productPolicy == nil {
		log.Printf("admin: set partial verification opt-in for %s: policy store not configured", productID)
		writeError(w, http.StatusInternalServerError, "product policy store not configured")
		return
	}

	if err := h.productPolicy.SetPartialVerificationOptIn(r.Context(), productID, *req.OptIn); err != nil {
		switch {
		case errors.Is(err, errProductNotFound):
			writeError(w, http.StatusNotFound, "product not found")
		case errors.Is(err, errPolicyColumnMissing):
			log.Printf("admin: set partial verification opt-in for %s: %v", productID, err)
			writeError(w, http.StatusServiceUnavailable, err.Error())
		default:
			log.Printf("admin: set partial verification opt-in for %s: %v", productID, err)
			writeError(w, http.StatusInternalServerError, "failed to update partial_verification_opt_in")
		}
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"status":                      "ok",
		"product_id":                  productID.String(),
		"partial_verification_opt_in": *req.OptIn,
	})
}
