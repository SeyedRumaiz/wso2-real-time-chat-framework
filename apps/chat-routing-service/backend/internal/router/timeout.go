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

// TimeoutResult is one engineer's outcome from a timeout sweep -- exactly
// one of ReassignedTo (with AssignedCase) or Requeued applies, mirroring
// DeclineResult's own shape: a timed-out PENDING case is handled the same
// way as an explicit Decline from the caller's perspective, just triggered
// by a background sweep instead of the engineer's own click.
type TimeoutResult struct {
	// UserID is the engineer who was PENDING past the timeout -- taken
	// OFFLINE as a side effect (see SweepExpiredPending's doc comment for
	// why OFFLINE rather than AVAILABLE). See migrations/
	// 000014_rename_engineer_status_table for why this is a user ID rather
	// than an email.
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

// SweepExpiredPending finds every engineer who has been PENDING on the
// same case for at least timeout and treats each one exactly like an
// explicit Decline (see Router.Decline: reassign to the next available
// engineer, excluding this one, or flip the case's chat_queue row back to
// WAITING_FOR_ENGINEER), except that the unresponsive engineer is taken
// OFFLINE rather than AVAILABLE or re-queued-as-available: they didn't
// respond to the original alert, so immediately handing them (or keeping
// them eligible for) another case would likely just repeat the same
// timeout. Going OFFLINE requires them to deliberately set themselves
// AVAILABLE again before they're routed anything else. A deliberate,
// conservative default -- straightforward to change to AVAILABLE here if
// that turns out to be too aggressive in practice.
//
// Meant to be called periodically by a caller that also owns delivering
// the result somewhere (see apps/csm-portal/backend's
// ChatHandler.StartTimeoutSweeper) -- this package has no resident
// background loop of its own, matching this service's "synchronous HTTP
// only, caller-driven" design (see the sdk-go routingclient package doc
// comment). Each call is a snapshot; an engineer who crosses the timeout
// between two calls is simply picked up on the next one, since updated_at
// (the staleness clock every other state transition already bumps) only
// moves forward.
func (r *Router) SweepExpiredPending(ctx context.Context, timeout time.Duration) ([]TimeoutResult, error) {
	rows, err := r.db.Query(ctx, `
		SELECT user_id, current_case_id FROM cs_engineer_status
		WHERE chat_status = 'BUSY' AND accepted_at IS NULL
		  AND updated_at < now() - make_interval(secs => $1)
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

// timeoutOne re-verifies and applies a single engineer's timeout inside its
// own transaction -- one state transition per transaction, matching every
// other Router method. Re-checks status/case/staleness under the row lock:
// SweepExpiredPending's own scan is unlocked, so the engineer may have
// already accepted, declined, or been reassigned since -- any of those
// makes this a no-op (nil, nil) instead of double-processing them.
func (r *Router) timeoutOne(ctx context.Context, userID, caseID string, timeout time.Duration) (*TimeoutResult, error) {
	var result *TimeoutResult
	err := r.withTx(ctx, func(tx pgx.Tx) error {
		var (
			status        Status
			currentCaseID *string
			currentCase   []byte
			updatedAt     time.Time
			acceptedAt    *time.Time
		)
		err := tx.QueryRow(ctx, `
			SELECT chat_status, current_case_id, current_case, updated_at, accepted_at
			FROM cs_engineer_status WHERE user_id = $1 FOR UPDATE
		`, userID).Scan(&status, &currentCaseID, &currentCase, &updatedAt, &acceptedAt)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			return nil
		case err != nil:
			return fmt.Errorf("lock engineer: %w", err)
		}
		if !isPendingAccept(status, currentCaseID != nil, acceptedAt) || currentCaseID == nil || *currentCaseID != caseID || time.Since(updatedAt) < timeout {
			return nil
		}

		var timedOut CaseInfo
		if err := json.Unmarshal(currentCase, &timedOut); err != nil {
			return fmt.Errorf("decode current case: %w", err)
		}

		if _, err := tx.Exec(ctx, `
			UPDATE cs_engineer_status
			SET chat_status = 'OFFLINE', pending_offline = false, accepted_at = NULL,
			    current_case_id = NULL, current_case = NULL,
			    available_since = NULL, updated_at = now()
			WHERE user_id = $1
		`, userID); err != nil {
			return fmt.Errorf("clear timed-out session: %w", err)
		}

		// Audit trail (see migrations/000009's own doc comment, and
		// migrations/000014's rename of this table's columns): userID's
		// ping on this conversation is now settled as TIMED_OUT.
		if _, err := tx.Exec(ctx, `
			INSERT INTO chat_queue_engineer_assignment (conversation_id, engineer_id, status)
			VALUES ($1, $2, 'TIMED_OUT')
		`, timedOut.ConversationID, userID); err != nil {
			return fmt.Errorf("record timeout outcome: %w", err)
		}

		timedOutJSON, err := json.Marshal(timedOut)
		if err != nil {
			return fmt.Errorf("marshal timed-out case: %w", err)
		}

		candidate, ok, err := popAvailableEngineer(ctx, tx, userID)
		if err != nil {
			return err
		}
		if ok {
			if err := assignCaseToEngineer(ctx, tx, candidate, timedOut, timedOutJSON); err != nil {
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
