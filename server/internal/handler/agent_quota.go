package handler

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/quotagroup"
)

// GetAgentQuotaGroup reports a configured account gate to its owner or a
// workspace administrator. The alias is non-secret; the failure text is
// omitted because provider errors can contain account-specific information.
func (h *Handler) GetAgentQuotaGroup(w http.ResponseWriter, r *http.Request) {
	agent, ok := h.loadAgentForUser(w, r, chi.URLParam(r, "id"))
	if !ok || !h.canManageAgent(w, r, agent) {
		return
	}
	key := quotagroup.FromRuntimeConfig(agent.RuntimeConfig)
	if key == "" || !agent.OwnerID.Valid {
		writeJSON(w, http.StatusOK, map[string]any{"configured": false, "blocked": false})
		return
	}
	block, err := h.Queries.GetAgentQuotaGroupBlock(r.Context(), db.GetAgentQuotaGroupBlockParams{OwnerID: agent.OwnerID, GroupKey: key})
	if errors.Is(err, pgx.ErrNoRows) {
		writeJSON(w, http.StatusOK, map[string]any{"configured": true, "group": key, "blocked": false})
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not read quota group")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"configured": true, "group": key, "blocked": true, "blocked_at": block.BlockedAt, "reason": "provider quota limit"})
}

// ClearAgentQuotaGroup is an explicit human recovery action after the
// subscription's quota window has been checked. It never replays terminal runs.
func (h *Handler) ClearAgentQuotaGroup(w http.ResponseWriter, r *http.Request) {
	agent, ok := h.loadAgentForUser(w, r, chi.URLParam(r, "id"))
	if !ok || !h.canManageAgent(w, r, agent) {
		return
	}
	// This gate belongs to the agent owner across workspaces.
	if !agent.OwnerID.Valid {
		writeError(w, http.StatusBadRequest, "agent has no owner for quota grouping")
		return
	}
	if uuidToString(agent.OwnerID) != requestUserID(r) {
		writeError(w, http.StatusForbidden, "only the agent owner can clear the quota group")
		return
	}
	key := quotagroup.FromRuntimeConfig(agent.RuntimeConfig)
	if key == "" {
		writeError(w, http.StatusBadRequest, "agent has no valid quota_group")
		return
	}
	_, err := h.Queries.ClearAgentQuotaGroupBlock(r.Context(), db.ClearAgentQuotaGroupBlockParams{OwnerID: agent.OwnerID, GroupKey: key})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not clear quota group")
		return
	}
	if err := h.TaskService.NotifyQuotaGroupCleared(r.Context(), agent.OwnerID, key); err != nil {
		slog.Warn("quota group cleared but runtime wakeup failed", "agent_id", uuidToString(agent.ID), "error", err)
	}
	writeJSON(w, http.StatusOK, map[string]any{"group": key, "blocked": false})
}
