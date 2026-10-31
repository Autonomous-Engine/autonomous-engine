package main

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"

	"github.com/autonomousengine/autonomous-engine/internal/zones"
)

// ─── Codex zones (migration 063) ─────────────────────────────────────────────
//
// Admin → Products → Repository → Zones. Every write goes through
// zones.Store, which records a codex version with section:zones for the
// repository's product on its base branch in the same transaction.
//
//	GET    /api/admin/repositories/zones?id=<repo>      -> []zones.Zone
//	POST   /api/admin/repositories/zones?id=<repo>      {name, description, patterns, policy} -> 201 zones.Zone
//	PUT    /api/admin/zones?id=<zone>                   {patterns?, policy?, description?}    -> zones.Zone
//	DELETE /api/admin/zones?id=<zone>                   -> 204
//	POST   /api/admin/zones/freeze?id=<zone>            {reason}                              -> zones.Zone
//	POST   /api/admin/zones/thaw?id=<zone>              -> zones.Zone

func (h *AdminHandlers) zoneStore() *zones.Store { return zones.NewStore(h.db) }

// writeZoneError maps zones errors to HTTP statuses.
func writeZoneError(w http.ResponseWriter, what string, err error) {
	switch {
	case errors.Is(err, zones.ErrNotFound):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, zones.ErrDuplicate):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, zones.ErrInvalid):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, zones.ErrNoBranch):
		writeError(w, http.StatusPreconditionFailed, err.Error())
	default:
		log.Printf("admin: %s: %v", what, err)
		writeError(w, http.StatusInternalServerError, "failed to "+what)
	}
}

// RepositoryZones lists and creates a repository's zones.
func (h *AdminHandlers) RepositoryZones(w http.ResponseWriter, r *http.Request) {
	repoID, err := uuidFromQuery(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	switch r.Method {
	case http.MethodGet:
		zs, err := h.zoneStore().ListForRepository(r.Context(), repoID)
		if err != nil {
			writeZoneError(w, "list zones", err)
			return
		}
		writeJSON(w, http.StatusOK, zs)
	case http.MethodPost:
		var in struct {
			Name        string       `json:"name"`
			Description string       `json:"description"`
			Patterns    []string     `json:"patterns"`
			Policy      zones.Policy `json:"policy"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
		z, err := h.zoneStore().Create(r.Context(), repoID, zones.Template{
			Name: in.Name, Description: in.Description, Patterns: in.Patterns, Policy: in.Policy, Source: zones.SourceDashboard,
		})
		if err != nil {
			writeZoneError(w, "create zone", err)
			return
		}
		log.Printf("admin: zone %s (%s) created on repository %s by %s", z.Name, z.Policy, repoID, actorLabel(r))
		writeJSON(w, http.StatusCreated, z)
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// Zone edits or deletes one zone.
func (h *AdminHandlers) Zone(w http.ResponseWriter, r *http.Request) {
	id, err := uuidFromQuery(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	switch r.Method {
	case http.MethodPut, http.MethodPatch:
		var in struct {
			Patterns    []string      `json:"patterns"`
			Policy      *zones.Policy `json:"policy"`
			Description *string       `json:"description"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
		z, err := h.zoneStore().Update(r.Context(), id, zones.Update{Patterns: in.Patterns, Policy: in.Policy, Description: in.Description})
		if err != nil {
			writeZoneError(w, "update zone", err)
			return
		}
		log.Printf("admin: zone %s edited by %s: policy=%s patterns=%v", z.Name, actorLabel(r), z.Policy, z.Patterns)
		writeJSON(w, http.StatusOK, z)
	case http.MethodDelete:
		if err := h.zoneStore().Delete(r.Context(), id); err != nil {
			writeZoneError(w, "delete zone", err)
			return
		}
		log.Printf("admin: zone %s deleted by %s", id, actorLabel(r))
		w.WriteHeader(http.StatusNoContent)
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// FreezeZone freezes a zone with a reason: new work touching it is not
// planned (the item escalates) and implementation stops before
// verification.
func (h *AdminHandlers) FreezeZone(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	id, err := uuidFromQuery(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	var in struct {
		Reason string `json:"reason"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	z, err := h.zoneStore().Freeze(r.Context(), id, in.Reason)
	if err != nil {
		writeZoneError(w, "freeze zone", err)
		return
	}
	log.Printf("admin: zone %s frozen by %s: %s", z.Name, actorLabel(r), in.Reason)
	writeJSON(w, http.StatusOK, z)
}

// ThawZone makes a frozen zone active again.
func (h *AdminHandlers) ThawZone(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	id, err := uuidFromQuery(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	z, err := h.zoneStore().Thaw(r.Context(), id)
	if err != nil {
		writeZoneError(w, "thaw zone", err)
		return
	}
	log.Printf("admin: zone %s thawed by %s", z.Name, actorLabel(r))
	writeJSON(w, http.StatusOK, z)
}

// actorLabel names the acting user for the log.
func actorLabel(r *http.Request) string {
	if actor := actorIDFromCtx(r); actor != nil {
		return "user:" + actor.String()
	}
	return "admin"
}
