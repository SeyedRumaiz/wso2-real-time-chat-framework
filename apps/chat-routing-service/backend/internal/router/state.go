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
// service's own PostgreSQL database (see migrations/). Every exported
// method runs as a single transaction, matching the "one state transition,
// one atomic unit" shape the original in-memory prototype's single mutex
// gave for free -- see each method's own doc comment for what it does
// inside that transaction.
type Router struct {
	db *pgxpool.Pool
}

// NewRouter constructs a Router backed by db. Does not itself run
// migrations -- see migrations/ and this service's README for how to apply
// them; db.Ping has already been called by internal/db.NewPool by the time
// this is constructed (see cmd/server/main.go).
func NewRouter(db *pgxpool.Pool) *Router {
	return &Router{db: db}
}

// withTx runs fn inside a transaction, committing on success and rolling
// back (a no-op if fn already committed nothing) on any error, including a
// panic recovered elsewhere up the stack.
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

// queueStatus mirrors chat_queue.status (see migrations/
// 000015_redesign_chat_queue.up.sql) -- WAITING_FOR_ENGINEER for a case
// nobody has been assigned yet, ASSIGNED from the moment an engineer is
// (whether immediately, at Escalate time, or later via a queue drain or a
// Decline/timeout reassignment) until Router.Accept confirms them, at
// which point the row is deleted outright rather than moving to a third
// status.
type queueStatus string

const (
	queueWaitingForEngineer queueStatus = "WAITING_FOR_ENGINEER"
	queueAssigned           queueStatus = "ASSIGNED"
)

// EscalateResult is Escalate's outcome: exactly one of EngineerUserID (set)
// or Queued (true) applies.
type EscalateResult struct {
	// EngineerUserID is the IdP "userid" claim of the engineer this case was
	// assigned to (see migrations/000014_rename_engineer_status_table for
	// why this is a user ID rather than an email) -- empty when Queued.
	EngineerUserID string `json:"engineerId,omitempty"`
	Queued         bool   `json:"queued,omitempty"`
	// Position is 1-based ("you are #1 in the queue"), only meaningful when
	// Queued is true.
	Position int `json:"position,omitempty"`
}

// Escalate assigns c to whichever AVAILABLE engineer has taken the fewest
// chats today (ties broken by who has been AVAILABLE the longest -- see
// popAvailableEngineer), or appends it to the waiting queue if nobody
// qualifies. A per-customer "sticky" preference used to be tried first here
// (see customer_engineer_assignments) -- dropped per the 2026-09-07 DB
// schema review (see migrations/000013's own doc comment): it was fully
// derivable and, per that review's own conclusion, unnecessary state to
// maintain.
//
// A chat_queue row is created for c regardless of the outcome (ASSIGNED or
// WAITING_FOR_ENGINEER) and lives until Router.Accept confirms the
// engineer -- see migrations/000015_redesign_chat_queue's own doc comment
// for why.
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
	// PendingOffline echoes the engineer's resulting PendingOffline flag --
	// meaningful only when they were mid-session at the time of the call.
	PendingOffline bool `json:"pendingOffline,omitempty"`
	// AssignedCase is set when this presence change immediately drained the
	// queue (transitioning to AVAILABLE with a non-empty queue assigns the
	// head to this same engineer).
	AssignedCase *CaseInfo `json:"assignedCase,omitempty"`
}

// SetPresence applies an engineer's requested status change, creating their
// row (defaulting to OFFLINE) on first contact if this is the first time
// this service has heard from them. userID is the IdP's stable per-account
// "userid" claim (see migrations/000014_rename_engineer_status_table and
// csm-portal/backend's internal/middleware.UserInfo.UserID) -- this
// service's cs_engineer_status table is keyed by it directly (no separate
// email is stored any more), so unlike before this migration there is
// nothing else the caller needs to supply on first contact.
//
// Mid-session (current_case_id IS NOT NULL): capacity is a single dedicated
// session, so the session itself is never affected here. The only thing a
// request can change is pending_offline -- requesting OFFLINE sets it (the
// engineer will be removed once Completed(userID) is called instead of
// rejoining the pool); requesting AVAILABLE or BUSY clears it (the engineer
// changed their mind about leaving).
//
// Idle: AVAILABLE joins the pool (available_since = now()) and, if the
// queue is non-empty, immediately claims and assigns the oldest waiting
// case (the engineer goes straight to PENDING with that case -- not yet
// Busy, see Accept -- still counted as "applied" rather than a separate
// step the caller has to notice). OFFLINE leaves/stays out of the pool.
// Neither PENDING nor BUSY is a valid direct request -- both are derived
// states this method only ever produces as a side effect (PENDING via the
// AVAILABLE queue-drain above, BUSY only via Accept), never something a
// caller asks for; a request for either here is a no-op (Applied: false),
// the same as any other unrecognized status.
func (r *Router) SetPresence(ctx context.Context, userID string, want Status) (PresenceResult, error) {
	var result PresenceResult
	err := r.withTx(ctx, func(tx pgx.Tx) error {
		row, err := ensureAndLockEngineer(ctx, tx, userID)
		if err != nil {
			return err
		}

		if row.CurrentCaseID != nil {
			pendingOffline := want == StatusOffline
			if _, err := tx.Exec(ctx, `
				UPDATE cs_engineer_status SET pending_offline = $1, updated_at = now()
				WHERE user_id = $2
			`, pendingOffline, userID); err != nil {
				return fmt.Errorf("update pending_offline: %w", err)
			}
			result = PresenceResult{Applied: true, PendingOffline: pendingOffline}
			return nil
		}

		switch want {
		case StatusAvailable:
			if _, err := tx.Exec(ctx, `
				UPDATE cs_engineer_status
				SET chat_status = 'AVAILABLE', pending_offline = false,
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
				SET chat_status = 'OFFLINE', pending_offline = false,
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
	// Removed is true when the engineer had requested OFFLINE while
	// mid-session (pending_offline) -- they are now fully OFFLINE and did
	// NOT rejoin the available pool or receive a queued case. This is the
	// "removed after the current session completes" requirement.
	Removed bool `json:"removed,omitempty"`
	// Rejoined is true when the engineer went back to AVAILABLE (the
	// non-Removed path) -- possibly immediately BUSY again if AssignedCase
	// is also set.
	Rejoined     bool      `json:"rejoined,omitempty"`
	AssignedCase *CaseInfo `json:"assignedCase,omitempty"`
}

// Completed clears the engineer's current case (the session they just
// ended) and either removes them entirely (if they'd asked to go OFFLINE
// while busy) or returns them to AVAILABLE -- immediately assigning the
// next queued case to them, if any, exactly like SetPresence's own
// queue-drain-on-AVAILABLE behavior. A no-op (zero CompletedResult) if
// userID has no row at all, OR if userID has no current_case_id right now
// -- the latter guards against a duplicate/stale call for a session that
// was already completed (e.g. csm-portal/backend's HandleCompleteSession
// being hit twice for the same case, which can happen client-side if a UI
// briefly resurrects a just-ended session from a stale cached read before
// its own refetch lands -- see EngineerAlertNotification.tsx). Without this
// guard, a second call here would re-derive AVAILABLE-vs-OFFLINE from
// pending_offline's value AFTER the first call already reset it to false,
// silently overwriting whatever that first call correctly decided.
func (r *Router) Completed(ctx context.Context, userID string) (CompletedResult, error) {
	var result CompletedResult
	err := r.withTx(ctx, func(tx pgx.Tx) error {
		var (
			pendingOffline bool
			currentCaseID  *string
		)
		err := tx.QueryRow(ctx, `
			SELECT pending_offline, current_case_id FROM cs_engineer_status WHERE user_id = $1 FOR UPDATE
		`, userID).Scan(&pendingOffline, &currentCaseID)
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

		if pendingOffline {
			if _, err := tx.Exec(ctx, `
				UPDATE cs_engineer_status
				SET chat_status = 'OFFLINE', pending_offline = false, accepted_at = NULL,
				    current_case_id = NULL, current_case = NULL,
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
// decline itself was a no-op (the engineer wasn't actually holding that
// case), in which case all three are zero.
type DeclineResult struct {
	// ReassignedTo is the user ID of the engineer the case was handed to
	// instead (see migrations/000014_rename_engineer_status_table).
	ReassignedTo string `json:"reassignedTo,omitempty"`
	Requeued     bool   `json:"requeued,omitempty"`
	// AssignedCase is the declined case, set alongside ReassignedTo so the
	// caller (csm-portal/backend) can deliver it to that other engineer the
	// same way a fresh escalation would be, without having to remember the
	// case's own fields itself.
	AssignedCase *CaseInfo `json:"assignedCase,omitempty"`
}

// Decline handles an engineer dismissing a case they were just assigned
// (before accepting it). Treats the decline like Completed for the
// declining engineer (same pending_offline handling), then tries to hand
// caseID to whichever other AVAILABLE engineer has handled the fewest
// chats today (same fallback ranking Escalate uses, excluding the
// decliner); if none are free, flips the case's existing chat_queue row
// back to WAITING_FOR_ENGINEER (see migrations/000015_redesign_chat_queue's
// own doc comment for why this keeps the case's original queue position --
// the customer already waited once, so they should not end up behind newer
// arrivals). A no-op (zero DeclineResult) if userID has no row, or isn't
// currently holding caseID.
func (r *Router) Decline(ctx context.Context, userID, caseID string) (DeclineResult, error) {
	var result DeclineResult
	err := r.withTx(ctx, func(tx pgx.Tx) error {
		var (
			currentCaseID  *string
			currentCase    []byte
			pendingOffline bool
		)
		err := tx.QueryRow(ctx, `
			SELECT current_case_id, current_case, pending_offline
			FROM cs_engineer_status WHERE user_id = $1 FOR UPDATE
		`, userID).Scan(&currentCaseID, &currentCase, &pendingOffline)
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

		if pendingOffline {
			if _, err := tx.Exec(ctx, `
				UPDATE cs_engineer_status
				SET chat_status = 'OFFLINE', pending_offline = false, accepted_at = NULL,
				    current_case_id = NULL, current_case = NULL,
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

		// Audit trail (see migrations/000009's own doc comment, and
		// migrations/000014's rename of this table's columns): userID's
		// ping on this conversation is now settled as REJECTED, independent
		// of whatever happens to the case next (reassigned to someone else,
		// or requeued).
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
	// Applied is true when userID was PENDING on exactly caseID and has now
	// been flipped to BUSY. False means the accept is stale -- the case
	// was already declined/reassigned/requeued out from under this
	// engineer (or they never held it at all) -- and the caller (csm-
	// portal/backend's HandleAcceptSession) should surface a "this
	// request is no longer available" response rather than proceeding.
	Applied bool `json:"applied"`
}

// Accept confirms userID is actually accepting the case they were
// assigned: only flips PENDING -> BUSY (available_since stays NULL,
// current_case* untouched -- this is a pure status change) when userID's
// current_case_id still equals caseID and their status is still PENDING.
// Any other state -- OFFLINE/AVAILABLE (declined or reassigned already),
// already BUSY (accept already applied, e.g. a duplicate click or a
// rehydrated retry), or PENDING on a *different* case -- reports
// Applied: false rather than erroring, since "the thing you tried to
// accept isn't there anymore" is an expected race (a customer-side
// timeout, another path already resolving it), not a server fault.
//
// Also deletes the case's chat_queue row -- per the 2026-09-07 DB schema
// review, a chat_queue row lives from Escalate until exactly this moment
// (see migrations/000015_redesign_chat_queue's own doc comment), not from
// Escalate until mere assignment.
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
		if !isPendingAccept(status, currentCaseID != nil, acceptedAt) || currentCaseID == nil || *currentCaseID != caseID {
			result = AcceptResult{}
			return nil
		}

		var accepted CaseInfo
		if err := json.Unmarshal(currentCase, &accepted); err != nil {
			return fmt.Errorf("decode current case: %w", err)
		}

		if _, err := tx.Exec(ctx, `
			UPDATE cs_engineer_status SET chat_status = 'BUSY', accepted_at = now(), updated_at = now() WHERE user_id = $1
		`, userID); err != nil {
			return fmt.Errorf("accept case: %w", err)
		}

		if err := deleteQueueRow(ctx, tx, accepted.ConversationID); err != nil {
			return err
		}

		// Audit trail (see migrations/000009's own doc comment, and
		// migrations/000014's rename of this table's columns): userID's
		// ping on this conversation is now settled as CONNECTED.
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
		// because of a stand-in bookkeeping gap. setConversationEngineer
		// only returns an error for an actual database failure.
		if err := setConversationEngineer(ctx, tx, caseID, userID); err != nil {
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

// PresenceDetail is GetPresence's result: the status alone was enough for
// the "initialize the dropdown correctly" use case it was built for, but a
// PENDING or BUSY engineer's browser session can be lost independently of
// this server-side state (a refresh, a closed tab) with nothing left in
// the UI to act on it -- CurrentCase lets a caller rehydrate that lost
// alert/session instead of leaving the engineer stuck with no way to
// accept, decline, or end it.
type PresenceDetail struct {
	Status      Status
	CurrentCase *CaseInfo
	// PendingSince is when this engineer entered PENDING -- the same
	// updated_at column SweepExpiredPending measures staleness against
	// (see timeout.go) -- nil unless Status is PENDING. Lets a caller
	// (the frontend's pending-alert countdown) compute how much longer
	// until a timeout sweep reassigns this case even after losing local
	// timer state to a refresh or a closed tab.
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
	UserID         string    `json:"userId"`
	Status         Status    `json:"status"`
	PendingOffline bool      `json:"pendingOffline"`
	CurrentCase    *CaseInfo `json:"currentCase,omitempty"`
	// ChatsToday is a live COUNT(*) over chat_conversation for today (see
	// migrations/000011_drop_assignment_log.up.sql) -- 0 if the engineer
	// hasn't accepted a case yet today, computed fresh on every call rather
	// than read from a stored column.
	ChatsToday int `json:"chatsToday"`
}

// DebugState is the full dump GET /route/debug/state returns.
// Verification-only -- no real caller (csm-portal/backend or otherwise)
// depends on this endpoint; it exists purely so the end-to-end test plan
// can assert on internal state directly instead of inferring it from SSE
// side effects alone.
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
		SELECT e.user_id, e.chat_status, e.pending_offline, e.current_case, e.accepted_at,
		       COALESCE(t.today_count, 0)
		FROM cs_engineer_status e
		LEFT JOIN (
			SELECT engineer_id AS user_id, COUNT(*) AS today_count
			FROM chat_conversation
			WHERE engineer_id IS NOT NULL
			  AND updated_at >= CURRENT_DATE AND updated_at < CURRENT_DATE + 1
			GROUP BY engineer_id
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
			userID         string
			status         Status
			pendingOffline bool
			currentCase    []byte
			acceptedAt     *time.Time
			chatsToday     int
		)
		if err := rows.Scan(&userID, &status, &pendingOffline, &currentCase, &acceptedAt, &chatsToday); err != nil {
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
			UserID: userID, Status: externalStatus(status, currentCase != nil, acceptedAt), PendingOffline: pendingOffline, CurrentCase: cc,
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
			SELECT engineer_id AS user_id, COUNT(*) AS today_count
			FROM chat_conversation
			WHERE engineer_id IS NOT NULL
			  AND updated_at >= CURRENT_DATE AND updated_at < CURRENT_DATE + 1
			GROUP BY engineer_id
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
	CurrentCaseID *string
}

// ensureAndLockEngineer makes sure userID has a row (defaulting to OFFLINE,
// exactly like the original in-memory Router's getOrCreate), then locks and
// returns it FOR UPDATE for the rest of tx. userID is itself this table's
// primary key (see migrations/000014_rename_engineer_status_table) -- no
// separate identifier is required to create a first-contact row any more.
func ensureAndLockEngineer(ctx context.Context, tx pgx.Tx, userID string) (engineerRow, error) {
	if _, err := tx.Exec(ctx, `
		INSERT INTO cs_engineer_status (user_id) VALUES ($1)
		ON CONFLICT (user_id) DO NOTHING
	`, userID); err != nil {
		return engineerRow{}, fmt.Errorf("ensure engineer row: %w", err)
	}

	var row engineerRow
	if err := tx.QueryRow(ctx, `
		SELECT current_case_id FROM cs_engineer_status WHERE user_id = $1 FOR UPDATE
	`, userID).Scan(&row.CurrentCaseID); err != nil {
		return engineerRow{}, fmt.Errorf("lock engineer row: %w", err)
	}
	return row, nil
}

// popAvailableEngineer locks and returns an AVAILABLE, idle engineer other
// than exclude (pass "" to exclude no one): whichever has been assigned the
// fewest chats today, ties broken by who has been AVAILABLE the longest --
// or ok=false if none are free. "Fewest chats today" is a dynamic COUNT(*)
// over chat_conversation (engineer_id, updated_at) rather than a stored
// counter -- see migrations/000011_drop_assignment_log.up.sql's own doc
// comment for why this now counts chats actually ACCEPTED today rather
// than merely assigned (a real, deliberate behavior change, not just a
// refactor of where the count lives). FOR UPDATE OF e SKIP LOCKED (scoped
// to the cs_engineer_status side of the join, since the count comes from an
// aggregate subquery that isn't itself lockable) lets concurrent callers
// each grab a different engineer instead of blocking on each other. Used by
// Escalate directly and by Decline's reassignment fallback.
func popAvailableEngineer(ctx context.Context, tx pgx.Tx, exclude string) (userID string, ok bool, err error) {
	err = tx.QueryRow(ctx, `
		SELECT e.user_id
		FROM cs_engineer_status e
		LEFT JOIN (
			SELECT engineer_id AS user_id, COUNT(*) AS today_count
			FROM chat_conversation
			WHERE engineer_id IS NOT NULL
			  AND updated_at >= CURRENT_DATE AND updated_at < CURRENT_DATE + 1
			GROUP BY engineer_id
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

// isPendingAccept reports whether a BUSY engineer's current case has not
// actually been confirmed yet (Router.Accept hasn't run for it). PENDING
// is no longer a stored value of cs_engineer_status.chat_status (see
// migrations/000012_remove_pending_status.up.sql's own doc comment for the
// full reasoning) -- assignCaseToEngineer sets chat_status = 'BUSY'
// immediately, the same instant capacity is reserved, exactly as it always
// has; what used to be a fourth PENDING enum value is this check instead,
// against the accepted_at column that migration added.
func isPendingAccept(stored Status, hasCase bool, acceptedAt *time.Time) bool {
	return stored == StatusBusy && hasCase && acceptedAt == nil
}

// externalStatus is the Status every caller outside this package should
// see: StatusPending instead of the row's actual stored BUSY value exactly
// when isPendingAccept is true, otherwise the stored value unchanged. Every
// public-facing read (GetPresence, DebugState) goes through this rather
// than the raw column, so PENDING keeps behaving like a real fourth status
// to every existing caller (this service's own HTTP handlers, the SDK,
// csm-portal/backend, the CSM portal frontend) even though it is no longer
// one in the database.
func externalStatus(stored Status, hasCase bool, acceptedAt *time.Time) Status {
	if isPendingAccept(stored, hasCase, acceptedAt) {
		return StatusPending
	}
	return stored
}

// assignCaseToEngineer marks userID BUSY with c as their current case,
// reserving their capacity immediately -- accepted_at stays NULL until
// Router.Accept confirms it, which is what makes this read back as PENDING
// rather than genuinely BUSY to every external caller (see externalStatus
// above) until then. Per-customer "sticky" bookkeeping used to happen here
// too -- dropped along with customer_engineer_assignments, see
// migrations/000013's own doc comment. Historical "chats assigned/accepted"
// ranking has no dedicated log to write here either -- see
// popAvailableEngineer's own doc comment for where that now comes from.
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

// insertQueueRow creates c's chat_queue row (see migrations/
// 000015_redesign_chat_queue's own doc comment) with the given initial
// status -- WAITING_FOR_ENGINEER when Escalate found nobody free,
// ASSIGNED when it assigned someone immediately. Only meaningful for a
// WAITING_FOR_ENGINEER row, position is c's 1-based place among every
// currently-waiting row, by (created_at, chat_conversation_id) order (0 for
// an ASSIGNED row, since nothing is waiting on it).
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
// uses. Unlike the pre-000015 popQueueHead this replaces, the row is
// updated in place, not deleted -- see migrations/
// 000015_redesign_chat_queue's own doc comment for why the row now lives
// until Router.Accept instead.
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
// other engineer is free to take the case over immediately. The row's
// created_at is untouched, so it naturally keeps its original place ahead
// of every case that arrived after it -- see migrations/
// 000015_redesign_chat_queue's own doc comment. The row is assumed to
// already exist: every case gets one at Escalate time and it is only ever
// removed by Router.Accept, which this engineer -- by definition, since
// they're declining or timing out -- has not called.
func requeueWaiting(ctx context.Context, tx pgx.Tx, conversationID string) error {
	if _, err := tx.Exec(ctx, `
		UPDATE chat_queue SET status = 'WAITING_FOR_ENGINEER' WHERE chat_conversation_id = $1
	`, conversationID); err != nil {
		return fmt.Errorf("requeue case: %w", err)
	}
	return nil
}

// deleteQueueRow removes conversationID's chat_queue row outright -- called
// only from Router.Accept, the one point in this case's lifecycle where
// this table's own doc comment says the row should finally go away.
func deleteQueueRow(ctx context.Context, tx pgx.Tx, conversationID string) error {
	if _, err := tx.Exec(ctx, `DELETE FROM chat_queue WHERE chat_conversation_id = $1`, conversationID); err != nil {
		return fmt.Errorf("delete queue row: %w", err)
	}
	return nil
}
