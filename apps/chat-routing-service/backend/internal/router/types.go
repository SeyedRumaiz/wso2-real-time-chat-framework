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

// Package router implements the engineer-availability/queue state machine
// for the live-engineer-chat routing feature: engineers are AVAILABLE,
// PENDING (assigned, not yet accepted), BUSY (accepted, in-progress), or
// OFFLINE. An escalation goes to whichever AVAILABLE engineer has taken the
// fewest chats today (ties broken by who's been AVAILABLE longest), or gets
// queued FIFO if nobody qualifies; a completed session drains the queue.
// See Router.Escalate for the assignment logic and Router itself for the
// rest of the state machine.
//
// Engineers are identified by their IdP "userid" claim -- this package
// stores no email of its own; a caller that needs one already has it from
// its own authenticated session.
//
// Backed by PostgreSQL, so engineer presence and the queue survive a
// restart, and multiple replicas of this service can run against the same
// database safely since every state transition is a transaction rather
// than an in-process mutex. An assigned engineer who never accepts is
// caught by the timeout sweep in timeout.go, not left stuck forever.
package router

// Status is an engineer's current availability.
type Status string

const (
	StatusAvailable Status = "AVAILABLE"
	// StatusPending is an engineer who's just been assigned a case but
	// hasn't clicked Accept yet. Their capacity is already reserved, so
	// they're skipped by every "find an available engineer" query, but the
	// portal shows this separately from Busy until Accept confirms it.
	StatusPending Status = "PENDING"
	// StatusBusy is an engineer with an accepted, in-progress session.
	StatusBusy    Status = "BUSY"
	StatusOffline Status = "OFFLINE"
)

// CaseInfo is everything csm-portal/backend needs to reconstruct the chat
// event it publishes to whichever engineer a case ends up assigned to.
// Carried through Escalate/queueing/Decline verbatim -- this service never
// interprets these fields itself. Stored as a JSONB blob rather than
// normalized columns since nothing here queries by any field but CaseID.
type CaseInfo struct {
	CaseID         string `json:"caseId"`
	ConversationID string `json:"conversationId"`
	ProjectID      string `json:"projectId,omitempty"`
	Subject        string `json:"subject,omitempty"`
	CustomerEmail  string `json:"customerEmail,omitempty"`
	CustomerName   string `json:"customerName,omitempty"`
	Message        string `json:"message,omitempty"`
}
