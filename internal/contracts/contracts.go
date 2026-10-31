// Package contracts is the cross-repository contract model and its
// deterministic compatibility checkers (M2-06).
//
// A contract edge is a dependency between two repository-scoped subtasks of
// one parent work item: the consumer subtask depends on the producer subtask
// (plan_task_dependencies.kind = 'contract', migration 056), and the edge
// carries Refs naming the contracts. The planner derives edges from
// codex_contract_edges and from the criteria's Contracts; M1-06 holds the
// consumer until the producer completes. Before pr-coordinator opens the
// CONSUMER's pull request it materialises the producer's pinned base, the
// producer's head and the consumer's head and calls Check.
//
// Check never passes silently: a contract it cannot evaluate (a type with no
// checker, a format no checker parses, a producer that does not define the
// contract where the checker looks) is StatusUnchecked with the reason, which
// pr-coordinator records and blocks on only under products.contracts_strict.
package contracts

import (
	"context"
	"fmt"
	"strings"
)

// Contract types (Ref.Type).
const (
	TypeREST          = "rest"
	TypeEvent         = "event"
	TypeDBSchema      = "db_schema"
	TypeSharedPackage = "shared_package"
	TypeOther         = "other"
)

// Edge sources (Ref.Source).
const (
	SourceCodex    = "codex"
	SourceCriteria = "criteria"
)

// Ref names one contract between a producer and a consumer repository. It is
// stored, as one element of a JSON array, in
// plan_task_dependencies.contract_ref and carried in the subtask's
// implementation.tasks message.
type Ref struct {
	Type           string `json:"type"`
	Name           string `json:"name"`
	ProducerRepoID string `json:"producer_repo_id"`
	ConsumerRepoID string `json:"consumer_repo_id"`
	Source         string `json:"source"`
	// Codex provenance (Source = codex): where the extractor saw each side.
	ProducerFile string `json:"producer_file,omitempty"`
	ProducerLine int    `json:"producer_line,omitempty"`
	ConsumerFile string `json:"consumer_file,omitempty"`
	ConsumerLine int    `json:"consumer_line,omitempty"`
	// CriterionID is the criterion that declared it (Source = criteria).
	CriterionID string `json:"criterion_id,omitempty"`
}

// Key identifies a contract independently of where it was seen: two Refs
// with one Key are the same contract between the same repositories.
func (r Ref) Key() string {
	return strings.Join([]string{r.Type, strings.ToLower(strings.TrimSpace(r.Name)), r.ProducerRepoID, r.ConsumerRepoID}, "|")
}

// Label is how messages name the contract: `rest "GET /api/users/{id}"`.
func (r Ref) Label() string {
	return fmt.Sprintf("%s %q", r.Type, r.Name)
}

// NormalizeType maps a codex contract_type or a criterion's contract type
// (the extractor families: rest_openapi, graphql, grpc_protobuf, events,
// sql_schemas, shared_packages, go_interfaces, ts_exports, python_exports,
// or their record types: rest.route, event.topic, sql.table, ...) to one of
// the five edge types.
func NormalizeType(s string) string {
	t := strings.ToLower(strings.TrimSpace(s))
	switch {
	case t == "":
		return TypeOther
	case t == TypeREST || strings.Contains(t, "rest") || strings.Contains(t, "openapi") || strings.Contains(t, "http") || strings.Contains(t, "route"):
		return TypeREST
	case t == TypeEvent || strings.Contains(t, "event") || strings.Contains(t, "topic") || strings.Contains(t, "kafka"):
		return TypeEvent
	case t == TypeDBSchema || strings.Contains(t, "sql") || strings.Contains(t, "schema") || strings.Contains(t, "table") || t == "db" || strings.HasPrefix(t, "db_") || strings.HasPrefix(t, "db."):
		return TypeDBSchema
	case t == TypeSharedPackage || strings.Contains(t, "package") || strings.Contains(t, "export") || strings.Contains(t, "interface"):
		return TypeSharedPackage
	}
	return TypeOther
}

// Status is a check's verdict.
type Status string

const (
	// StatusOK: the checker evaluated the contract and found nothing the
	// consumer relies on removed or retyped by the producer.
	StatusOK Status = "ok"
	// StatusMismatch: the producer's change breaks a reference the consumer
	// makes; Mismatches names each one.
	StatusMismatch Status = "mismatch"
	// StatusUnchecked: the contract could not be evaluated (no checker for
	// its type, a format the checker does not parse, no producer base to
	// diff against). Recorded, never a pass.
	StatusUnchecked Status = "unchecked"
)

// Mismatch is one break: what the producer changed and where the consumer
// relies on what it changed.
type Mismatch struct {
	// ProducerChange: "removed field email from GET /api/users/{id} (openapi.json)".
	ProducerChange string `json:"producer_change"`
	// ConsumerRef: "src/client.ts:12: return body.email".
	ConsumerRef string `json:"consumer_ref"`
}

// Result is what Check found for one contract.
type Result struct {
	Contract   Ref        `json:"contract"`
	Status     Status     `json:"status"`
	Details    []string   `json:"details,omitempty"`
	Mismatches []Mismatch `json:"mismatches,omitempty"`
}

// Summary is one line for a PR description, a log or a block reason.
func (r Result) Summary() string {
	switch r.Status {
	case StatusMismatch:
		parts := make([]string, 0, len(r.Mismatches))
		for _, m := range r.Mismatches {
			parts = append(parts, fmt.Sprintf("the producer %s, but the consumer still uses it at %s", m.ProducerChange, m.ConsumerRef))
		}
		return fmt.Sprintf("contract %s: %s", r.Contract.Label(), strings.Join(parts, "; "))
	case StatusUnchecked:
		return fmt.Sprintf("contract %s: unchecked (%s)", r.Contract.Label(), strings.Join(r.Details, "; "))
	}
	if len(r.Details) > 0 {
		return fmt.Sprintf("contract %s: ok (%s)", r.Contract.Label(), strings.Join(r.Details, "; "))
	}
	return fmt.Sprintf("contract %s: ok", r.Contract.Label())
}

// Side is one repository's materialised trees: directories holding the
// files of a commit (a git archive extracted, or a fixture). Base is the
// work item's pinned base of that repository, "" when it is unpinned; Head
// is the subtask's verified head.
type Side struct {
	Base string
	Head string
}

// Check evaluates one contract over the producer's base and head and the
// consumer's head. It reads only the directories it is given.
func Check(ctx context.Context, edge Ref, producer, consumer Side) Result {
	res := Result{Contract: edge}
	if strings.TrimSpace(edge.Name) == "" {
		return unchecked(res, "the contract has no name")
	}
	if producer.Head == "" || consumer.Head == "" {
		return unchecked(res, "the producer or consumer head is not available")
	}
	if err := ctx.Err(); err != nil {
		return unchecked(res, err.Error())
	}
	switch NormalizeType(edge.Type) {
	case TypeREST:
		return checkREST(ctx, res, producer, consumer)
	case TypeEvent:
		return checkEvent(ctx, res, producer, consumer)
	case TypeDBSchema:
		return checkDBSchema(ctx, res, producer, consumer)
	}
	return unchecked(res, fmt.Sprintf("no checker for contract type %q", edge.Type))
}

func unchecked(r Result, why string) Result {
	r.Status = StatusUnchecked
	r.Details = append(r.Details, why)
	r.Mismatches = nil
	return r
}

// finish settles the status: any mismatch wins; otherwise the status the
// checker set (unchecked) or ok.
func finish(r Result) Result {
	if len(r.Mismatches) > 0 {
		r.Status = StatusMismatch
		return r
	}
	if r.Status == "" {
		r.Status = StatusOK
	}
	return r
}
