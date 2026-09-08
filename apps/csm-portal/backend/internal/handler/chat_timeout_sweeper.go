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

package handler

import (
	"context"
	"log/slog"
	"time"
)

// StartTimeoutSweeper polls chat-routing-service periodically for
// engineers who were assigned a case (PENDING) and never accepted it past
// that service's own configured PENDING_TIMEOUT_SECONDS, delivering any
// resulting reassignment the same way a fresh escalation would arrive, and
// clearing the stale alert on the original (unresponsive) engineer's
// screen.
//
// Deliberately a poll from this side rather than a push from
// chat-routing-service: this handler already owns the only thing that can
// act on the result (the engineer SSE hub), and chat-routing-service is
// intentionally synchronous/caller-driven only (see routingclient's
// package doc comment on why it never calls back) -- adding a reverse
// callback direction just for this one feature would be new
// infrastructure for a small win. Runs until ctx is cancelled -- see
// cmd/server/main.go, which starts this alongside the HTTP server and
// passes the same shutdown context.
func (h *ChatHandler) StartTimeoutSweeper(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			h.sweepTimeoutsOnce(ctx)
		}
	}
}

// sweepTimeoutsOnce runs one sweep. Best-effort by construction, matching
// this file's other background/side-channel calls: a failed sweep just
// means this tick found nothing to do from this handler's point of view —
// chat-routing-service itself is unaffected, and the next tick tries again.
func (h *ChatHandler) sweepTimeoutsOnce(ctx context.Context) {
	results, err := h.routing.SweepTimeouts(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "chat: timeout sweep failed", "err", err)
		return
	}
	for _, result := range results {
		slog.InfoContext(ctx, "chat: engineer timed out on pending case",
			"userID", result.UserID, "caseId", result.CaseID,
			"reassignedTo", result.ReassignedTo, "requeued", result.Requeued)

		// Tell the unresponsive engineer's own browser their stale pending
		// alert is gone. NOTE: the frontend does not yet have a handler for
		// this "case_timed_out" event type -- EngineerAlertNotification.tsx
		// needs a small follow-up to clear/relabel the card on this event,
		// the same way it already does for handleComplete/handleDismiss.
		// Until then this event is harmless but inert on the receiving
		// browser (an unrecognised type is simply ignored) -- it does not
		// block the reassignment below, which is the part that actually
		// matters for the customer.
		h.publishToEngineer(result.UserID, chatEvent{
			Type:      "case_timed_out",
			CaseID:    result.CaseID,
			Timestamp: time.Now().UTC().Format(time.RFC3339),
		})

		if result.ReassignedTo != "" && result.AssignedCase != nil {
			h.publishToEngineer(result.ReassignedTo, assignedCaseEvent(*result.AssignedCase))
		}
	}
}
