package main

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

// Cross-repository contract edges (M2-06) on the work item detail, and the
// per-product contracts_strict toggle.
//
// An edge is a plan_task_dependencies row of kind 'contract' between two
// subtasks of the item: the consumer waits for the producer, and before the
// consumer's PR opens pr-coordinator checks the contracts and records the
// result in contract_check.

// contractEdgeView is one contract edge with its subtasks' titles and
// repositories and its last check (null until pr-coordinator ran one).
type contractEdgeView struct {
	ConsumerTaskID     string          `json:"consumer_task_id"`
	ConsumerTitle      string          `json:"consumer_title"`
	ConsumerRepository string          `json:"consumer_repository"`
	ProducerTaskID     string          `json:"producer_task_id"`
	ProducerTitle      string          `json:"producer_title"`
	ProducerRepository string          `json:"producer_repository"`
	Contracts          json.RawMessage `json:"contracts"`
	LastCheck          json.RawMessage `json:"last_check"`
}

const contractEdgesSQL = `
SELECT d.task_id::text, c.title, COALESCE(rc.name, ''),
       d.depends_on_task_id::text, p.title, COALESCE(rp.name, ''),
       COALESCE(d.contract_ref, '[]'::jsonb), d.contract_check
  FROM plan_task_dependencies d
  JOIN plan_tasks c ON c.id = d.task_id
  JOIN plan_tasks p ON p.id = d.depends_on_task_id
  LEFT JOIN repositories rc ON rc.id = c.repository_id
  LEFT JOIN repositories rp ON rp.id = p.repository_id
 WHERE c.parent_work_id = $1 AND d.kind = 'contract'
 ORDER BY 6, 3, d.task_id`

// loadContractEdges returns the item's contract edges. A query error (for
// instance a database before migration 056) is logged and yields an empty
// array: the detail page must still render.
func loadContractEdges(ctx context.Context, db rowQuerier, id uuid.UUID) []contractEdgeView {
	out := []contractEdgeView{}
	if db == nil {
		return out
	}
	rows, err := db.Query(ctx, contractEdgesSQL, id)
	if err != nil {
		log.Printf("handlers: get contract edges for %s: %v", id, err)
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var e contractEdgeView
		var refs, check []byte
		if err := rows.Scan(&e.ConsumerTaskID, &e.ConsumerTitle, &e.ConsumerRepository,
			&e.ProducerTaskID, &e.ProducerTitle, &e.ProducerRepository, &refs, &check); err != nil {
			log.Printf("handlers: scan contract edge for %s: %v", id, err)
			continue
		}
		e.Contracts = json.RawMessage(refs)
		if len(check) > 0 {
			e.LastCheck = json.RawMessage(check)
		} else {
			e.LastCheck = json.RawMessage("null")
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		log.Printf("handlers: read contract edges for %s: %v", id, err)
	}
	return out
}

// errContractsColumnMissing: the database predates migration 056.
var errContractsColumnMissing = errors.New("products.contracts_strict column is missing; apply migration 056")

// ContractsStrictStore persists products.contracts_strict. Narrow so the
// handler can be unit-tested with a fake.
type ContractsStrictStore interface {
	SetContractsStrict(ctx context.Context, productID uuid.UUID, strict bool) error
}

func (s *pgProductPolicyStore) SetContractsStrict(ctx context.Context, productID uuid.UUID, strict bool) error {
	tag, err := s.db.Exec(ctx,
		`UPDATE products SET contracts_strict = $1, updated_at = NOW() WHERE id = $2`, strict, productID)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == pgUndefinedColumn {
			return errContractsColumnMissing
		}
		return err
	}
	if tag.RowsAffected() == 0 {
		return errProductNotFound
	}
	return nil
}

// setProductContractsStrictRequest: strict is a pointer so an omitted field
// is a 400, not a silent false.
type setProductContractsStrictRequest struct {
	ProductID string `json:"product_id"`
	Strict    *bool  `json:"strict"`
}

// SetProductContractsStrict handles POST /api/admin/products/contracts-strict
// {product_id, strict}.
func (h *AdminHandlers) SetProductContractsStrict(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req setProductContractsStrictRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	productID, err := uuid.Parse(req.ProductID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid product_id")
		return
	}
	if req.Strict == nil {
		writeError(w, http.StatusBadRequest, "strict is required")
		return
	}
	if h.contractsPolicy == nil {
		writeError(w, http.StatusInternalServerError, "product policy store not configured")
		return
	}
	if err := h.contractsPolicy.SetContractsStrict(r.Context(), productID, *req.Strict); err != nil {
		switch {
		case errors.Is(err, errProductNotFound):
			writeError(w, http.StatusNotFound, "product not found")
		case errors.Is(err, errContractsColumnMissing):
			log.Printf("admin: set contracts_strict for %s: %v", productID, err)
			writeError(w, http.StatusServiceUnavailable, err.Error())
		default:
			log.Printf("admin: set contracts_strict for %s: %v", productID, err)
			writeError(w, http.StatusInternalServerError, "failed to update contracts_strict")
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":           "ok",
		"product_id":       productID.String(),
		"contracts_strict": *req.Strict,
	})
}
