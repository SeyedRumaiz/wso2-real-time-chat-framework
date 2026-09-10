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
	"github.com/jackc/pgx/v5/pgxpool"
)

// Router is the engineer-availability/queue state machine, backed by this
// service's own PostgreSQL database. Every exported method runs as a
// single transaction, so each state transition is atomic.
type Router struct {
	db *pgxpool.Pool
}

// NewRouter constructs a Router backed by db. Doesn't run migrations
// itself -- see migrations/ and the README for that.
func NewRouter(db *pgxpool.Pool) *Router {
	return &Router{db: db}
}

// withTx runs fn inside a transaction, committing on success and rolling
// back on any error (a no-op if fn already committed nothing).
func (r *Router) withTx(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("router: begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op once committed

	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("router: commit transaction: %w", err)
	}
	return nil
}

// queueStatus mirrors chat_queue.status: WAITING_FOR_ENGINEER for a case
// nobody's been assigned yet, ASSIGNED from the moment someone is (whether
// immediately, via a queue drain, or a Decline/timeout reassignment) until
// Router.Accept confirms them and the row is deleted.
type queueStatus string

const (
	queueWaitingForEngineer queueStatus = "WAITING_FOR_ENGINEER"
	queueAssigned           queueStatus = "ASSIGNED"
)

// EscalateResult is Escalate's outcome: exactly one of EngineerUserID (set)
// or Queued (true) applies.
type EscalateResult struct {
	// EngineerUserID is the IdP "userid" claim of the engineer this case was
	// assigned to -- empty when Queued.
	EngineerUserID string `json:"engineerId,omitempty"`
	Queued         bool   `json:"queued,omitempty"`
	// Position is 1-based ("you are #1 in the queue"), only meaningful when
	// Queued is true.
	Position int `json:"position,omitempty"`
}

// Escalate assigns c to whichever AVAILABLE engineer has taken the fewest
// chats today (ties broken by who's been AVAILABLE longest -- see
// popAvailableEngineer), or appends it to the waiting queue if nobody
// qualifies.
//
// A chat_queue row is created for c either way (ASSIGNED or
// WAITING_FOR_ENGINEER) and lives until Router.Accept confirms the
// engineer.
func (r *Router) Escalate(ctx context.Context, c CaseInfo) (EscalateResult, error) {
	caseInfoJSON, err := json.Marshal(c)
	if err != nil {
		return EscalateResult{}, fmt.Errorf("router: marshal case info: %w", err)
	}

	var result EscalateResult
	err = r.withTx(ctx, func(tx pgx.Tx) error {
		userID, ok, err := popAvailableEngineer(ctx, tx, "")
		if err != nil {
			return err
		}
		if !ok {
			position, err := insertQueueRow(ctx, tx, c, caseInfoJSON, queueWaitingForEngineer)
			if err != nil {
				return err
			}
			result = EscalateResult{Queued: true, Position: position}
			return nil
		}

		if _, err := insertQueueRow(ctx, tx, c, caseInfoJSON, queueAssigned); err != nil {
			return err
		}
		if err := assignCaseToEngineer(ctx, tx, userID, c, caseInfoJSON); err != nil {
			return err
		}
		result = EscalateResult{EngineerUserID: userID}
		return nil
	})
	if err != nil {
		return EscalateResult{}, err
	}
	return result, nil
}

// PresenceResult is SetPresence's outcome.
type PresenceResult struct {
	Applied bool `json:"applied"`
	// AssignedCase is set when this presence change immediately drained the
	// queue (transitioning to AVAILABLE with a non-empty queue assigns the
	// head to this same engineer).
	AssignedCase *CaseInfo `json:"assignedCase,omitempty"`
}

// SetPresence applies an engineer's requested status change, creating their
// row (defaulting to OFFLINE) on first contact. userID is the IdP's stable
// per-account "userid" claim -- cs_engineer_status is keyed by it directly,
// so there's nothing else a caller needs to supply.
//
// Mid-session (current_case_id IS NOT NULL): requesting OFFLINE takes
// effect immediately -- chat_status flips to OFFLINE right away, while
// current_case_id/current_case/accepted_at are left untouched, so the
// engineer keeps whatever they're already holding (PENDING or BUSY) until
// it's resolved via Accept, Decline, Completed, or a timeout. Requesting
// AVAILABLE or BUSY while mid-session undoes an earlier OFFLINE request by
// flipping chat_status back to BUSY; a no-op if they were never OFFLINE.
// Completed and Decline both check chat_status when the session actually
// ends: OFFLINE means don't rejoin the pool or take a queued case, anything
// else means return to AVAILABLE. Accept and the timeout sweep (see
// isStuckPending, and SweepExpiredPending in timeout.go) both still treat
// an OFFLINE engineer holding an unconfirmed case like a BUSY/unconfirmed
// one, so a case never gets stranded just because the engineer asked to
// leave before anyone confirmed it.
//
// Idle: AVAILABLE joins the pool and, if the queue is non-empty,
// immediately claims and assigns the oldest waiting case (the engineer
// goes straight to PENDING with it, not yet BUSY -- see Accept). OFFLINE
// leaves/stays out of the pool. PENDING and BUSY aren't valid direct
// requests -- both are states this method only ever produces as a side
// effect -- so requesting either here is a no-op, same as any other
// unrecognized status.
func (r *Router) SetPresence(ctx context.Context, userID string, want Status) (PresenceResult, error) {
	var result PresenceResult
	err := r.withTx(ctx, func(tx pgx.Tx) error {
		row, err := ensureAndLockEngineer(ctx, tx, userID)
		if err != nil {
			return err
		}

		if row.CurrentCaseID != nil {
			if want == StatusOffline {
				if row.ChatStatus != StatusOffline {
					if _, err := tx.Exec(ctx, `
						UPDATE cs_engineer_status SET chat_status = 'OFFLINE', updated_at = now()
						WHERE user_id = $1
					`, userID); err != nil {
						return fmt.Errorf("set offline mid-session: %w", err)
					}
				}
				result = PresenceResult{Applied: true}
				return nil
			}

			// Anything else mid-session means "stay" -- undo an earlier
			// OFFLINE request if one is in effect; otherwise this is a no-op
			// (the session itself is never touched by a presence request).
			if row.ChatStatus == StatusOffline {
				if _, err := tx.Exec(ctx, `
					UPDATE cs_engineer_status SET chat_status = 'BUSY', updated_at = now()
					WHERE user_id = $1
				`, userID); err != nil {
					return fmt.Errorf("undo mid-session offline request: %w", err)
				}
			}
			result = PresenceResult{Applied: true}
			return nil
		}

		switch want {
		case StatusAvailable:
			if _, err := tx.Exec(ctx, `
				UPDATE cs_engineer_status
				SET chat_status = 'AVAILABLE',
				    available_since = now(), updated_at = now()
				WHERE user_id = $1
			`, userID); err != nil {
				return fmt.Errorf("set available: %w", err)
			}

			c, ok, err := claimOldestWaiting(ctx, tx)
			if err != nil {
				return err
			}
			if !ok {
				result = PresenceResult{Applied: true}
				return nil
			}

			caseInfoJSON, err := json.Marshal(c)
			if err != nil {
				return fmt.Errorf("marshal queued case: %w", err)
			}
			if err := assignCaseToEngineer(ctx, tx, userID, c, caseInfoJSON); err != nil {
				return err
			}
			assigned := c
			result = PresenceResult{Applied: true, AssignedCase: &assigned}
			return nil

		case StatusOffline:
			if _, err := tx.Exec(ctx, `
				UPDATE cs_engineer_status
				SET chat_status = 'OFFLINE',
				    available_since = NULL, updated_at = now()
				WHERE user_id = $1
			`, userID); err != nil {
				return fmt.Errorf("set offline: %w", err)
			}
			result = PresenceResult{Applied: true}
			return nil

		default:
			result = PresenceResult{Applied: false}
			return nil
		}
	})
	if err != nil {
		return PresenceResult{}, err
	}
	return result, nil
}

// CompletedResult is Completed's outcome.
type CompletedResult struct {
	// Removed is true when the engineer had requested OFFLINE mid-session --
	// they're not rejoining the pool or getting a queued case.
	Removed bool `json:"removed,omitempty"`
	// Rejoined is true when the engineer went back to AVAILABLE, possibly
	// immediately BUSY again if AssignedCase is also set.
	Rejoined     bool      `json:"rejoined,omitempty"`
	AssignedCase *CaseInfo `json:"assignedCase,omitempty"`
}

// Completed clears the engineer's current case and either removes them
// entirely (if chat_status is already OFFLINE) or returns them to
// AVAILABLE, immediately assigning the next queued case if there is one. A
// no-op if userID has no row, or has no current_case_id right now -- the
// latter guards against a duplicate call for a session that already ended
// (a UI can fire this twice for the same case). Without that guard, a
// second call would re-derive AVAILABLE-vs-OFFLINE from chat_status after
// the first call had already reset it, silently flipping a correct OFFLINE
// result back to AVAILABLE.
func (r *Router) Completed(ctx context.Context, userID string) (CompletedResult, error) {
	var result CompletedResult
	err := r.withTx(ctx, func(tx pgx.Tx) error {
		var (
			chatStatus    Status
			currentCaseID *string
		)
		err := tx.QueryRow(ctx, `
			SELECT chat_status, current_case_id FROM cs_engineer_status WHERE user_id = $1 FOR UPDATE
		`, userID).Scan(&chatStatus, &currentCaseID)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			result = CompletedResult{}
			return nil
		case err != nil:
			return fmt.Errorf("lock engineer: %w", err)
		}
		if currentCaseID == nil {
			result = CompletedResult{}
			return nil
		}

		if chatStatus == StatusOffline {
			if _, err := tx.Exec(ctx, `
				UPDATE cs_engineer_status
				SET accepted_at = NULL, current_case_id = NULL, current_case = NULL,
				    available_since = NULL, updated_at = now()
				WHERE user_id = $1
			`, userID); err != nil {
				return fmt.Errorf("clear session (removed): %w", err)
			}
			result = CompletedResult{Removed: true}
			return nil
		}

		if _, err := tx.Exec(ctx, `
			UPDATE cs_engineer_status
			SET chat_status = 'AVAILABLE', accepted_at = NULL, current_case_id = NULL, current_case = NULL,
			    available_since = now(), updated_at = now()
			WHERE user_id = $1
		`, userID); err != nil {
			return fmt.Errorf("clear session (rejoin): %w", err)
		}

		c, ok, err := claimOldestWaiting(ctx, tx)
		if err != nil {
			return err
		}
		if !ok {
			result = CompletedResult{Rejoined: true}
			return nil
		}

		caseInfoJSON, err := json.Marshal(c)
		if err != nil {
			return fmt.Errorf("marshal queued case: %w", err)
		}
		if err := assignCaseToEngineer(ctx, tx, userID, c, caseInfoJSON); err != nil {
			return err
		}
		assigned := c
		result = CompletedResult{Rejoined: true, AssignedCase: &assigned}
		return nil
	})
	if err != nil {
		return CompletedResult{}, err
	}
	return result, nil
}

// DeclineResult is Decline's outcome: exactly one of ReassignedTo (set,
// with AssignedCase also set) or Requeued (true) applies, unless the
// decline itself was a no-op, in which case all three are zero.
type DeclineResult struct {
	// ReassignedTo is the user ID of the engineer the case was handed to
	// instead.
	ReassignedTo string `json:"reassignedTo,omitempty"`
	Requeued     bool   `json:"requeued,omitempty"`
	// AssignedCase is the declined case, set alongside ReassignedTo so the
	// caller can deliver it to that other engineer without having to
	// remember the case's own fields itself.
	AssignedCase *CaseInfo `json:"assignedCase,omitempty"`
}

// Decline handles an engineer dismissing a case they were just assigned,
// before accepting it. Treats it like Completed for the declining engineer,
// then tries to hand caseID to whichever other AVAILABLE engineer has
// handled the fewest chats today (same ranking Escalate uses, excluding the
// decliner); if none are free, flips the case's chat_queue row back to
// WAITING_FOR_ENGINEER, keeping its original queue position rather than
// sending the customer to the back of the line a second time. A no-op if
// userID has no row, or isn't currently holding caseID.
func (r *Router) Decline(ctx context.Context, userID, caseID string) (DeclineResult, error) {
	var result DeclineResult
	err := r.withTx(ctx, func(tx pgx.Tx) error {
		var (
			currentCaseID *string
			currentCase   []byte
			chatStatus    Status
		)
		err := tx.QueryRow(ctx, `
			SELECT current_case_id, current_case, chat_status
			FROM cs_engineer_status WHERE user_id = $1 FOR UPDATE
		`, userID).Scan(&currentCaseID, &currentCase, &chatStatus)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			result = DeclineResult{}
			return nil
		case err != nil:
			return fmt.Errorf("lock engineer: %w", err)
		}
		if currentCaseID == nil || *currentCaseID != caseID {
			result = DeclineResult{}
			return nil
		}

		var declined CaseInfo
		if err := json.Unmarshal(currentCase, &declined); err != nil {
			return fmt.Errorf("decode current case: %w", err)
		}

		if chatStatus == StatusOffline {
			if _, err := tx.Exec(ctx, `
				UPDATE cs_engineer_status
				SET accepted_at = NULL, current_case_id = NULL, current_case = NULL,
				    available_since = NULL, updated_at = now()
				WHERE user_id = $1
			`, userID); err != nil {
				return fmt.Errorf("clear declined session (offline): %w", err)
			}
		} else {
			if _, err := tx.Exec(ctx, `
				UPDATE cs_engineer_status
				SET chat_status = 'AVAILABLE', accepted_at = NULL, current_case_id = NULL, current_case = NULL,
				    available_since = now(), updated_at = now()
				WHERE user_id = $1
			`, userID); err != nil {
				return fmt.Errorf("clear declined session (available): %w", err)
			}
		}

		// Audit trail: userID's ping on this conversation is settled as
		// REJECTED, independent of what happens to the case next.
		if _, err := tx.Exec(ctx, `
			INSERT INTO chat_queue_engineer_assignment (conversation_id, engineer_id, status)
			VALUES ($1, $2, 'REJECTED')
		`, declined.ConversationID, userID); err != nil {
			return fmt.Errorf("record decline outcome: %w", err)
		}

		declinedJSON, err := json.Marshal(declined)
		if err != nil {
			return fmt.Errorf("marshal declined case: %w", err)
		}

		candidate, ok, err := popAvailableEngineer(ctx, tx, userID)
		if err != nil {
			return err
		}
		if ok {
			if err := assignCaseToEngineer(ctx, tx, candidate, declined, declinedJSON); err != nil {
				return err
			}
			assigned := declined
			result = DeclineResult{ReassignedTo: candidate, AssignedCase: &assigned}
			return nil
		}

		if err := requeueWaiting(ctx, tx, declined.ConversationID); err != nil {
			return err
		}
		result = DeclineResult{Requeued: true}
		return nil
	})
	if err != nil {
		return DeclineResult{}, err
	}
	return result, nil
}

// AcceptResult is Accept's outcome.
type AcceptResult struct {
	// Applied is true when userID had an unconfirmed case on exactly caseID
	// and has now been confirmed on it. False means the accept is stale --
	// the case was already declined/reassigned/requeued, or never held at
	// all -- and the caller should surface a "no longer available" response
	// rather than proceeding.
	Applied bool `json:"applied"`
}

// Accept confirms userID is actually accepting the case they were
// assigned: sets accepted_at when their current_case_id still equals
// caseID and nobody's confirmed it yet (see isStuckPending). Deliberately
// not gated on chat_status being exactly BUSY -- an engineer who requested
// OFFLINE mid-session before confirming this case can still accept it,
// and doing so leaves chat_status exactly as it already is (BUSY or
// OFFLINE) instead of forcing it back to BUSY, so their stated intent to
// leave survives the accept and is honored once the session ends (see
// Completed). Any other state reports Applied: false rather than erroring
// -- "the thing you tried to accept isn't there anymore" is an expected
// race, not a server fault.
//
// Also deletes the case's chat_queue row -- a row lives from Escalate
// until exactly this moment, not until mere assignment.
func (r *Router) Accept(ctx context.Context, userID, caseID string) (AcceptResult, error) {
	var result AcceptResult
	err := r.withTx(ctx, func(tx pgx.Tx) error {
		var (
			status        Status
			currentCaseID *string
			currentCase   []byte
			acceptedAt    *time.Time
		)
		err := tx.QueryRow(ctx, `
			SELECT chat_status, current_case_id, current_case, accepted_at
			FROM cs_engineer_status WHERE user_id = $1 FOR UPDATE
		`, userID).Scan(&status, &currentCaseID, &currentCase, &acceptedAt)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			result = AcceptResult{}
			return nil
		case err != nil:
			return fmt.Errorf("lock engineer: %w", err)
		}
		if !isStuckPending(status, currentCaseID != nil, acceptedAt) || currentCaseID == nil || *currentCaseID != caseID {
			result = AcceptResult{}
			return nil
		}

		var accepted CaseInfo
		if err := json.Unmarshal(currentCase, &accepted); err != nil {
			return fmt.Errorf("decode current case: %w", err)
		}

		if _, err := tx.Exec(ctx, `
			UPDATE cs_engineer_status SET accepted_at = now(), updated_at = now() WHERE user_id = $1
		`, userID); err != nil {
			return fmt.Errorf("accept case: %w", err)
		}

		if err := deleteQueueRow(ctx, tx, accepted.ConversationID); err != nil {
			return err
		}

		// Audit trail: userID's ping on this conversation is settled as
		// CONNECTED.
		if _, err := tx.Exec(ctx, `
			INSERT INTO chat_queue_engineer_assignment (conversation_id, engineer_id, status)
			VALUES ($1, $2, 'CONNECTED')
		`, accepted.ConversationID, userID); err != nil {
			return fmt.Errorf("record accept outcome: %w", err)
		}

		// Local stand-in persistence (see workitem.go) -- records the
		// accepting engineer on chat_conversation in the SAME transaction
		// as the PENDING -> BUSY flip, so the two can never disagree about
		// whether an accept actually went through. A caseID with no
		// chat_conversation row (this stand-in added after some in-flight
		// cases already existed, or CreateWorkItem's own best-effort call
		// having failed) is a plain UPDATE-matches-zero-rows no-op here,
		// not an error -- the real presence flip above must not fail
		// because of a stand-in bookkeeping gap. setConversationAssignee
		// only returns an error for an actual database failure.
		if err := setConversationAssignee(ctx, tx, caseID, userID); err != nil {
			return err
		}
		result = AcceptResult{Applied: true}
		return nil
	})
	if err != nil {
		return AcceptResult{}, err
	}
	return result, nil
}

// PresenceDetail is GetPresence's result. CurrentCase lets a caller
// rehydrate a lost pending alert or active session (a refresh, a closed
// tab) instead of leaving the engineer stuck with nothing to act on.
type PresenceDetail struct {
	Status      Status
	CurrentCase *CaseInfo
	// PendingSince is when this engineer entered PENDING (nil unless Status
	// is PENDING), so a caller can compute how much longer until the
	// timeout sweep reassigns this case even after losing local timer
	// state. Not set for an engineer who requested OFFLINE before
	// confirming their case -- their Status reads OFFLINE, not PENDING,
	// even though the timeout sweep is still tracking them underneath.
	PendingSince *time.Time
}

// GetPresence returns userID's current status and, when PENDING or BUSY,
// the case they are currently on (nil otherwise) -- defaulting to
// OFFLINE/no case for an engineer this database has never seen a presence
// update from.
func (r *Router) GetPresence(ctx context.Context, userID string) (PresenceDetail, error) {
	var (
		status      Status
		currentCase []byte
		updatedAt   time.Time
		acceptedAt  *time.Time
	)
	err := r.db.QueryRow(ctx, `SELECT chat_status, current_case, updated_at, accepted_at FROM cs_engineer_status WHERE user_id = $1`, userID).
		Scan(&status, &currentCase, &updatedAt, &acceptedAt)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return PresenceDetail{Status: StatusOffline}, nil
	case err != nil:
		return PresenceDetail{}, fmt.Errorf("router: get presence: %w", err)
	}
	detail := PresenceDetail{Status: externalStatus(status, currentCase != nil, acceptedAt)}
	if isPendingAccept(status, currentCase != nil, acceptedAt) {
		since := updatedAt
		detail.PendingSince = &since
	}
	if currentCase != nil {
		var c CaseInfo
		if err := json.Unmarshal(currentCase, &c); err != nil {
			return PresenceDetail{}, fmt.Errorf("router: get presence: decode current case: %w", err)
		}
		detail.CurrentCase = &c
	}
	return detail, nil
}

// DebugEngineer is one engineer's row in DebugState's dump.
type DebugEngineer struct {
	UserID      string    `json:"userId"`
	Status      Status    `json:"status"`
	CurrentCase *CaseInfo `json:"currentCase,omitempty"`
	// ChatsToday is a live COUNT(*) over chat_conversation for today,
	// computed fresh on every call rather than read from a stored column.
	ChatsToday int `json:"chatsToday"`
}

// DebugState is the full dump GET /route/debug/state returns.
// Verification-only -- lets tests assert on internal state directly
// instead of inferring it from SSE side effects.
type DebugState struct {
	Engineers []DebugEngineer `json:"engineers"`
	Available []string        `json:"available"`
	Queue     []CaseInfo      `json:"queue"`
}

// DebugState snapshots the current engineer registry, available pool, and
// waiting queue. Three separate read-only queries rather than one
// transaction -- this is a debug/test endpoint, not a state transition, so
// a slightly stale cross-query view is an acceptable tradeoff for not
// holding locks.
func (r *Router) DebugState(ctx context.Context) (DebugState, error) {
	engineers, err := r.debugEngineers(ctx)
	if err != nil {
		return DebugState{}, err
	}
	available, err := r.debugAvailable(ctx)
	if err != nil {
		return DebugState{}, err
	}
	queue, err := r.debugQueue(ctx)
	if err != nil {
		return DebugState{}, err
	}
	return DebugState{Engineers: engineers, Available: available, Queue: queue}, nil
}

func (r *Router) debugEngineers(ctx context.Context) ([]DebugEngineer, error) {
	rows, err := r.db.Query(ctx, `
		SELECT e.user_id, e.chat_status, e.current_case, e.accepted_at,
		       COALESCE(t.today_count, 0)
		FROM cs_engineer_status e
		LEFT JOIN (
			SELECT assignee_id AS user_id, COUNT(*) AS today_count
			FROM chat_conversation
			WHERE assignee_id IS NOT NULL
			  AND updated_at >= CURRENT_DATE AND updated_at < CURRENT_DATE + 1
			GROUP BY assignee_id
		) t ON t.user_id = e.user_id
		ORDER BY e.user_id
	`)
	if err != nil {
		return nil, fmt.Errorf("debug state: query engineers: %w", err)
	}
	defer rows.Close()

	engineers := []DebugEngineer{}
	for rows.Next() {
		var (
			userID      string
			status      Status
			currentCase []byte
			acceptedAt  *time.Time
			chatsToday  int
		)
		if err := rows.Scan(&userID, &status, &currentCase, &acceptedAt, &chatsToday); err != nil {
			return nil, fmt.Errorf("debug state: scan engineer: %w", err)
		}
		var cc *CaseInfo
		if currentCase != nil {
			var c CaseInfo
			if err := json.Unmarshal(currentCase, &c); err != nil {
				return nil, fmt.Errorf("debug state: decode current case: %w", err)
			}
			cc = &c
		}
		engineers = append(engineers, DebugEngineer{
			UserID: userID, Status: externalStatus(status, currentCase != nil, acceptedAt), CurrentCase: cc,
			ChatsToday: chatsToday,
		})
	}
	return engineers, rows.Err()
}

func (r *Router) debugAvailable(ctx context.Context) ([]string, error) {
	rows, err := r.db.Query(ctx, `
		SELECT e.user_id
		FROM cs_engineer_status e
		LEFT JOIN (
			SELECT assignee_id AS user_id, COUNT(*) AS today_count
			FROM chat_conversation
			WHERE assignee_id IS NOT NULL
			  AND updated_at >= CURRENT_DATE AND updated_at < CURRENT_DATE + 1
			GROUP BY assignee_id
		) t ON t.user_id = e.user_id
		WHERE e.chat_status = 'AVAILABLE' AND e.current_case_id IS NULL
		ORDER BY COALESCE(t.today_count, 0) ASC, e.available_since ASC
	`)
	if err != nil {
		return nil, fmt.Errorf("debug state: query available: %w", err)
	}
	defer rows.Close()

	available := []string{}
	for rows.Next() {
		var userID string
		if err := rows.Scan(&userID); err != nil {
			return nil, fmt.Errorf("debug state: scan available: %w", err)
		}
		available = append(available, userID)
	}
	return available, rows.Err()
}

func (r *Router) debugQueue(ctx context.Context) ([]CaseInfo, error) {
	rows, err := r.db.Query(ctx, `
		SELECT case_info FROM chat_queue
		WHERE status = 'WAITING_FOR_ENGINEER'
		ORDER BY created_at ASC, chat_conversation_id ASC
	`)
	if err != nil {
		return nil, fmt.Errorf("debug state: query queue: %w", err)
	}
	defer rows.Close()

	queue := []CaseInfo{}
	for rows.Next() {
		var caseInfoJSON []byte
		if err := rows.Scan(&caseInfoJSON); err != nil {
			return nil, fmt.Errorf("debug state: scan queue row: %w", err)
		}
		var c CaseInfo
		if err := json.Unmarshal(caseInfoJSON, &c); err != nil {
			return nil, fmt.Errorf("debug state: decode queued case: %w", err)
		}
		queue = append(queue, c)
	}
	return queue, rows.Err()
}

// engineerRow is the subset of a cs_engineer_status row SetPresence needs,
// locked FOR UPDATE for the rest of its transaction.
type engineerRow struct {
	ChatStatus    Status
	CurrentCaseID *string
}

// ensureAndLockEngineer makes sure userID has a row (defaulting to
// OFFLINE), then locks and returns it FOR UPDATE for the rest of tx.
func ensureAndLockEngineer(ctx context.Context, tx pgx.Tx, userID string) (engineerRow, error) {
	if _, err := tx.Exec(ctx, `
		INSERT INTO cs_engineer_status (user_id) VALUES ($1)
		ON CONFLICT (user_id) DO NOTHING
	`, userID); err != nil {
		return engineerRow{}, fmt.Errorf("ensure engineer row: %w", err)
	}

	var row engineerRow
	if err := tx.QueryRow(ctx, `
		SELECT chat_status, current_case_id FROM cs_engineer_status WHERE user_id = $1 FOR UPDATE
	`, userID).Scan(&row.ChatStatus, &row.CurrentCaseID); err != nil {
		return engineerRow{}, fmt.Errorf("lock engineer row: %w", err)
	}
	return row, nil
}

// popAvailableEngineer locks and returns an AVAILABLE, idle engineer other
// than exclude (pass "" to exclude no one): whichever has taken the fewest
// chats today, ties broken by who's been AVAILABLE longest -- or ok=false
// if none are free. "Fewest chats today" counts chats actually accepted
// today (not merely assigned), via a live COUNT(*) over chat_conversation
// rather than a stored counter. FOR UPDATE OF e SKIP LOCKED (scoped to the
// cs_engineer_status side of the join, since the count comes from an
// aggregate subquery that isn't itself lockable) lets concurrent callers
// each grab a different engineer instead of blocking on each other. Used
// by Escalate directly and by Decline's reassignment fallback.
func popAvailableEngineer(ctx context.Context, tx pgx.Tx, exclude string) (userID string, ok bool, err error) {
	err = tx.QueryRow(ctx, `
		SELECT e.user_id
		FROM cs_engineer_status e
		LEFT JOIN (
			SELECT assignee_id AS user_id, COUNT(*) AS today_count
			FROM chat_conversation
			WHERE assignee_id IS NOT NULL
			  AND updated_at >= CURRENT_DATE AND updated_at < CURRENT_DATE + 1
			GROUP BY assignee_id
		) t ON t.user_id = e.user_id
		WHERE e.chat_status = 'AVAILABLE' AND e.current_case_id IS NULL AND e.user_id != $1
		ORDER BY COALESCE(t.today_count, 0) ASC, e.available_since ASC
		LIMIT 1
		FOR UPDATE OF e SKIP LOCKED
	`, exclude).Scan(&userID)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return "", false, nil
	case err != nil:
		return "", false, fmt.Errorf("pop available engineer: %w", err)
	default:
		return userID, true, nil
	}
}

// isPendingAccept reports whether a BUSY engineer's current case hasn't
// actually been confirmed yet (Router.Accept hasn't run for it). PENDING
// isn't a stored chat_status value -- assignCaseToEngineer sets chat_status
// = 'BUSY' immediately, the instant capacity is reserved, and this checks
// accepted_at instead. Deliberately BUSY-only, unlike isStuckPending below:
// an engineer who's asked to leave (chat_status = 'OFFLINE') should read as
// OFFLINE everywhere external, not PENDING, even while the timeout sweep is
// still tracking their unconfirmed case.
func isPendingAccept(stored Status, hasCase bool, acceptedAt *time.Time) bool {
	return stored == StatusBusy && hasCase && acceptedAt == nil
}

// isStuckPending reports whether userID's current case still has nobody
// confirmed on it, and so still needs the timeout/reassignment safety net
// (see SweepExpiredPending in timeout.go) and must still be acceptable via
// Accept. Broader than isPendingAccept above: it also covers an engineer
// who requested OFFLINE (see SetPresence) before ever confirming this same
// case. Both "still BUSY, never asked to leave" and "asked to leave, but
// nobody's confirmed the case yet" are the same underlying problem --
// nobody has actually taken this case -- so both get the same safety net.
func isStuckPending(stored Status, hasCase bool, acceptedAt *time.Time) bool {
	return hasCase && acceptedAt == nil && (stored == StatusBusy || stored == StatusOffline)
}

// externalStatus is the Status every caller outside this package should
// see: StatusPending instead of the row's actual BUSY value when
// isPendingAccept is true, otherwise the stored value unchanged. Every
// public-facing read goes through this instead of the raw column, so
// PENDING keeps behaving like a real status to every caller even though
// it's not one in the database. An OFFLINE engineer holding an unconfirmed
// case (see isStuckPending) is deliberately reported as OFFLINE here, not
// PENDING -- their intent to leave should be visible immediately, even
// though the timeout sweep is still watching their case underneath.
func externalStatus(stored Status, hasCase bool, acceptedAt *time.Time) Status {
	if isPendingAccept(stored, hasCase, acceptedAt) {
		return StatusPending
	}
	return stored
}

// assignCaseToEngineer marks userID BUSY with c as their current case,
// reserving their capacity immediately. accepted_at stays NULL until
// Accept confirms it, which is what makes this read back as PENDING rather
// than genuinely BUSY (see externalStatus) until then.
func assignCaseToEngineer(ctx context.Context, tx pgx.Tx, userID string, c CaseInfo, caseInfoJSON []byte) error {
	if _, err := tx.Exec(ctx, `
		UPDATE cs_engineer_status
		SET chat_status = 'BUSY', accepted_at = NULL, current_case_id = $1, current_case = $2::jsonb,
		    available_since = NULL, updated_at = now()
		WHERE user_id = $3
	`, c.CaseID, caseInfoJSON, userID); err != nil {
		return fmt.Errorf("assign case to engineer: %w", err)
	}
	return nil
}

// insertQueueRow creates c's chat_queue row with the given initial status
// -- WAITING_FOR_ENGINEER when Escalate found nobody free, ASSIGNED when
// it assigned someone immediately. position is c's 1-based place among
// every currently-waiting row (0 for an ASSIGNED row, since nothing's
// waiting on it) -- only meaningful for a WAITING_FOR_ENGINEER row.
func insertQueueRow(ctx context.Context, tx pgx.Tx, c CaseInfo, caseInfoJSON []byte, status queueStatus) (position int, err error) {
	var createdAt time.Time
	if err := tx.QueryRow(ctx, `
		INSERT INTO chat_queue (chat_conversation_id, case_info, status)
		VALUES ($1, $2::jsonb, $3)
		RETURNING created_at
	`, c.ConversationID, caseInfoJSON, status).Scan(&createdAt); err != nil {
		return 0, fmt.Errorf("insert queue row: %w", err)
	}
	if status != queueWaitingForEngineer {
		return 0, nil
	}

	if err := tx.QueryRow(ctx, `
		SELECT COUNT(*) FROM chat_queue
		WHERE status = 'WAITING_FOR_ENGINEER'
		  AND (created_at, chat_conversation_id) <= ($1, $2)
	`, createdAt, c.ConversationID).Scan(&position); err != nil {
		return 0, fmt.Errorf("compute queue position: %w", err)
	}
	return position, nil
}

// claimOldestWaiting flips the oldest WAITING_FOR_ENGINEER row (by
// created_at, then chat_conversation_id to break ties) to ASSIGNED and
// returns its case, or ok=false if nothing is waiting. FOR UPDATE SKIP
// LOCKED inside the subquery, same job-queue idiom popAvailableEngineer
// uses. The row is updated in place, not deleted -- it lives until Accept.
func claimOldestWaiting(ctx context.Context, tx pgx.Tx) (CaseInfo, bool, error) {
	var caseInfoJSON []byte
	err := tx.QueryRow(ctx, `
		UPDATE chat_queue
		SET status = 'ASSIGNED'
		WHERE chat_conversation_id = (
			SELECT chat_conversation_id FROM chat_queue
			WHERE status = 'WAITING_FOR_ENGINEER'
			ORDER BY created_at ASC, chat_conversation_id ASC
			LIMIT 1
			FOR UPDATE SKIP LOCKED
		)
		RETURNING case_info
	`).Scan(&caseInfoJSON)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return CaseInfo{}, false, nil
	case err != nil:
		return CaseInfo{}, false, fmt.Errorf("claim oldest waiting case: %w", err)
	}

	var c CaseInfo
	if err := json.Unmarshal(caseInfoJSON, &c); err != nil {
		return CaseInfo{}, false, fmt.Errorf("decode queued case: %w", err)
	}
	return c, true, nil
}

// requeueWaiting flips conversationID's existing chat_queue row back to
// WAITING_FOR_ENGINEER -- used by Decline and SweepExpiredPending when no
// other engineer is free to take the case over immediately. created_at is
// left untouched, so the case keeps its original place ahead of anything
// that arrived after it. The row is assumed to already exist: every case
// gets one at Escalate time, removed only by Accept -- which, by
// definition, hasn't happened for a case that's being declined or timed
// out.
func requeueWaiting(ctx context.Context, tx pgx.Tx, conversationID string) error {
	if _, err := tx.Exec(ctx, `
		UPDATE chat_queue SET status = 'WAITING_FOR_ENGINEER' WHERE chat_conversation_id = $1
	`, conversationID); err != nil {
		return fmt.Errorf("requeue case: %w", err)
	}
	return nil
}

// deleteQueueRow removes conversationID's chat_queue row outright --
// called only from Accept, the one point where the row should go away.
func deleteQueueRow(ctx context.Context, tx pgx.Tx, conversationID string) error {
	if _, err := tx.Exec(ctx, `DELETE FROM chat_queue WHERE chat_conversation_id = $1`, conversationID); err != nil {
		return fmt.Errorf("delete queue row: %w", err)
	}
	return nil
}
