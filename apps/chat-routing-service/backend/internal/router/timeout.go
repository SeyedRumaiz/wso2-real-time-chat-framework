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
	// Email is the engineer who was PENDING past the timeout -- taken
	// OFFLINE as a side effect (see SweepExpiredPending's doc comment for
	// why OFFLINE rather than AVAILABLE).
	Email        string    `json:"email"`
	CaseID       string    `json:"caseId"`
	ReassignedTo string    `json:"reassignedTo,omitempty"`
	Requeued     bool      `json:"requeued,omitempty"`
	AssignedCase *CaseInfo `json:"assignedCase,omitempty"`
}

// pendingCandidate is one row from SweepExpiredPending's initial, unlocked
// scan -- re-verified under lock by timeoutOne before anything changes.
type pendingCandidate struct {
	email  string
	caseID string
}

// SweepExpiredPending finds every engineer who has been PENDING on the
// same case for at least timeout and treats each one exactly like an
// explicit Decline (see Router.Decline: reassign to the next available
// engineer, excluding this one, or push back onto the FRONT of the queue),
// except that the unresponsive engineer is taken OFFLINE rather than
// AVAILABLE or re-queued-as-available: they didn't respond to the original
// alert, so immediately handing them (or keeping them eligible for)
// another case would likely just repeat the same timeout. Going OFFLINE
// requires them to deliberately set themselves AVAILABLE again before
// they're routed anything else. A deliberate, conservative default --
// straightforward to change to AVAILABLE here if that turns out to be too
// aggressive in practice.
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
		SELECT email, current_case_id FROM engineers
		WHERE status = 'PENDING' AND updated_at < now() - make_interval(secs => $1)
	`, timeout.Seconds())
	if err != nil {
		return nil, fmt.Errorf("router: scan expired pending: %w", err)
	}
	var candidates []pendingCandidate
	for rows.Next() {
		var c pendingCandidate
		if err := rows.Scan(&c.email, &c.caseID); err != nil {
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
		result, err := r.timeoutOne(ctx, c.email, c.caseID, timeout)
		if err != nil {
			return results, fmt.Errorf("router: time out %s on %s: %w", c.email, c.caseID, err)
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
func (r *Router) timeoutOne(ctx context.Context, email, caseID string, timeout time.Duration) (*TimeoutResult, error) {
	var result *TimeoutResult
	err := r.withTx(ctx, func(tx pgx.Tx) error {
		var (
			status        Status
			currentCaseID *string
			currentCase   []byte
			updatedAt     time.Time
		)
		err := tx.QueryRow(ctx, `
			SELECT status, current_case_id, current_case, updated_at
			FROM engineers WHERE email = $1 FOR UPDATE
		`, email).Scan(&status, &currentCaseID, &currentCase, &updatedAt)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			return nil
		case err != nil:
			return fmt.Errorf("lock engineer: %w", err)
		}
		if status != StatusPending || currentCaseID == nil || *currentCaseID != caseID || time.Since(updatedAt) < timeout {
			return nil
		}

		var timedOut CaseInfo
		if err := json.Unmarshal(currentCase, &timedOut); err != nil {
			return fmt.Errorf("decode current case: %w", err)
		}

		if _, err := tx.Exec(ctx, `
			UPDATE engineers
			SET status = 'OFFLINE', pending_offline = false,
			    current_case_id = NULL, current_case = NULL,
			    available_since = NULL, updated_at = now()
			WHERE email = $1
		`, email); err != nil {
			return fmt.Errorf("clear timed-out session: %w", err)
		}

		timedOutJSON, err := json.Marshal(timedOut)
		if err != nil {
			return fmt.Errorf("marshal timed-out case: %w", err)
		}

		candidate, ok, err := popAvailableEngineer(ctx, tx, email)
		if err != nil {
			return err
		}
		if ok {
			if err := assignCaseToEngineer(ctx, tx, candidate, timedOut, timedOutJSON); err != nil {
				return err
			}
			assigned := timedOut
			result = &TimeoutResult{Email: email, CaseID: caseID, ReassignedTo: candidate, AssignedCase: &assigned}
			return nil
		}

		if _, err := enqueueCase(ctx, tx, timedOut, timedOutJSON, true); err != nil {
			return err
		}
		result = &TimeoutResult{Email: email, CaseID: caseID, Requeued: true}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}
