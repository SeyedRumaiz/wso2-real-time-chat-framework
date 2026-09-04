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

// Package routingclient is the client SDK for chat-routing-service
// (apps/chat-routing-service/backend) — the in-memory engineer
// availability/queue state machine that decides which engineer (if any) an
// escalation is routed to. See that service's internal/handler/routes.go
// for the exact routes and response shapes this client mirrors.
//
// # Server-to-server only
//
// Every call here is authenticated with a shared bearer secret
// (X-Routing-Service-Token) rather than a user JWT — chat-routing-service
// and its callers do not share a JWT audience/issuer for a "service
// identity", so this is a static, pre-shared secret instead. That makes
// this client safe to use only from a backend process that can hold a
// secret: never construct a Client in code that ships to a browser or any
// other untrusted runtime, and never proxy this token through to one. A
// frontend that needs routing decisions should keep calling its own
// backend's proxy endpoints (see apps/csm-portal/backend/internal/handler/
// chat.go's HandleEscalate/HandleSetPresence/etc.), which hold this
// Client server-side and never expose InternalToken to the browser.
//
// # Versioning
//
// This module is nested inside the wso2-open-operations/cs-tools monorepo
// at apps/chat-routing-service/sdk-go and is versioned independently of
// both chat-routing-service/backend and any of its callers, via git tags
// scoped to this directory (e.g. apps/chat-routing-service/sdk-go/v0.1.0).
// A breaking change to chat-routing-service's HTTP API should land here as
// a new type/method (or a major version bump) rather than a silent
// behavior change to an existing one, since callers pin a version like any
// other Go dependency.
package routingclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// internalTokenHeader must match chat-routing-service/backend's own
// internal/middleware.InternalTokenHeader.
const internalTokenHeader = "X-Routing-Service-Token"

// maxResponseBodyBytes bounds how much of an error response this client will
// read into memory/log.
const maxResponseBodyBytes = 64 << 10 // 64 KiB

// Config holds the configuration for the chat-routing-service client.
type Config struct {
	// BaseURL is the chat-routing-service's listener base URL, e.g.
	// "http://localhost:9096" (ROUTING_SERVICE_BASE_URL).
	BaseURL string
	// InternalToken is the shared secret sent as X-Routing-Service-Token.
	// Must equal that service's own ROUTING_SERVICE_TOKEN. Load this from
	// your own process's environment/secret store — never hardcode it,
	// and never let it reach client-side code (see the package doc above).
	InternalToken string
}

// Client calls the chat-routing-service. Safe for concurrent use by
// multiple goroutines (holds no mutable state beyond its *http.Client).
type Client struct {
	http    *http.Client
	baseURL string
	token   string
}

// NewClient constructs a Client. Does not validate connectivity — the first
// call surfaces a dial failure; callers decide per-endpoint whether that is
// fatal to the caller-facing request (chat-routing-service's callers
// typically fall back to a degraded default for user-facing calls like
// Escalate, and log-and-continue for best-effort side-channel calls like
// Completed/Decline — see apps/csm-portal/backend/internal/handler/chat.go
// for a worked example of both).
func NewClient(cfg Config) *Client {
	return &Client{
		http:    &http.Client{Timeout: 10 * time.Second},
		baseURL: strings.TrimRight(cfg.BaseURL, "/"),
		token:   cfg.InternalToken,
	}
}

// Status mirrors chat-routing-service/backend/internal/router.Status.
// Duplicated rather than shared via a common package: chat-routing-service
// and this SDK are versioned and deployed independently, and this HTTP API
// is their only coupling point.
type Status string

const (
	StatusAvailable Status = "AVAILABLE"
	// StatusPending is an engineer assigned a case but who has not yet
	// clicked Accept -- see chat-routing-service's router.StatusPending
	// and router.Router.Accept.
	StatusPending Status = "PENDING"
	StatusBusy    Status = "BUSY"
	StatusOffline Status = "OFFLINE"
)

// CaseInfo mirrors router.CaseInfo — the case fields carried through an
// escalation, queueing, or reassignment. Field names and JSON tags match
// that service's escalateRequest/CaseInfo wire shape exactly, so a CaseInfo
// value can be marshaled directly as the POST /route/escalate body.
type CaseInfo struct {
	CaseID         string `json:"caseId"`
	ConversationID string `json:"conversationId"`
	ProjectID      string `json:"projectId,omitempty"`
	Subject        string `json:"subject,omitempty"`
	CustomerEmail  string `json:"customerEmail,omitempty"`
	CustomerName   string `json:"customerName,omitempty"`
	Message        string `json:"message,omitempty"`
}

// EscalateResult mirrors router.EscalateResult.
type EscalateResult struct {
	EngineerEmail string `json:"engineerEmail,omitempty"`
	Queued        bool   `json:"queued,omitempty"`
	Position      int    `json:"position,omitempty"`
}

// PresenceResult mirrors router.PresenceResult.
type PresenceResult struct {
	Applied        bool      `json:"applied"`
	PendingOffline bool      `json:"pendingOffline,omitempty"`
	AssignedCase   *CaseInfo `json:"assignedCase,omitempty"`
}

// CompletedResult mirrors router.CompletedResult.
type CompletedResult struct {
	Removed      bool      `json:"removed,omitempty"`
	Rejoined     bool      `json:"rejoined,omitempty"`
	AssignedCase *CaseInfo `json:"assignedCase,omitempty"`
}

// DeclineResult mirrors router.DeclineResult.
type DeclineResult struct {
	ReassignedTo string `json:"reassignedTo,omitempty"`
	Requeued     bool   `json:"requeued,omitempty"`
	// AssignedCase is set alongside ReassignedTo — the declined case, now
	// handed to that other engineer, in the same shape Escalate would
	// have delivered it in originally.
	AssignedCase *CaseInfo `json:"assignedCase,omitempty"`
}

// AcceptResult mirrors router.AcceptResult.
type AcceptResult struct {
	Applied bool `json:"applied"`
}

// do is the shared request/response plumbing for every method below: encode
// reqBody (if any) as the JSON request body, attach the shared-secret
// header, and on a 2xx response decode into out (if any). Any non-2xx
// response or transport failure is returned as an error; the routing
// service never uses redirects, so the 2xx check does not follow any.
func (c *Client) do(ctx context.Context, method, path string, reqBody, out any) error {
	var bodyReader io.Reader
	if reqBody != nil {
		b, err := json.Marshal(reqBody)
		if err != nil {
			return fmt.Errorf("routingclient: encode request: %w", err)
		}
		bodyReader = bytes.NewReader(b)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bodyReader)
	if err != nil {
		return fmt.Errorf("routingclient: build request: %w", err)
	}
	if reqBody != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set(internalTokenHeader, c.token)

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("routingclient: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBodyBytes))
		return fmt.Errorf("routingclient: %s %s: upstream returned %d: %s", method, path, resp.StatusCode, string(body))
	}

	if out == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("routingclient: %s %s: decode response: %w", method, path, err)
	}
	return nil
}

// Escalate calls POST /route/escalate, asking the routing service to assign
// ci to an available engineer or queue it.
func (c *Client) Escalate(ctx context.Context, ci CaseInfo) (EscalateResult, error) {
	var out EscalateResult
	err := c.do(ctx, http.MethodPost, "/route/escalate", ci, &out)
	return out, err
}

// SetPresence calls POST /route/presence, applying an engineer's requested
// status change (see router.Router.SetPresence's doc comment for the full
// state machine this triggers). engineerID is the caller's stable
// per-account identifier (e.g. an IdP "userid" claim) -- the routing
// service only uses it the first time it sees email, to populate that new
// row's engineer_id primary key; it's ignored (not an error) on every
// later call for an already-known email.
func (c *Client) SetPresence(ctx context.Context, email, engineerID string, status Status) (PresenceResult, error) {
	var out PresenceResult
	body := struct {
		Email      string `json:"email"`
		EngineerID string `json:"engineerId"`
		Status     Status `json:"status"`
	}{Email: email, EngineerID: engineerID, Status: status}
	err := c.do(ctx, http.MethodPost, "/route/presence", body, &out)
	return out, err
}

// Completed calls POST /route/completed, reporting that email just ended
// their current session.
func (c *Client) Completed(ctx context.Context, email string) (CompletedResult, error) {
	var out CompletedResult
	body := struct {
		Email string `json:"email"`
	}{Email: email}
	err := c.do(ctx, http.MethodPost, "/route/completed", body, &out)
	return out, err
}

// Decline calls POST /route/decline, reporting that email is declining the
// case identified by caseID before accepting it.
func (c *Client) Decline(ctx context.Context, email, caseID string) (DeclineResult, error) {
	var out DeclineResult
	body := struct {
		Email  string `json:"email"`
		CaseID string `json:"caseId"`
	}{Email: email, CaseID: caseID}
	err := c.do(ctx, http.MethodPost, "/route/decline", body, &out)
	return out, err
}

// Accept calls POST /route/accept, confirming email is accepting the case
// (caseID) they were assigned -- flips PENDING to BUSY server-side. See
// router.Router.Accept's own doc comment for when Applied comes back
// false (a stale accept) rather than an error.
func (c *Client) Accept(ctx context.Context, email, caseID string) (AcceptResult, error) {
	var out AcceptResult
	body := struct {
		Email  string `json:"email"`
		CaseID string `json:"caseId"`
	}{Email: email, CaseID: caseID}
	err := c.do(ctx, http.MethodPost, "/route/accept", body, &out)
	return out, err
}

// CreateWorkItem calls POST /route/workitem -- LOCAL STAND-IN persistence,
// see chat-routing-service's internal/router/workitem.go package doc
// comment. Creates the work_item + chat_conversation pair (plus the first
// comment, if initialMessage is non-empty) for a brand-new escalation.
func (c *Client) CreateWorkItem(ctx context.Context, caseID, conversationID, creatorEmail, subject, initialMessage string) error {
	body := struct {
		CaseID         string `json:"caseId"`
		ConversationID string `json:"conversationId"`
		CreatorEmail   string `json:"creatorEmail"`
		Subject        string `json:"subject"`
		InitialMessage string `json:"initialMessage,omitempty"`
	}{
		CaseID: caseID, ConversationID: conversationID, CreatorEmail: creatorEmail,
		Subject: subject, InitialMessage: initialMessage,
	}
	return c.do(ctx, http.MethodPost, "/route/workitem", body, nil)
}

// AddComment calls POST /route/comment -- LOCAL STAND-IN persistence, same
// caveat as CreateWorkItem above. Used for both directions of a live chat
// message (customer and engineer) so the whole transcript lands in one
// place.
func (c *Client) AddComment(ctx context.Context, caseID, authorEmail, content string) error {
	body := struct {
		CaseID      string `json:"caseId"`
		AuthorEmail string `json:"authorEmail"`
		Content     string `json:"content"`
	}{CaseID: caseID, AuthorEmail: authorEmail, Content: content}
	return c.do(ctx, http.MethodPost, "/route/comment", body, nil)
}

// PresenceDetail is GetPresence's result -- status plus, when PENDING or
// BUSY, the case the engineer is currently on (nil otherwise). CurrentCase
// exists so a caller can rehydrate an active alert or session's UI state
// after it's been lost client-side (a refresh, a closed tab) even though
// the engineer is still genuinely holding it server-side.
type PresenceDetail struct {
	Status      Status    `json:"status"`
	CurrentCase *CaseInfo `json:"currentCase,omitempty"`
}

// GetPresence calls GET /route/presence/{email}, returning StatusOffline
// (and no case) for an engineer the routing service has never seen a
// presence update from (see router.Router.GetPresence).
func (c *Client) GetPresence(ctx context.Context, email string) (PresenceDetail, error) {
	var out PresenceDetail
	err := c.do(ctx, http.MethodGet, "/route/presence/"+url.PathEscape(email), nil, &out)
	if err != nil {
		return PresenceDetail{}, err
	}
	if out.Status == "" {
		out.Status = StatusOffline
	}
	return out, nil
}
