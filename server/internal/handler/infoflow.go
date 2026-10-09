package handler

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/multica-ai/multica/server/internal/integrations/infoflow"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

type InfoFlowInstallationResponse struct {
	ID              string `json:"id"`
	WorkspaceID     string `json:"workspace_id"`
	AgentID         string `json:"agent_id"`
	ApplicationID   string `json:"application_id"`
	BotUserID       string `json:"bot_user_id,omitempty"`
	InstallerUserID string `json:"installer_user_id"`
	Status          string `json:"status"`
	InstalledAt     string `json:"installed_at"`
	CreatedAt       string `json:"created_at"`
	UpdatedAt       string `json:"updated_at"`
}

func infoflowInstallationToResponse(row db.ChannelInstallation) InfoFlowInstallationResponse {
	cfg, _ := infoflow.UnmarshalInstallationConfig(row.Config)
	return InfoFlowInstallationResponse{
		ID:              uuidToString(row.ID),
		WorkspaceID:     uuidToString(row.WorkspaceID),
		AgentID:         uuidToString(row.AgentID),
		ApplicationID:   cfg.AppID,
		BotUserID:       cfg.BotUserID,
		InstallerUserID: uuidToString(row.InstallerUserID),
		Status:          row.Status,
		InstalledAt:     row.InstalledAt.Time.UTC().Format(time.RFC3339),
		CreatedAt:       row.CreatedAt.Time.UTC().Format(time.RFC3339),
		UpdatedAt:       row.UpdatedAt.Time.UTC().Format(time.RFC3339),
	}
}

func (h *Handler) ListInfoFlowInstallations(w http.ResponseWriter, r *http.Request) {
	if h.InfoFlowInstall == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"installations":     []InfoFlowInstallationResponse{},
			"configured":        false,
			"install_supported": false,
		})
		return
	}
	wsUUID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "id"), "workspace id")
	if !ok {
		return
	}
	rows, err := h.InfoFlowInstall.ListByWorkspace(r.Context(), wsUUID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list infoflow installations")
		return
	}
	out := make([]InfoFlowInstallationResponse, 0, len(rows))
	for _, row := range rows {
		out = append(out, infoflowInstallationToResponse(row))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"installations":     out,
		"configured":        true,
		"install_supported": true,
	})
}

type RegisterInfoFlowBYORequest struct {
	ApplicationID string `json:"application_id"`
	AppKey        string `json:"app_key"`
	AppSecret     string `json:"app_secret"`
	BotUserID     string `json:"bot_user_id,omitempty"`
}

func (h *Handler) RegisterInfoFlowBYO(w http.ResponseWriter, r *http.Request) {
	if h.InfoFlowInstall == nil {
		writeFeatureDisabled(w, "infoflow_not_configured", "infoflow integration not enabled")
		return
	}
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	wsUUID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "id"), "workspace id")
	if !ok {
		return
	}
	agentIDStr := strings.TrimSpace(r.URL.Query().Get("agent_id"))
	if agentIDStr == "" {
		writeError(w, http.StatusBadRequest, "agent_id is required")
		return
	}
	agentUUID, ok := parseUUIDOrBadRequest(w, agentIDStr, "agent_id")
	if !ok {
		return
	}
	if _, err := h.Queries.GetAgentInWorkspace(r.Context(), db.GetAgentInWorkspaceParams{
		ID: agentUUID, WorkspaceID: wsUUID,
	}); err != nil {
		writeError(w, http.StatusNotFound, "agent not found in this workspace")
		return
	}
	initiatorUUID, ok := parseUUIDOrBadRequest(w, userID, "user id")
	if !ok {
		return
	}
	var body RegisterInfoFlowBYORequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	row, err := h.InfoFlowInstall.RegisterBYO(r.Context(), infoflow.RegisterBYOParams{
		WorkspaceID:   wsUUID,
		AgentID:       agentUUID,
		InitiatorID:   initiatorUUID,
		ApplicationID: body.ApplicationID,
		AppKey:        body.AppKey,
		AppSecret:     body.AppSecret,
		BotUserID:     body.BotUserID,
	})
	if err != nil {
		switch {
		case errors.Is(err, infoflow.ErrInvalidApplicationID), errors.Is(err, infoflow.ErrInvalidAppKey), errors.Is(err, infoflow.ErrInvalidAppSecret), errors.Is(err, infoflow.ErrInvalidBotUserID), errors.Is(err, infoflow.ErrCredentialValidation):
			writeError(w, http.StatusBadRequest, err.Error())
		case errors.Is(err, infoflow.ErrAppOwnedBySameWorkspace):
			writeError(w, http.StatusConflict, "this InfoFlow application is already connected to another agent in this workspace — disconnect it there first, then connect it here")
		case errors.Is(err, infoflow.ErrAppOwnedByArchivedAgent):
			writeError(w, http.StatusConflict, "this InfoFlow application is connected to an archived agent in this workspace — restore that agent, or disconnect its bot, before connecting it here")
		case errors.Is(err, infoflow.ErrAppOwnedByAnotherWorkspace):
			writeError(w, http.StatusConflict, "this InfoFlow application is already connected to a different Multica workspace — disconnect it there before connecting it here")
		default:
			writeError(w, http.StatusInternalServerError, "failed to register infoflow installation")
		}
		return
	}
	writeJSON(w, http.StatusOK, infoflowInstallationToResponse(row))
}

func (h *Handler) RevokeInfoFlowInstallation(w http.ResponseWriter, r *http.Request) {
	if h.InfoFlowInstall == nil {
		writeFeatureDisabled(w, "infoflow_not_configured", "infoflow integration not configured")
		return
	}
	wsUUID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "id"), "workspace id")
	if !ok {
		return
	}
	instUUID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "installationId"), "installation id")
	if !ok {
		return
	}
	if _, err := h.InfoFlowInstall.GetInWorkspace(r.Context(), instUUID, wsUUID); err != nil {
		if errors.Is(err, infoflow.ErrInstallationNotFound) {
			writeError(w, http.StatusNotFound, "infoflow installation not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to load installation")
		return
	}
	if err := h.InfoFlowInstall.Revoke(r.Context(), instUUID); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to revoke installation")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type RedeemInfoFlowBindingTokenRequest struct {
	Token string `json:"token"`
}

type RedeemInfoFlowBindingTokenResponse struct {
	WorkspaceID    string `json:"workspace_id"`
	InstallationID string `json:"installation_id"`
	InfoFlowUserID string `json:"infoflow_user_id"`
}

func (h *Handler) RedeemInfoFlowBindingToken(w http.ResponseWriter, r *http.Request) {
	if h.InfoFlowBindingTokens == nil {
		writeFeatureDisabled(w, "infoflow_not_configured", "infoflow integration not configured")
		return
	}
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	var req RedeemInfoFlowBindingTokenRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Token == "" {
		writeError(w, http.StatusBadRequest, "token is required")
		return
	}
	userUUID, ok := parseUUIDOrBadRequest(w, userID, "user id")
	if !ok {
		return
	}
	redeemed, err := h.InfoFlowBindingTokens.RedeemAndBind(r.Context(), req.Token, userUUID)
	if err != nil {
		switch {
		case errors.Is(err, infoflow.ErrBindingTokenInvalid):
			writeError(w, http.StatusGone, "binding token invalid or expired")
		case errors.Is(err, infoflow.ErrBindingAlreadyAssigned):
			writeError(w, http.StatusConflict, "this InfoFlow account is already bound to a different Multica user")
		case errors.Is(err, infoflow.ErrBindingNotWorkspaceMember):
			writeError(w, http.StatusForbidden, "binding refused (are you a workspace member?)")
		default:
			writeError(w, http.StatusInternalServerError, "failed to redeem token")
		}
		return
	}
	writeJSON(w, http.StatusOK, RedeemInfoFlowBindingTokenResponse{
		WorkspaceID:    uuidToString(redeemed.WorkspaceID),
		InstallationID: uuidToString(redeemed.InstallationID),
		InfoFlowUserID: redeemed.InfoFlowUserID,
	})
}
