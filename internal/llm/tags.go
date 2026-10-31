package llm

import (
	"context"
	"strings"

	"github.com/google/uuid"

	"github.com/autonomousengine/autonomous-engine/internal/agentregistry"
)

// Inference roles (inference_logs.role, migration 053). Every model call is
// tagged with the role its caller plays so cost and outcomes can be split
// by author / reviewer / planner spend.
const (
	RoleAuthor   = "author"
	RoleVerifier = "verifier"
	RoleReviewer = "reviewer"
	RolePlanner  = "planner"
	RoleResearch = "research"
	RoleQA       = "qa"
	RoleOther    = "other"
)

// RoleForAgentType maps a service's agent type to its inference role.
// Unknown agent types are "other" (never NULL), so a new service is
// tagged, if coarsely, from its first call.
func RoleForAgentType(agentType string) string {
	switch agentType {
	case "implementation-agent":
		return RoleAuthor
	case "review-agent":
		return RoleReviewer
	case "qa-agent":
		return RoleQA
	case "verifier":
		return RoleVerifier
	case "implementation-planner", "product-agent":
		return RolePlanner
	case "research-agent", "research-coordinator":
		return RoleResearch
	case "":
		return ""
	default:
		return RoleOther
	}
}

// InferenceTags are written on every inference_logs row a call produces.
// Empty fields are written as NULL (Provider/ModelID: the writer's own
// fallback).
type InferenceTags struct {
	Role             string
	AgentType        string
	ConfigSnapshotID string
	// Provider and ModelID are the resolved model, written to
	// model_provider / model_id so the price join (inference_log_costs)
	// matches without parsing the adapter name.
	Provider string
	ModelID  string
	// WorkItemID is used when the call itself names no work item.
	WorkItemID *uuid.UUID
}

// merge returns t with every empty field taken from base.
func (t InferenceTags) merge(base InferenceTags) InferenceTags {
	if t.Role == "" {
		t.Role = base.Role
	}
	if t.AgentType == "" {
		t.AgentType = base.AgentType
	}
	if t.ConfigSnapshotID == "" {
		t.ConfigSnapshotID = base.ConfigSnapshotID
	}
	if t.Provider == "" {
		t.Provider = base.Provider
	}
	if t.ModelID == "" {
		t.ModelID = base.ModelID
	}
	if t.WorkItemID == nil {
		t.WorkItemID = base.WorkItemID
	}
	return t
}

type tagsKey struct{}

// WithInferenceTags returns ctx carrying t. Calls made with the returned
// context (Service.Execute, tools.RunToolLoop) write t on their
// inference_logs rows; fields left empty fall back to the tags of an outer
// WithInferenceTags and then to the Service's own.
func WithInferenceTags(ctx context.Context, t InferenceTags) context.Context {
	return context.WithValue(ctx, tagsKey{}, t.merge(InferenceTagsFrom(ctx)))
}

// InferenceTagsFrom returns the tags ctx carries (zero value when none).
func InferenceTagsFrom(ctx context.Context) InferenceTags {
	if ctx == nil {
		return InferenceTags{}
	}
	t, _ := ctx.Value(tagsKey{}).(InferenceTags)
	return t
}

// TagsForResolution returns the tags of calls made with a registry
// resolution: role from the agent type, the snapshot, the resolved model.
func TagsForResolution(res *agentregistry.Resolution, workItemID uuid.UUID) InferenceTags {
	if res == nil {
		return InferenceTags{}
	}
	t := InferenceTags{
		Role:             RoleForAgentType(res.AgentType),
		AgentType:        res.AgentType,
		ConfigSnapshotID: res.SnapshotID,
		Provider:         res.Provider,
		ModelID:          res.ModelID,
	}
	if workItemID != uuid.Nil {
		id := workItemID
		t.WorkItemID = &id
	}
	return t
}

// SplitAdapterName splits an adapter name ("anthropic/claude-x") into its
// provider and model. A name without a slash is all provider.
func SplitAdapterName(name string) (provider, model string) {
	if i := strings.Index(name, "/"); i > 0 {
		return name[:i], name[i+1:]
	}
	return name, ""
}

// ModelIdentity returns the model_provider and model_id to write for a
// call made through an adapter named adapterName: the tags' resolved model
// when set, otherwise the adapter name split at its slash.
func ModelIdentity(adapterName string, tags InferenceTags) (provider, modelID string) {
	if tags.Provider != "" && tags.ModelID != "" {
		return tags.Provider, tags.ModelID
	}
	return SplitAdapterName(adapterName)
}

// nullIfEmpty maps "" to a SQL NULL.
func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// NullIfEmpty maps "" to a SQL NULL (for other inference_logs writers).
func NullIfEmpty(s string) any { return nullIfEmpty(s) }
