package checks

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// EdgeQuerier is the pool surface LoadContractEdges needs (*pgxpool.Pool
// satisfies it).
type EdgeQuerier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// LoadContractEdges returns the codex_contract_edges rows whose producer is
// repoID (migration 026), each naming its consumer repository by name when
// the repositories row exists and by id otherwise. Edges whose consumer is
// the producer itself are excluded: in-repository dependencies are the
// language impact's job.
func LoadContractEdges(ctx context.Context, db EdgeQuerier, repoID uuid.UUID) ([]ContractEdge, error) {
	rows, err := db.Query(ctx, `
		SELECT ce.contract_type, ce.contract_name, ce.producer_file,
		       COALESCE(r.name, ce.consumer_repo_id::text), ce.consumer_file
		  FROM codex_contract_edges ce
		  LEFT JOIN repositories r ON r.id = ce.consumer_repo_id
		 WHERE ce.producer_repo_id = $1 AND ce.consumer_repo_id <> ce.producer_repo_id
		 ORDER BY ce.producer_file, ce.contract_name, 4, ce.consumer_file`, repoID)
	if err != nil {
		return nil, fmt.Errorf("load contract edges: %w", err)
	}
	defer rows.Close()
	var out []ContractEdge
	for rows.Next() {
		var e ContractEdge
		if err := rows.Scan(&e.ContractType, &e.ContractName, &e.ProducerFile, &e.ConsumerRepo, &e.ConsumerFile); err != nil {
			return nil, fmt.Errorf("scan contract edge: %w", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("contract edges: %w", err)
	}
	return out, nil
}
