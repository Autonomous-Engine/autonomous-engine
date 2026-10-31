package rollout

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/autonomousengine/autonomous-engine/internal/configsnap"
)

// UnitAgentType is the agent type of the rollout unit: the
// implementation-agent snapshot recorded per attempt (M4-05 spec, "Rollout
// unit").
const UnitAgentType = "implementation-agent"

// Capture of the rollout unit for a definition.
//
// The model is resolved the way agentregistry.Resolve resolves it: the
// definition's own provider/model when it names both, otherwise the global
// default. The global default is read from the default enabled llm_providers
// row (provider_type, endpoint, settings.model) — the row the dashboard
// mirrors into the global credential_registry keys the agents read
// (provider_sync.go) — so the capture can run inside a caller's transaction
// and see an uncommitted change. An agent-type-scoped credential override
// for implementation-agent is not seen here; shadow attribution (Attribute)
// catches the difference, and the guard then simply finds no approval.
//
// codex_version is recorded as 0 (no work item): candidates and the guard
// compare captures made the same way, and Equivalent ignores codex_version
// when matching an attempt's snapshot to them.
const (
	defaultDefinitionSQL = `SELECT id FROM agent_definitions WHERE is_default AND enabled ORDER BY name, id LIMIT 1`
	definitionModelSQL   = `SELECT provider, model_id, model_endpoint FROM agent_definitions WHERE id = $1`
	globalModelSQL       = `SELECT provider_type, endpoint, COALESCE(settings->>'model', '') FROM llm_providers
		WHERE is_default AND enabled ORDER BY name, id LIMIT 1`
)

// DefaultDefinitionID returns the production default definition (nil when
// there is none: the full pipeline runs without a definition).
func DefaultDefinitionID(ctx context.Context, q configsnap.Querier) (*uuid.UUID, error) {
	var id uuid.UUID
	err := q.QueryRow(ctx, defaultDefinitionSQL).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("rollout: default definition: %w", err)
	}
	return &id, nil
}

// UnitInput returns the configsnap input of the rollout unit for defID (nil
// = no definition).
func UnitInput(ctx context.Context, q configsnap.Querier, defID *uuid.UUID) (configsnap.Input, error) {
	in := configsnap.Input{AgentType: UnitAgentType, AgentDefinitionID: defID}
	if defID != nil {
		var provider, model, endpoint string
		err := q.QueryRow(ctx, definitionModelSQL, *defID).Scan(&provider, &model, &endpoint)
		if errors.Is(err, pgx.ErrNoRows) {
			return in, fmt.Errorf("rollout: agent definition %s not found", defID)
		}
		if err != nil {
			return in, fmt.Errorf("rollout: definition model: %w", err)
		}
		if provider != "" && model != "" {
			in.Model = configsnap.Model{Provider: provider, ModelID: model, Endpoint: endpoint, Source: configsnap.SourceAgentDefinition}
			return in, nil
		}
	}
	var provider, endpoint, model string
	err := q.QueryRow(ctx, globalModelSQL).Scan(&provider, &endpoint, &model)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return in, fmt.Errorf("rollout: global model: %w", err)
	}
	if o, ok := ctx.Value(overlayKey{}).(GlobalModelOverlay); ok {
		if o.Provider != nil {
			provider = *o.Provider
		}
		if o.ModelID != nil {
			model = *o.ModelID
		}
		if o.Endpoint != nil {
			endpoint = *o.Endpoint
		}
	}
	in.Model = configsnap.Model{Provider: provider, ModelID: model, Endpoint: endpoint, Source: configsnap.SourceGlobalDefault}
	return in, nil
}

// GlobalModelOverlay replaces parts of the global default model a capture
// sees. The guard uses it for a credential write (llm provider /
// model_name / endpoint at global or implementation-agent scope): the
// values are encrypted, so the change cannot be applied in SQL; the
// "after" surface is captured with the new value overlaid instead.
type GlobalModelOverlay struct {
	Provider *string
	ModelID  *string
	Endpoint *string
}

type overlayKey struct{}

// WithGlobalModelOverlay returns ctx carrying o for captures made with it.
func WithGlobalModelOverlay(ctx context.Context, o GlobalModelOverlay) context.Context {
	return context.WithValue(ctx, overlayKey{}, o)
}

// OverlayForCredential returns the overlay a credential write produces, or
// nil when the key does not shape the rollout unit's global model.
func OverlayForCredential(category, keyName, value string) *GlobalModelOverlay {
	if category != "llm" {
		return nil
	}
	v := value
	switch keyName {
	case "provider":
		return &GlobalModelOverlay{Provider: &v}
	case "model_name":
		return &GlobalModelOverlay{ModelID: &v}
	case "endpoint":
		return &GlobalModelOverlay{Endpoint: &v}
	}
	return nil
}

// CredentialShapesUnit reports whether a credential at scope/target is the
// one implementation-agent resolves for keyName: global/default unless a
// more specific row (agent_instance/instance-1, agent_type/
// implementation-agent, integration) exists, or agent_type/
// implementation-agent unless an agent_instance/instance-1 row exists
// (configstore.ResolveCredential's chain). Values are not read.
func CredentialShapesUnit(ctx context.Context, q configsnap.Querier, scope, target, category, keyName string) (bool, error) {
	if category != "llm" || (keyName != "provider" && keyName != "model_name" && keyName != "endpoint") {
		return false, nil
	}
	var more string
	switch {
	case scope == "global" && target == "default":
		more = `(scope = 'agent_instance' AND scope_target = 'instance-1') OR (scope = 'agent_type' AND scope_target = $1) OR scope = 'integration'`
	case scope == "agent_type" && target == UnitAgentType:
		more = `(scope = 'agent_instance' AND scope_target = 'instance-1' AND $1::text <> '')`
	default:
		return false, nil
	}
	var shadowed bool
	err := q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM credential_registry
		WHERE category = 'llm' AND key_name = $2 AND (`+more+`))`, UnitAgentType, keyName).Scan(&shadowed)
	if err != nil {
		return false, fmt.Errorf("rollout: credential chain: %w", err)
	}
	return !shadowed, nil
}

// CaptureUnit captures (without storing) the rollout unit for defID and
// returns its id and document.
func CaptureUnit(ctx context.Context, q configsnap.Querier, defID *uuid.UUID) (string, configsnap.Document, error) {
	in, err := UnitInput(ctx, q, defID)
	if err != nil {
		return "", nil, err
	}
	doc, err := configsnap.Capture(ctx, q, in)
	if err != nil {
		return "", nil, err
	}
	id, _, err := configsnap.Hash(doc)
	if err != nil {
		return "", nil, err
	}
	return id, doc, nil
}

// ProductionSnapshot captures the production default's rollout unit: what
// an implementation attempt would run under now with the default
// definition. q may be a transaction holding an uncommitted change.
func ProductionSnapshot(ctx context.Context, q configsnap.Querier) (string, configsnap.Document, error) {
	defID, err := DefaultDefinitionID(ctx, q)
	if err != nil {
		return "", nil, err
	}
	return CaptureUnit(ctx, q, defID)
}

// Equivalent reports whether two snapshot documents describe the same
// configuration apart from codex_version (a property of the work item the
// snapshot was captured for, not of the configuration).
func Equivalent(a, b configsnap.Document) bool {
	ca, err := canonicalWithoutCodex(a)
	if err != nil {
		return false
	}
	cb, err := canonicalWithoutCodex(b)
	if err != nil {
		return false
	}
	return bytes.Equal(ca, cb)
}

func canonicalWithoutCodex(d configsnap.Document) ([]byte, error) {
	if d == nil {
		return nil, fmt.Errorf("nil document")
	}
	cp := make(map[string]any, len(d))
	for k, v := range d {
		if k == "codex_version" {
			continue
		}
		cp[k] = v
	}
	return configsnap.Canonicalize(cp)
}

// snapshotSummary is the part of a stored snapshot attribution needs.
type snapshotSummary struct {
	agentType string
	defID     string
	doc       configsnap.Document
}

func loadSnapshot(ctx context.Context, q DB, id string) (*snapshotSummary, error) {
	var agentType string
	var defID *uuid.UUID
	var raw []byte
	err := q.QueryRow(ctx, `SELECT agent_type, agent_definition_id, document FROM config_snapshots WHERE id = $1`, id).Scan(&agentType, &defID, &raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("rollout: load snapshot %s: %w", id, err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var doc configsnap.Document
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("rollout: decode snapshot %s: %w", id, err)
	}
	s := &snapshotSummary{agentType: agentType, doc: doc}
	if defID != nil {
		s.defID = defID.String()
	}
	return s, nil
}

// RecordRef is one benchmark result record as attribution sees it: which
// side ran it and the work item's recorded snapshot (the wrapper's
// versions.config_snapshot_id; "" when unknown).
type RecordRef struct {
	TaskID     string
	Trial      int
	Side       string // "candidate" or "baseline"
	SnapshotID string
}

// Attribute checks every record against the snapshot its side requested.
// A record is attributed when its snapshot is Equivalent to the requested
// one, or — for an item that never reached implementation — when it is
// the intake pipeline snapshot (agent_type "*") of the requested
// snapshot's agent definition. Unknown snapshots are not attributed (fail
// closed).
func Attribute(ctx context.Context, q DB, candidateID, baselineID string, records []RecordRef) (Attribution, error) {
	var a Attribution
	cache := map[string]*snapshotSummary{}
	load := func(id string) (*snapshotSummary, error) {
		if s, ok := cache[id]; ok {
			return s, nil
		}
		s, err := loadSnapshot(ctx, q, id)
		if err != nil {
			return nil, err
		}
		cache[id] = s
		return s, nil
	}
	want := map[string]string{"candidate": candidateID, "baseline": baselineID}
	for _, r := range records {
		requestedID, ok := want[r.Side]
		if !ok {
			continue
		}
		if r.Side == "candidate" {
			a.CandidateRecords++
		} else {
			a.BaselineRecords++
		}
		attributed, problem, err := attributeOne(load, requestedID, r.SnapshotID)
		if err != nil {
			return a, err
		}
		if attributed {
			if r.Side == "candidate" {
				a.CandidateAttributed++
			} else {
				a.BaselineAttributed++
			}
			continue
		}
		if len(a.Problems) < 20 {
			a.Problems = append(a.Problems, fmt.Sprintf("%s %s trial %d: %s", r.Side, r.TaskID, r.Trial, problem))
		}
	}
	return a, nil
}

func attributeOne(load func(string) (*snapshotSummary, error), requestedID, recordedID string) (bool, string, error) {
	if requestedID == "" {
		return false, "no requested snapshot for this side", nil
	}
	if recordedID == "" {
		return false, "the work item recorded no configuration snapshot", nil
	}
	if recordedID == requestedID {
		return true, "", nil
	}
	req, err := load(requestedID)
	if err != nil {
		return false, "", err
	}
	if req == nil {
		return false, "requested snapshot " + short(requestedID) + " is not stored", nil
	}
	rec, err := load(recordedID)
	if err != nil {
		return false, "", err
	}
	if rec == nil {
		return false, "recorded snapshot " + short(recordedID) + " is not stored", nil
	}
	if rec.agentType == configsnap.PipelineAgentType {
		if rec.defID == req.defID {
			return true, "", nil
		}
		return false, fmt.Sprintf("intake routed to definition %q, requested %q", rec.defID, req.defID), nil
	}
	if Equivalent(rec.doc, req.doc) {
		return true, "", nil
	}
	return false, "ran under snapshot " + short(recordedID) + ", requested " + short(requestedID), nil
}

func short(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}
