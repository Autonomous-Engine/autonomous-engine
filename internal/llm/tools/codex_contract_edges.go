package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/autonomousengine/autonomous-engine/internal/llm/adapter"
)

// codexContractEdges queries the codex_contract_edges table for edges
// matching a contract name, scoped to the agent's product via the
// repositories table. Direction filters to producer, consumer, or both.
func (e *Executor) codexContractEdges(ctx context.Context, callID string, args map[string]interface{}) adapter.ToolResult {
	if e.codex == nil || e.productID == nil {
		return e.codexMissingResult(callID, "codex_contract_edges")
	}

	contractName, _ := args["contract_name"].(string)
	if strings.TrimSpace(contractName) == "" {
		return adapter.ToolResult{ToolCallID: callID, Content: "contract_name is required", IsError: true}
	}
	direction, _ := args["direction"].(string)
	if direction == "" {
		direction = "both"
	}
	if direction != "producer" && direction != "consumer" && direction != "both" {
		return adapter.ToolResult{
			ToolCallID: callID,
			Content:    fmt.Sprintf("direction must be \"producer\", \"consumer\", or \"both\"; got %q", direction),
			IsError:    true,
		}
	}

	pool := e.codex.Pool()
	if pool == nil {
		return adapter.ToolResult{ToolCallID: callID, Content: "codex_contract_edges: database pool unavailable", IsError: true}
	}

	// Build the query based on direction. All queries are scoped to
	// repos belonging to the executor's product.
	var query string
	switch direction {
	case "producer":
		query = `
			SELECT ce.hash, ce.contract_type, ce.contract_name,
			       ce.producer_file, ce.producer_line,
			       ce.consumer_file, ce.consumer_line,
			       ce.confidence
			FROM codex_contract_edges ce
			JOIN repositories r ON ce.producer_repo_id = r.id
			WHERE r.product_id = $1 AND ce.contract_name = $2`
	case "consumer":
		query = `
			SELECT ce.hash, ce.contract_type, ce.contract_name,
			       ce.producer_file, ce.producer_line,
			       ce.consumer_file, ce.consumer_line,
			       ce.confidence
			FROM codex_contract_edges ce
			JOIN repositories r ON ce.consumer_repo_id = r.id
			WHERE r.product_id = $1 AND ce.contract_name = $2`
	default: // "both"
		query = `
			SELECT ce.hash, ce.contract_type, ce.contract_name,
			       ce.producer_file, ce.producer_line,
			       ce.consumer_file, ce.consumer_line,
			       ce.confidence
			FROM codex_contract_edges ce
			JOIN repositories r ON (ce.producer_repo_id = r.id OR ce.consumer_repo_id = r.id)
			WHERE r.product_id = $1 AND ce.contract_name = $2`
	}

	rows, err := pool.Query(ctx, query, *e.productID, contractName)
	if err != nil {
		return adapter.ToolResult{ToolCallID: callID, Content: "codex_contract_edges: " + err.Error(), IsError: true}
	}
	defer rows.Close()

	type edge struct {
		Hash         string  `json:"hash"`
		ContractType string  `json:"contract_type"`
		ContractName string  `json:"contract_name"`
		ProducerFile string  `json:"producer_file"`
		ProducerLine *int    `json:"producer_line,omitempty"`
		ConsumerFile string  `json:"consumer_file"`
		ConsumerLine *int    `json:"consumer_line,omitempty"`
		Confidence   float64 `json:"confidence"`
	}

	var edges []edge
	for rows.Next() {
		var ed edge
		if err := rows.Scan(
			&ed.Hash, &ed.ContractType, &ed.ContractName,
			&ed.ProducerFile, &ed.ProducerLine,
			&ed.ConsumerFile, &ed.ConsumerLine,
			&ed.Confidence,
		); err != nil {
			return adapter.ToolResult{ToolCallID: callID, Content: "codex_contract_edges: scan: " + err.Error(), IsError: true}
		}
		edges = append(edges, ed)
	}
	if err := rows.Err(); err != nil {
		return adapter.ToolResult{ToolCallID: callID, Content: "codex_contract_edges: rows: " + err.Error(), IsError: true}
	}

	payload := map[string]interface{}{
		"contract_name": contractName,
		"direction":     direction,
		"edges":         edges,
		"count":         len(edges),
	}
	data, _ := json.MarshalIndent(payload, "", "  ")
	if len(edges) == 0 {
		e.markCodexEmpty("codex_contract_edges", contractName)
	}
	return adapter.ToolResult{ToolCallID: callID, Content: string(data)}
}
