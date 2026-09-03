// Copyright (c) 2026 WSO2 LLC. (https://www.wso2.com).
//
// WSO2 LLC. licenses this file to you under the Apache License,
// Version 2.0 (the "License"); you may not use this file except
// in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied.  See the License for the
// specific language governing permissions and limitations
// under the License.

// Package handler implements the HTTP surface over internal/router.Router.
// Every route here is server-to-server only (see internal/middleware.
// InternalToken) — csm-portal/backend is the only real caller, via its own
// internal/routingclient package.
package handler

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/wso2-open-operations/cs-tools/apps/chat-routing-service/backend/internal/router"
)

// maxBodyBytes bounds request bodies — generous for these small JSON
// payloads while still capping memory use.
const maxBodyBytes = 64 << 10 // 64 KiB

// RoutingHandler adapts HTTP requests to router.Router calls.
type RoutingHandler struct {
	router *router.Router
}

// NewRoutingHandler constructs a RoutingHandler over r.
func NewRoutingHandler(r *router.Router) *RoutingHandler {
	return &RoutingHandler{router: r}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"message": msg})
}

// writeStorageError logs the underlying Postgres/pool error (never sent to
// the caller) and responds 502 — the routing service's own state store is
// unavailable, matching how csm-portal/backend already treats a routing
// service failure (see internal/handler/chat.go's HandleSetPresence).
func writeStorageError(w http.ResponseWriter, route string, err error) {
	slog.Error("routing store error", "route", route, "err", err)
	writeError(w, http.StatusBadGateway, "Routing state store is unavailable.")
}

func decodeBody(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid request payload.")
		return false
	}
	return true
}

func isValidStatus(s string) bool {
	switch router.Status(s) {
	case router.StatusAvailable, router.StatusPending, router.StatusBusy, router.StatusOffline:
		return true
	default:
		return false
	}
}

// escalateRequest is the body csm-portal/backend sends for
// POST /route/escalate — one field per router.CaseInfo field.
type escalateRequest struct {
	CaseID         string `json:"caseId"`
	ConversationID string `json:"conversationId"`
	ProjectID      string `json:"projectId"`
	Subject        string `json:"subject"`
	CustomerEmail  string `json:"customerEmail"`
	CustomerName   string `json:"customerName"`
	Message        string `json:"message"`
}

// Escalate handles POST /route/escalate.
func (h *RoutingHandler) Escalate(w http.ResponseWriter, r *http.Request) {
	var req escalateRequest
	if !decodeBody(w, r, &req) {
		return
	}
	if req.CaseID == "" || req.ConversationID == "" {
		writeError(w, http.StatusBadRequest, "caseId and conversationId are required.")
		return
	}

	result, err := h.router.Escalate(r.Context(), router.CaseInfo{
		CaseID:         req.CaseID,
		ConversationID: req.ConversationID,
		ProjectID:      req.ProjectID,
		Subject:        req.Subject,
		CustomerEmail:  req.CustomerEmail,
		CustomerName:   req.CustomerName,
		Message:        req.Message,
	})
	if err != nil {
		writeStorageError(w, "escalate", err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// presenceRequest is the body for POST /route/presence. EngineerID is the
// IdP's stable per-account "userid" claim (see internal/router.Router.
// SetPresence and migrations/000004) -- required so a first-time presence
// update can create the engineer's row; ignored (not an error) once that
// row already exists, since engineer_id never changes for an established
// email.
type presenceRequest struct {
	Email      string `json:"email"`
	EngineerID string `json:"engineerId"`
	Status     string `json:"status"`
}

// SetPresence handles POST /route/presence.
func (h *RoutingHandler) SetPresence(w http.ResponseWriter, r *http.Request) {
	var req presenceRequest
	if !decodeBody(w, r, &req) {
		return
	}
	if req.Email == "" || req.EngineerID == "" || !isValidStatus(req.Status) {
		writeError(w, http.StatusBadRequest, "email, engineerId, and a valid status (AVAILABLE|OFFLINE) are required.")
		return
	}

	result, err := h.router.SetPresence(r.Context(), req.Email, req.EngineerID, router.Status(req.Status))
	if err != nil {
		writeStorageError(w, "presence", err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// completedRequest is the body for POST /route/completed.
type completedRequest struct {
	Email string `json:"email"`
}

// Completed handles POST /route/completed.
func (h *RoutingHandler) Completed(w http.ResponseWriter, r *http.Request) {
	var req completedRequest
	if !decodeBody(w, r, &req) {
		return
	}
	if req.Email == "" {
		writeError(w, http.StatusBadRequest, "email is required.")
		return
	}

	result, err := h.router.Completed(r.Context(), req.Email)
	if err != nil {
		writeStorageError(w, "completed", err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// declineRequest is the body for POST /route/decline.
type declineRequest struct {
	Email  string `json:"email"`
	CaseID string `json:"caseId"`
}

// Decline handles POST /route/decline.
func (h *RoutingHandler) Decline(w http.ResponseWriter, r *http.Request) {
	var req declineRequest
	if !decodeBody(w, r, &req) {
		return
	}
	if req.Email == "" || req.CaseID == "" {
		writeError(w, http.StatusBadRequest, "email and caseId are required.")
		return
	}

	result, err := h.router.Decline(r.Context(), req.Email, req.CaseID)
	if err != nil {
		writeStorageError(w, "decline", err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// acceptRequest is the body for POST /route/accept.
type acceptRequest struct {
	Email  string `json:"email"`
	CaseID string `json:"caseId"`
}

// Accept handles POST /route/accept -- confirms email is accepting the
// case they were assigned (PENDING -> BUSY). See router.Router.Accept's
// own doc comment for when Applied comes back false instead of erroring.
func (h *RoutingHandler) Accept(w http.ResponseWriter, r *http.Request) {
	var req acceptRequest
	if !decodeBody(w, r, &req) {
		return
	}
	if req.Email == "" || req.CaseID == "" {
		writeError(w, http.StatusBadRequest, "email and caseId are required.")
		return
	}

	result, err := h.router.Accept(r.Context(), req.Email, req.CaseID)
	if err != nil {
		writeStorageError(w, "accept", err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// GetPresence handles GET /route/presence/{email}. Includes currentCase
// (omitted when there is none) so a caller whose own UI state for a
// pending alert or active session was lost -- a browser refresh, a closed
// tab -- can rehydrate it instead of the engineer being stuck PENDING or
// BUSY with nothing to act on (see router.Router.PresenceDetail).
func (h *RoutingHandler) GetPresence(w http.ResponseWriter, r *http.Request) {
	email := r.PathValue("email")
	if email == "" {
		writeError(w, http.StatusBadRequest, "email is required.")
		return
	}
	detail, err := h.router.GetPresence(r.Context(), email)
	if err != nil {
		writeStorageError(w, "presence:get", err)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Status      router.Status    `json:"status"`
		CurrentCase *router.CaseInfo `json:"currentCase,omitempty"`
	}{Status: detail.Status, CurrentCase: detail.CurrentCase})
}

// workItemRequest is the body for POST /route/workitem -- see
// router.Router.CreateWorkItem.
type workItemRequest struct {
	CaseID         string `json:"caseId"`
	ConversationID string `json:"conversationId"`
	CreatorEmail   string `json:"creatorEmail"`
	Subject        string `json:"subject"`
	InitialMessage string `json:"initialMessage"`
}

// CreateWorkItem handles POST /route/workitem -- LOCAL STAND-IN endpoint,
// see router/workitem.go's package doc comment. Called once per escalation
// from csm-portal/backend's HandleEscalate, regardless of routing outcome.
func (h *RoutingHandler) CreateWorkItem(w http.ResponseWriter, r *http.Request) {
	var req workItemRequest
	if !decodeBody(w, r, &req) {
		return
	}
	if req.CaseID == "" || req.ConversationID == "" || req.CreatorEmail == "" || req.Subject == "" {
		writeError(w, http.StatusBadRequest, "caseId, conversationId, creatorEmail, and subject are required.")
		return
	}

	if err := h.router.CreateWorkItem(r.Context(), req.CaseID, req.ConversationID, req.CreatorEmail, req.Subject, req.InitialMessage); err != nil {
		writeStorageError(w, "workitem:create", err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]bool{"created": true})
}

// commentRequest is the body for POST /route/comment -- see
// router.Router.AddComment.
type commentRequest struct {
	CaseID      string `json:"caseId"`
	AuthorEmail string `json:"authorEmail"`
	Content     string `json:"content"`
}

// AddComment handles POST /route/comment -- LOCAL STAND-IN endpoint, see
// router/workitem.go's package doc comment. Called for both directions of
// a live chat message (customer and engineer) -- see csm-portal/backend's
// HandleCustomerMessage and HandleEngineerMessage.
func (h *RoutingHandler) AddComment(w http.ResponseWriter, r *http.Request) {
	var req commentRequest
	if !decodeBody(w, r, &req) {
		return
	}
	if req.CaseID == "" || req.AuthorEmail == "" || req.Content == "" {
		writeError(w, http.StatusBadRequest, "caseId, authorEmail, and content are required.")
		return
	}

	if err := h.router.AddComment(r.Context(), req.CaseID, req.AuthorEmail, req.Content); err != nil {
		writeStorageError(w, "comment:add", err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]bool{"added": true})
}

// DebugWorkItem handles GET /route/debug/workitem/{caseId} -- LOCAL
// STAND-IN endpoint, verification-only, mirroring DebugState's own reason
// for existing.
func (h *RoutingHandler) DebugWorkItem(w http.ResponseWriter, r *http.Request) {
	caseID := r.PathValue("caseId")
	if caseID == "" {
		writeError(w, http.StatusBadRequest, "caseId is required.")
		return
	}
	detail, err := h.router.DebugWorkItem(r.Context(), caseID)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, detail)
}

// DebugState handles GET /route/debug/state — see router.DebugState's own
// doc comment on why this exists.
func (h *RoutingHandler) DebugState(w http.ResponseWriter, r *http.Request) {
	state, err := h.router.DebugState(r.Context())
	if err != nil {
		writeStorageError(w, "debug/state", err)
		return
	}
	writeJSON(w, http.StatusOK, state)
}
