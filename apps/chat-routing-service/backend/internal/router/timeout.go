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

package router

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// TimeoutResult is one conversation's outcome from a timeout sweep --
// exactly one of ReassignedTo (with AssignedCase) or Requeued applies,
// mirroring DeclineResult's shape: a timed-out case is handled the same
// way as an explicit Decline, just triggered by a sweep instead of a
// click. Only the timed-out conversation is affected -- the unresponsive
// engineer's other concurrent cases, if any, and their own chat_status,
// are untouched (see the package doc comment on why chat_status is now a
// pure manual toggle).
type TimeoutResult struct {
	// UserID is the engineer who was PENDING on CaseID past the timeout.
	UserID       string    `json:"userId"`
	CaseID       string    `json:"caseId"`
	ReassignedTo string    `json:"reassignedTo,omitempty"`
	Requeued     bool      `json:"requeued,omitempty"`
	AssignedCase *CaseInfo `json:"assignedCase,omitempty"`
}

// pendingCandidate is one row from SweepExpiredPending's initial, unlocked
// scan -- re-verified under lock by timeoutOne before anything changes.
type pendingCandidate struct {
	userID string
	caseID string
}

// SweepExpiredPending finds every chat_conversation that's been assigned
// to an engineer for at least timeout without being confirmed via Accept,
// and treats each one like an explicit Decline: reassign to the next
// available engineer with spare capacity, excluding the unresponsive one,
// or flip the case back to WAITING_FOR_ENGINEER. Unlike the old single-
// case model, the unresponsive engineer's chat_status is left completely
// alone -- they may well be mid-conversation on a different concurrent
// case at the same time, and a timeout on one case is not a reason to pull
// them off everything else. They simply stop being a candidate for this
// one specific case; SetPresence/Escalate's own capacity check already
// keeps them from being handed more work than they can handle.
//
// Meant to be called periodically by a caller that also owns delivering
// the result (see csm-portal/backend's ChatHandler.StartTimeoutSweeper) --
// this package has no background loop of its own. Each call is a
// snapshot; a conversation that crosses the timeout between two calls is
// simply picked up on the next one.
func (r *Router) SweepExpiredPending(ctx context.Context, timeout time.Duration) ([]TimeoutResult, error) {
	rows, err := r.db.Query(ctx, `
		SELECT assignee_id, case_id FROM chat_conversation
		WHERE assignee_id IS NOT NULL AND state = 'OPEN' AND accepted_at IS NULL
		  AND session_ended_at IS NULL AND updated_at < now() - make_interval(secs => $1)
	`, timeout.Seconds())
	if err != nil {
		return nil, fmt.Errorf("router: scan expired pending: %w", err)
	}
	var candidates []pendingCandidate
	for rows.Next() {
		var c pendingCandidate
		if err := rows.Scan(&c.userID, &c.caseID); err != nil {
			rows.Close()
			return nil, fmt.Errorf("router: scan expired pending row: %w", err)
		}
		candidates = append(candidates, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("router: scan expired pending: %w", err)
	}

	var results []TimeoutResult
	for _, c := range candidates {
		result, err := r.timeoutOne(ctx, c.userID, c.caseID, timeout)
		if err != nil {
			return results, fmt.Errorf("router: time out %s on %s: %w", c.userID, c.caseID, err)
		}
		if result != nil {
			results = append(results, *result)
		}
	}
	return results, nil
}

// timeoutOne re-verifies and applies a single conversation's timeout
// inside its own transaction. Re-checks assignment/confirmation/staleness
// under the row lock, since SweepExpiredPending's own scan is unlocked --
// the engineer may have already accepted, declined, or been reassigned in
// the meantime, in which case this is a no-op (nil, nil) instead of
// double-processing it.
func (r *Router) timeoutOne(ctx context.Context, userID, caseID string, timeout time.Duration) (*TimeoutResult, error) {
	var result *TimeoutResult
	err := r.withTx(ctx, func(tx pgx.Tx) error {
		var (
			caseInfoJSON []byte
			assigneeID   *string
			state        string
			acceptedAt   *time.Time
			updatedAt    time.Time
		)
		err := tx.QueryRow(ctx, `
			SELECT case_info, assignee_id, state, accepted_at, updated_at
			FROM chat_conversation WHERE case_id = $1 AND session_ended_at IS NULL
			FOR UPDATE
		`, caseID).Scan(&caseInfoJSON, &assigneeID, &state, &acceptedAt, &updatedAt)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			return nil
		case err != nil:
			return fmt.Errorf("lock conversation: %w", err)
		}
		if assigneeID == nil || *assigneeID != userID || !isPending(state, acceptedAt) || time.Since(updatedAt) < timeout {
			return nil
		}

		var timedOut CaseInfo
		if caseInfoJSON != nil {
			if err := json.Unmarshal(caseInfoJSON, &timedOut); err != nil {
				return fmt.Errorf("decode case info: %w", err)
			}
		}

		if _, err := tx.Exec(ctx, `
			UPDATE chat_conversation
			SET assignee_id = NULL, accepted_at = NULL, updated_at = now()
			WHERE case_id = $1
		`, caseID); err != nil {
			return fmt.Errorf("clear timed-out conversation: %w", err)
		}

		// Audit trail: userID's ping on this conversation is settled as
		// TIMED_OUT.
		if _, err := tx.Exec(ctx, `
			INSERT INTO chat_queue_engineer_assignment (conversation_id, engineer_id, status)
			VALUES ($1, $2, 'TIMED_OUT')
		`, timedOut.ConversationID, userID); err != nil {
			return fmt.Errorf("record timeout outcome: %w", err)
		}

		candidate, ok, err := popAvailableEngineer(ctx, tx, userID)
		if err != nil {
			return err
		}
		if ok {
			if err := assignCaseToEngineer(ctx, tx, candidate, timedOut); err != nil {
				return err
			}
			assigned := timedOut
			result = &TimeoutResult{UserID: userID, CaseID: caseID, ReassignedTo: candidate, AssignedCase: &assigned}
			return nil
		}

		if err := requeueWaiting(ctx, tx, timedOut.ConversationID); err != nil {
			return err
		}
		result = &TimeoutResult{UserID: userID, CaseID: caseID, Requeued: true}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}
