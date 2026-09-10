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

// These are integration tests: Router is backed by a real *pgxpool.Pool,
// not an interface with a fake, so there's no way to exercise its SQL
// without a real Postgres database. newTestRouter connects using this
// service's own normal DB_* environment variables, so run these the same
// way you'd run the server itself:
//
//	set -a && source .env && set +a && go test ./...
//
// Without those env vars set, every test here calls t.Skip rather than
// failing -- that's the expected state anywhere Postgres isn't reachable
// (CI, a laptop without the dev DB set up), not a test failure.
//
// These run against the same chat_routing schema the real dev database
// uses -- there's no isolated test schema yet. That's safe because every
// test here only touches engineer user IDs and case IDs it generates
// itself (see testUserID/testCaseID) and cleans up via t.Cleanup, so it
// can never collide with a real account or a real queued case. What this
// deliberately does not test is Escalate's own engineer-selection ranking
// or queue position, since both depend on every other row in the shared
// database, which an isolated test can't control. Fixtures call
// assignCaseToEngineer/insertQueueRow directly instead of going through
// Escalate, so a test that needs "engineer X is PENDING on case Y" can
// build exactly that without picking a real engineer out of the pool.
package router

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/wso2-open-operations/cs-tools/apps/chat-routing-service/backend/internal/config"
	"github.com/wso2-open-operations/cs-tools/apps/chat-routing-service/backend/internal/db"
)

// newTestRouter connects to the database configured via this service's own
// normal environment variables and returns a Router backed by it, or skips
// the calling test if that configuration is incomplete. The pool is closed
// automatically via t.Cleanup.
func newTestRouter(t *testing.T) (*Router, *pgxpool.Pool) {
	t.Helper()

	cfg := config.LoadDB()
	if err := cfg.Validate(); err != nil {
		t.Skipf("skipping: chat-routing-service database not configured (%v)", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	pool, err := db.NewPool(ctx, cfg.DSN())
	if err != nil {
		t.Fatalf("connect to test database: %v", err)
	}
	t.Cleanup(pool.Close)

	return NewRouter(pool), pool
}

// testUserID returns a collision-free engineer user ID, registers it via
// SetPresence (so it starts OFFLINE and idle, like any engineer's first
// contact), and cleans up every row it touched once the test finishes.
func testUserID(t *testing.T, r *Router, pool *pgxpool.Pool, tag string) string {
	t.Helper()
	userID := fmt.Sprintf("router-test-%s-%d", tag, time.Now().UnixNano())
	ctx := context.Background()

	if _, err := r.SetPresence(ctx, userID, StatusOffline); err != nil {
		t.Fatalf("seed test engineer %s: %v", userID, err)
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := pool.Exec(cleanupCtx, `DELETE FROM cs_engineer_status WHERE user_id = $1`, userID); err != nil {
			t.Logf("cleanup: delete engineer %s: %v", userID, err)
		}
		if _, err := pool.Exec(cleanupCtx, `DELETE FROM chat_queue_engineer_assignment WHERE engineer_id = $1`, userID); err != nil {
			t.Logf("cleanup: delete chat_queue_engineer_assignment for %s: %v", userID, err)
		}
	})
	return userID
}

// testCaseID returns a collision-free case ID (and matching conversation
// ID) and registers cleanup of any chat_queue row left under it, so a
// failed assertion doesn't leak a row into the shared queue.
func testCaseID(t *testing.T, pool *pgxpool.Pool, tag string) string {
	t.Helper()
	caseID := fmt.Sprintf("router-test-%s-%d", tag, time.Now().UnixNano())
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := pool.Exec(cleanupCtx, `DELETE FROM chat_queue WHERE chat_conversation_id = $1`, "conv-"+caseID); err != nil {
			t.Logf("cleanup: delete chat_queue row %s: %v", caseID, err)
		}
		if _, err := pool.Exec(cleanupCtx, `DELETE FROM chat_queue_engineer_assignment WHERE conversation_id = $1`, "conv-"+caseID); err != nil {
			t.Logf("cleanup: delete chat_queue_engineer_assignment row %s: %v", caseID, err)
		}
	})
	return caseID
}

// assignFixture puts userID directly into PENDING on a freshly-built
// CaseInfo for caseID, bypassing Escalate's engineer-selection (by calling
// assignCaseToEngineer directly), and gives the case a chat_queue row in
// the ASSIGNED state, matching what Escalate would have created. userID
// must already exist (see testUserID) and not currently hold a case.
func assignFixture(t *testing.T, r *Router, userID, caseID string) CaseInfo {
	t.Helper()
	ci := CaseInfo{CaseID: caseID, ConversationID: "conv-" + caseID}
	caseInfoJSON, err := json.Marshal(ci)
	if err != nil {
		t.Fatalf("marshal fixture case info: %v", err)
	}

	err = r.withTx(context.Background(), func(tx pgx.Tx) error {
		if _, err := insertQueueRow(context.Background(), tx, ci, caseInfoJSON, queueAssigned); err != nil {
			return err
		}
		return assignCaseToEngineer(context.Background(), tx, userID, ci, caseInfoJSON)
	})
	if err != nil {
		t.Fatalf("assignFixture: %v", err)
	}
	return ci
}

// engineerRowForTest is the subset of a cs_engineer_status row assertions
// check directly, bypassing Router's own API so a test can confirm what's
// actually in the database rather than only what a method's return value
// claims. Status is the externally-visible/derived value (see
// externalStatus) -- PENDING isn't a value the database itself stores, so
// reading the raw column wouldn't tell these tests what they want to know.
type engineerRowForTest struct {
	Status        Status
	CurrentCaseID *string
}

func getEngineerRow(t *testing.T, pool *pgxpool.Pool, userID string) engineerRowForTest {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var (
		rawStatus  Status
		acceptedAt *time.Time
	)
	row := engineerRowForTest{}
	err := pool.QueryRow(ctx, `
		SELECT chat_status, current_case_id, accepted_at FROM cs_engineer_status WHERE user_id = $1
	`, userID).Scan(&rawStatus, &row.CurrentCaseID, &acceptedAt)
	if err != nil {
		t.Fatalf("read engineer row %s: %v", userID, err)
	}
	row.Status = externalStatus(rawStatus, row.CurrentCaseID != nil, acceptedAt)
	return row
}

// completedSafely calls Router.Completed for userID, and, if that drained
// a case off the shared chat_queue (result.AssignedCase != nil),
// immediately puts it back and clears it off userID directly via SQL.
// None of this file's fixtures ever leave a WAITING_FOR_ENGINEER row
// behind, so a non-nil AssignedCase here can only be a real, pre-existing
// case from the shared dev database's queue -- not test data. Without
// this, a fixture engineer rejoining AVAILABLE could silently steal a
// real customer's queued escalation and then lose it once the fixture's
// row is deleted at cleanup.
func completedSafely(t *testing.T, r *Router, pool *pgxpool.Pool, userID string) CompletedResult {
	t.Helper()
	ctx := context.Background()
	result, err := r.Completed(ctx, userID)
	if err != nil {
		t.Fatalf("Completed: %v", err)
	}
	if result.AssignedCase == nil {
		return result
	}

	t.Logf("Completed drained a real queued case (caseId=%s) onto the test fixture engineer -- restoring it to the queue", result.AssignedCase.CaseID)
	rescueCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tx, err := pool.Begin(rescueCtx)
	if err != nil {
		t.Fatalf("rescue: begin transaction: %v", err)
	}
	defer func() { _ = tx.Rollback(rescueCtx) }()
	if err := requeueWaiting(rescueCtx, tx, result.AssignedCase.ConversationID); err != nil {
		t.Fatalf("rescue: re-queue drained case: %v", err)
	}
	if _, err := tx.Exec(rescueCtx, `
		UPDATE cs_engineer_status SET chat_status = 'AVAILABLE', current_case_id = NULL, current_case = NULL,
		    available_since = now(), updated_at = now()
		WHERE user_id = $1
	`, userID); err != nil {
		t.Fatalf("rescue: clear fixture engineer's stolen case: %v", err)
	}
	if err := tx.Commit(rescueCtx); err != nil {
		t.Fatalf("rescue: commit: %v", err)
	}
	return result
}

// TestIsStuckPending is a pure unit test -- isStuckPending takes no
// database connection, so it actually runs anywhere `go test` runs. It
// checks that an OFFLINE-but-unconfirmed engineer is treated exactly like
// a BUSY-but-unconfirmed one, so SweepExpiredPending and Accept both keep
// finding them.
func TestIsStuckPending(t *testing.T) {
	now := time.Now()
	tests := []struct {
		name       string
		stored     Status
		hasCase    bool
		acceptedAt *time.Time
		want       bool
	}{
		{"busy unconfirmed with case", StatusBusy, true, nil, true},
		{"busy confirmed", StatusBusy, true, &now, false},
		{"busy with no case (shouldn't happen, but must not panic/misreport)", StatusBusy, false, nil, false},
		{"offline unconfirmed with case", StatusOffline, true, nil, true},
		{"offline confirmed", StatusOffline, true, &now, false},
		{"offline idle", StatusOffline, false, nil, false},
		{"available idle", StatusAvailable, false, nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isStuckPending(tt.stored, tt.hasCase, tt.acceptedAt); got != tt.want {
				t.Errorf("isStuckPending(%s, hasCase=%v, accepted=%v) = %v, want %v", tt.stored, tt.hasCase, tt.acceptedAt != nil, got, tt.want)
			}
		})
	}
}

// TestIsPendingAccept_ExcludesOffline checks that isPendingAccept, which
// drives the public-facing PENDING relabeling, stays narrower than
// isStuckPending: an engineer who's asked to leave must keep reading as
// OFFLINE externally, not PENDING, even while the timeout sweep and
// Accept still track their unconfirmed case.
func TestIsPendingAccept_ExcludesOffline(t *testing.T) {
	now := time.Now()
	if isPendingAccept(StatusOffline, true, nil) {
		t.Error("isPendingAccept must not treat an OFFLINE-but-unconfirmed engineer as pending accept -- they must read as OFFLINE, not PENDING")
	}
	if !isPendingAccept(StatusBusy, true, nil) {
		t.Error("isPendingAccept must still treat a BUSY, unconfirmed engineer as pending accept")
	}
	if isPendingAccept(StatusBusy, true, &now) {
		t.Error("isPendingAccept must not apply once accepted_at is set")
	}
}

func TestCompleted_NoOpWhenIdle(t *testing.T) {
	r, pool := newTestRouter(t)
	userID := testUserID(t, r, pool, "completed-idle")

	result, err := r.Completed(context.Background(), userID)
	if err != nil {
		t.Fatalf("Completed: %v", err)
	}
	if result.Removed || result.Rejoined || result.AssignedCase != nil {
		t.Errorf("Completed on an idle engineer should be a pure no-op, got %+v", result)
	}
}

func TestCompleted_NoOpForUnknownUserID(t *testing.T) {
	r, _ := newTestRouter(t)
	result, err := r.Completed(context.Background(), fmt.Sprintf("nobody-%d", time.Now().UnixNano()))
	if err != nil {
		t.Fatalf("Completed: %v", err)
	}
	if result.Removed || result.Rejoined || result.AssignedCase != nil {
		t.Errorf("Completed for a user ID with no row at all should be a pure no-op, got %+v", result)
	}
}

func TestCompleted_RejoinsAvailableWhenNeverRequestedOffline(t *testing.T) {
	r, pool := newTestRouter(t)
	userID := testUserID(t, r, pool, "completed-rejoin")
	caseID := testCaseID(t, pool, "completed-rejoin")
	assignFixture(t, r, userID, caseID)

	// If the shared queue is genuinely non-empty, this may legitimately come
	// back with Rejoined + a real AssignedCase -- completedSafely puts that
	// back afterward either way. So this only asserts on Removed/Rejoined.
	result := completedSafely(t, r, pool, userID)
	if !result.Rejoined || result.Removed {
		t.Errorf("expected Rejoined (not Removed) when OFFLINE was never requested mid-session, got %+v", result)
	}

	row := getEngineerRow(t, pool, userID)
	if row.Status != StatusAvailable || row.CurrentCaseID != nil {
		t.Errorf("expected AVAILABLE with no current case after Completed (and any rescued case put back), got status=%s currentCaseID=%v", row.Status, row.CurrentCaseID)
	}
}

func TestCompleted_GoesOfflineWhenOfflineRequestedMidSession(t *testing.T) {
	r, pool := newTestRouter(t)
	userID := testUserID(t, r, pool, "completed-offline")
	caseID := testCaseID(t, pool, "completed-offline")
	assignFixture(t, r, userID, caseID)

	presenceResult, err := r.SetPresence(context.Background(), userID, StatusOffline)
	if err != nil {
		t.Fatalf("SetPresence(OFFLINE) mid-session: %v", err)
	}
	if !presenceResult.Applied {
		t.Fatalf("expected Applied requesting OFFLINE mid-session, got %+v", presenceResult)
	}
	// Mid-session OFFLINE must not touch the session itself, but (unlike the
	// old deferred pending_offline design) must take effect on chat_status
	// immediately -- see SetPresence's own doc comment. externalStatus
	// reports that as OFFLINE, not PENDING, even though the case is still
	// unconfirmed (see isPendingAccept vs. isStuckPending in state.go).
	mid := getEngineerRow(t, pool, userID)
	if mid.CurrentCaseID == nil || *mid.CurrentCaseID != caseID {
		t.Fatalf("requesting OFFLINE mid-session must not clear the current case yet, got %+v", mid)
	}
	if mid.Status != StatusOffline {
		t.Fatalf("expected chat_status to flip to OFFLINE immediately on a mid-session OFFLINE request, got %+v", mid)
	}

	result, err := r.Completed(context.Background(), userID)
	if err != nil {
		t.Fatalf("Completed: %v", err)
	}
	if !result.Removed || result.Rejoined || result.AssignedCase != nil {
		t.Errorf("expected Removed (offline, no rejoin) once OFFLINE was requested mid-session, got %+v", result)
	}

	row := getEngineerRow(t, pool, userID)
	if row.Status != StatusOffline || row.CurrentCaseID != nil {
		t.Errorf("expected OFFLINE with no current case after Completed, got %+v", row)
	}
}

func TestCompleted_DuplicateCallIsIdempotent(t *testing.T) {
	// Regression guard: a second Completed call for a session that already
	// ended must not re-derive AVAILABLE-vs-OFFLINE and silently overwrite a
	// correct OFFLINE result back to AVAILABLE.
	r, pool := newTestRouter(t)
	userID := testUserID(t, r, pool, "completed-duplicate")
	caseID := testCaseID(t, pool, "completed-duplicate")
	assignFixture(t, r, userID, caseID)

	if _, err := r.SetPresence(context.Background(), userID, StatusOffline); err != nil {
		t.Fatalf("SetPresence(OFFLINE) mid-session: %v", err)
	}

	first, err := r.Completed(context.Background(), userID)
	if err != nil {
		t.Fatalf("first Completed: %v", err)
	}
	if !first.Removed {
		t.Fatalf("expected the first Completed call to go OFFLINE (Removed), got %+v", first)
	}

	second, err := r.Completed(context.Background(), userID)
	if err != nil {
		t.Fatalf("second Completed: %v", err)
	}
	if second.Removed || second.Rejoined || second.AssignedCase != nil {
		t.Errorf("a duplicate Completed call for an already-ended session must be a no-op, got %+v", second)
	}

	row := getEngineerRow(t, pool, userID)
	if row.Status != StatusOffline {
		t.Errorf("a duplicate Completed call must not have changed status away from OFFLINE, got %s", row.Status)
	}
}

func TestSetPresence_ChangingMindUndoesMidSessionOffline(t *testing.T) {
	r, pool := newTestRouter(t)
	userID := testUserID(t, r, pool, "presence-change-mind")
	caseID := testCaseID(t, pool, "presence-change-mind")
	assignFixture(t, r, userID, caseID)

	if _, err := r.SetPresence(context.Background(), userID, StatusOffline); err != nil {
		t.Fatalf("SetPresence(OFFLINE): %v", err)
	}
	offline := getEngineerRow(t, pool, userID)
	if offline.Status != StatusOffline {
		t.Fatalf("expected chat_status OFFLINE immediately after the mid-session request, got %+v", offline)
	}

	result, err := r.SetPresence(context.Background(), userID, StatusAvailable)
	if err != nil {
		t.Fatalf("SetPresence(AVAILABLE) to change their mind: %v", err)
	}
	if !result.Applied {
		t.Errorf("expected Applied undoing a mid-session OFFLINE request, got %+v", result)
	}

	// The session itself must still be untouched by either presence call,
	// and chat_status must be back to BUSY -- reported externally as
	// PENDING, since the case is still unconfirmed (see isPendingAccept).
	mid := getEngineerRow(t, pool, userID)
	if mid.CurrentCaseID == nil || *mid.CurrentCaseID != caseID {
		t.Fatalf("expected session untouched by SetPresence, got %+v", mid)
	}
	if mid.Status != StatusPending {
		t.Errorf("expected chat_status reverted to BUSY (external PENDING, case still unconfirmed) after changing their mind, got %+v", mid)
	}

	completedResult := completedSafely(t, r, pool, userID)
	if !completedResult.Rejoined || completedResult.Removed {
		t.Errorf("expected AVAILABLE rejoin after undoing the mid-session OFFLINE request, got %+v", completedResult)
	}
}

func TestSetPresence_RejectsDirectPendingOrBusyRequest(t *testing.T) {
	r, pool := newTestRouter(t)
	userID := testUserID(t, r, pool, "presence-reject-derived")

	for _, want := range []Status{StatusPending, StatusBusy} {
		result, err := r.SetPresence(context.Background(), userID, want)
		if err != nil {
			t.Fatalf("SetPresence(%s): %v", want, err)
		}
		if result.Applied {
			t.Errorf("directly requesting %s should never be Applied -- it's a derived-only state, got %+v", want, result)
		}
	}
}

func TestAccept_AppliesWhenPendingOnSameCase(t *testing.T) {
	r, pool := newTestRouter(t)
	userID := testUserID(t, r, pool, "accept-valid")
	caseID := testCaseID(t, pool, "accept-valid")
	assignFixture(t, r, userID, caseID)

	result, err := r.Accept(context.Background(), userID, caseID)
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	if !result.Applied {
		t.Fatalf("expected Accept to apply for the case the engineer is actually PENDING on, got %+v", result)
	}

	row := getEngineerRow(t, pool, userID)
	if row.Status != StatusBusy {
		t.Errorf("expected BUSY after a valid Accept, got %s", row.Status)
	}

	var queueRows int
	if err := pool.QueryRow(context.Background(), `SELECT COUNT(*) FROM chat_queue WHERE chat_conversation_id = $1`, "conv-"+caseID).Scan(&queueRows); err != nil {
		t.Fatalf("check queue row removed: %v", err)
	}
	if queueRows != 0 {
		t.Errorf("expected Accept to delete the case's chat_queue row, but %d remain", queueRows)
	}
}

func TestAccept_RejectsWhenIdle(t *testing.T) {
	r, pool := newTestRouter(t)
	userID := testUserID(t, r, pool, "accept-idle")

	result, err := r.Accept(context.Background(), userID, "some-case-id")
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	if result.Applied {
		t.Errorf("expected Accept to reject an engineer with no PENDING case at all, got %+v", result)
	}
}

func TestAccept_RejectsStaleCaseID(t *testing.T) {
	r, pool := newTestRouter(t)
	userID := testUserID(t, r, pool, "accept-stale")
	caseID := testCaseID(t, pool, "accept-stale")
	assignFixture(t, r, userID, caseID)

	result, err := r.Accept(context.Background(), userID, "a-different-case-id-entirely")
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	if result.Applied {
		t.Errorf("expected Accept to reject a caseID that doesn't match what the engineer is actually PENDING on, got %+v", result)
	}

	// The real, still-PENDING case must be untouched by the failed accept.
	row := getEngineerRow(t, pool, userID)
	if row.Status != StatusPending || row.CurrentCaseID == nil || *row.CurrentCaseID != caseID {
		t.Errorf("a stale Accept must not disturb the engineer's real PENDING case, got %+v", row)
	}
}

func TestAccept_RejectsDoubleAccept(t *testing.T) {
	r, pool := newTestRouter(t)
	userID := testUserID(t, r, pool, "accept-double")
	caseID := testCaseID(t, pool, "accept-double")
	assignFixture(t, r, userID, caseID)

	first, err := r.Accept(context.Background(), userID, caseID)
	if err != nil || !first.Applied {
		t.Fatalf("first Accept should apply, got %+v, err=%v", first, err)
	}

	second, err := r.Accept(context.Background(), userID, caseID)
	if err != nil {
		t.Fatalf("second Accept: %v", err)
	}
	if second.Applied {
		t.Errorf("a second Accept for a case already BUSY (not PENDING) must not apply again, got %+v", second)
	}
}

// TestAccept_AppliesWhenOfflineButUnconfirmed is the integration-level
// counterpart to TestIsStuckPending: it checks that Accept applies for an
// OFFLINE-but-unconfirmed engineer without forcing chat_status back to
// BUSY, against a real database connection.
func TestAccept_AppliesWhenOfflineButUnconfirmed(t *testing.T) {
	r, pool := newTestRouter(t)
	userID := testUserID(t, r, pool, "accept-offline-unconfirmed")
	caseID := testCaseID(t, pool, "accept-offline-unconfirmed")
	assignFixture(t, r, userID, caseID)

	if _, err := r.SetPresence(context.Background(), userID, StatusOffline); err != nil {
		t.Fatalf("SetPresence(OFFLINE) mid-session: %v", err)
	}
	offline := getEngineerRow(t, pool, userID)
	if offline.Status != StatusOffline {
		t.Fatalf("expected chat_status OFFLINE immediately after the mid-session request, got %+v", offline)
	}

	result, err := r.Accept(context.Background(), userID, caseID)
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	if !result.Applied {
		t.Fatalf("expected Accept to apply for an OFFLINE-but-unconfirmed engineer's own case, got %+v", result)
	}

	// Accept must not force chat_status back to BUSY -- the engineer's
	// stated intent to leave (see SetPresence) must survive the accept and
	// only be honored once the session actually ends (see Completed).
	row := getEngineerRow(t, pool, userID)
	if row.Status != StatusOffline || row.CurrentCaseID == nil || *row.CurrentCaseID != caseID {
		t.Errorf("expected Accept to leave chat_status OFFLINE (not force BUSY) while keeping the current case, got %+v", row)
	}

	var queueRows int
	if err := pool.QueryRow(context.Background(), `SELECT COUNT(*) FROM chat_queue WHERE chat_conversation_id = $1`, "conv-"+caseID).Scan(&queueRows); err != nil {
		t.Fatalf("check queue row removed: %v", err)
	}
	if queueRows != 0 {
		t.Errorf("expected Accept to delete the case's chat_queue row even for an OFFLINE-but-unconfirmed engineer, but %d remain", queueRows)
	}

	// Ending the session now must remove them (OFFLINE, no rejoin) --
	// Completed's existing OFFLINE branch, exercised once more here right
	// after an Accept in between.
	completedResult, err := r.Completed(context.Background(), userID)
	if err != nil {
		t.Fatalf("Completed: %v", err)
	}
	if !completedResult.Removed || completedResult.Rejoined {
		t.Errorf("expected Completed to remove (not rejoin) an engineer who went OFFLINE before accepting, got %+v", completedResult)
	}
}

func TestDecline_NoOpForStaleCaseID(t *testing.T) {
	r, pool := newTestRouter(t)
	userID := testUserID(t, r, pool, "decline-stale")
	caseID := testCaseID(t, pool, "decline-stale")
	assignFixture(t, r, userID, caseID)

	result, err := r.Decline(context.Background(), userID, "a-different-case-id-entirely")
	if err != nil {
		t.Fatalf("Decline: %v", err)
	}
	if result.ReassignedTo != "" || result.Requeued || result.AssignedCase != nil {
		t.Errorf("Decline for a caseID the engineer isn't actually holding must be a no-op, got %+v", result)
	}

	// The real case must be untouched.
	row := getEngineerRow(t, pool, userID)
	if row.Status != StatusPending || row.CurrentCaseID == nil || *row.CurrentCaseID != caseID {
		t.Errorf("a stale Decline must not disturb the engineer's real PENDING case, got %+v", row)
	}
}

// Decline's reassignment path (as opposed to the no-op tested above) is
// deliberately not exercised here: it can hand the declined case to
// whichever OTHER engineer is currently AVAILABLE in the shared dev
// database, which could be a real account (see this file's package doc
// comment on Escalate's own selection logic for the same reason) --
// polluting that real engineer's live current_case with
// a fake test case is a worse outcome than the coverage gap. That path was
// exercised manually via curl during this feature's own testing instead
// (see the project's live-engineer-chat-escalation-summary.md).

func TestEnqueue_WaitingFlipsToAssignedAndKeepsCreatedAt(t *testing.T) {
	r, pool := newTestRouter(t)
	ctx := context.Background()

	backID := testCaseID(t, pool, "enqueue-back")
	frontID := testCaseID(t, pool, "enqueue-front")
	backCase := CaseInfo{CaseID: backID, ConversationID: "conv-" + backID}
	frontCase := CaseInfo{CaseID: frontID, ConversationID: "conv-" + frontID}
	backJSON, _ := json.Marshal(backCase)
	frontJSON, _ := json.Marshal(frontCase)

	var backCreatedAt, frontCreatedAt time.Time
	err := r.withTx(ctx, func(tx pgx.Tx) error {
		if _, err := insertQueueRow(ctx, tx, backCase, backJSON, queueWaitingForEngineer); err != nil {
			return err
		}
		if _, err := insertQueueRow(ctx, tx, frontCase, frontJSON, queueWaitingForEngineer); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		t.Fatalf("enqueue fixture: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM chat_queue WHERE chat_conversation_id IN ($1, $2)`, backCase.ConversationID, frontCase.ConversationID)
	})

	if err := pool.QueryRow(ctx, `SELECT created_at FROM chat_queue WHERE chat_conversation_id = $1`, backCase.ConversationID).Scan(&backCreatedAt); err != nil {
		t.Fatalf("read back created_at: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT created_at FROM chat_queue WHERE chat_conversation_id = $1`, frontCase.ConversationID).Scan(&frontCreatedAt); err != nil {
		t.Fatalf("read front created_at: %v", err)
	}
	if !backCreatedAt.Before(frontCreatedAt) && backCreatedAt != frontCreatedAt {
		t.Fatalf("expected backCase to have been enqueued before frontCase, got back=%v front=%v", backCreatedAt, frontCreatedAt)
	}

	// requeueWaiting must flip a claimed row back to WAITING_FOR_ENGINEER
	// without touching created_at, so it keeps its original place ahead of
	// anything that arrives later. Exercised directly against backCase's own
	// row rather than via claimOldestWaiting, since the real queue could
	// claim a different row first.
	err = r.withTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE chat_queue SET status = 'ASSIGNED' WHERE chat_conversation_id = $1`, backCase.ConversationID); err != nil {
			return err
		}
		return requeueWaiting(ctx, tx, backCase.ConversationID)
	})
	if err != nil {
		t.Fatalf("simulate assign+requeue: %v", err)
	}

	var afterCreatedAt time.Time
	var status queueStatus
	if err := pool.QueryRow(ctx, `SELECT created_at, status FROM chat_queue WHERE chat_conversation_id = $1`, backCase.ConversationID).Scan(&afterCreatedAt, &status); err != nil {
		t.Fatalf("read back row after requeue: %v", err)
	}
	if status != queueWaitingForEngineer {
		t.Errorf("expected backCase to be WAITING_FOR_ENGINEER again after requeueWaiting, got %s", status)
	}
	if !afterCreatedAt.Equal(backCreatedAt) {
		t.Errorf("expected requeueWaiting to leave created_at untouched, got before=%v after=%v", backCreatedAt, afterCreatedAt)
	}
}

// TestTimeoutOne_ReassignsOfflineButUnconfirmedEngineer is the regression
// test for the bug pending_offline's removal fixes: an engineer who went
// OFFLINE mid-session before confirming their case would otherwise drop
// out of the timeout sweep entirely once OFFLINE started taking immediate
// effect on chat_status, stranding their case. Calls timeoutOne directly
// (not the full sweep) so this only ever touches its own fixture engineer,
// never a real one in the shared dev database. Backdates updated_at via
// SQL instead of waiting out a real timeout.
func TestTimeoutOne_ReassignsOfflineButUnconfirmedEngineer(t *testing.T) {
	r, pool := newTestRouter(t)
	ctx := context.Background()
	userID := testUserID(t, r, pool, "timeout-offline")
	caseID := testCaseID(t, pool, "timeout-offline")
	assignFixture(t, r, userID, caseID)

	if _, err := r.SetPresence(ctx, userID, StatusOffline); err != nil {
		t.Fatalf("SetPresence(OFFLINE) mid-session: %v", err)
	}
	offline := getEngineerRow(t, pool, userID)
	if offline.Status != StatusOffline {
		t.Fatalf("expected chat_status OFFLINE immediately after the mid-session request, got %+v", offline)
	}

	if _, err := pool.Exec(ctx, `
		UPDATE cs_engineer_status SET updated_at = now() - interval '1 hour' WHERE user_id = $1
	`, userID); err != nil {
		t.Fatalf("backdate updated_at to simulate a real timeout: %v", err)
	}

	result, err := r.timeoutOne(ctx, userID, caseID, time.Minute)
	if err != nil {
		t.Fatalf("timeoutOne: %v", err)
	}
	if result == nil {
		t.Fatalf("expected timeoutOne to fire for an OFFLINE-but-unconfirmed engineer past the timeout -- this is exactly the case pending_offline's removal could have silently dropped from the sweep")
	}
	if result.ReassignedTo != "" {
		// Might be a real engineer in the shared dev database -- rescue them
		// exactly like completedSafely does elsewhere in this file, so this
		// test can never leave a real account holding fake test data.
		reassignedTo := result.ReassignedTo
		t.Cleanup(func() {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if _, err := pool.Exec(cleanupCtx, `
				UPDATE cs_engineer_status
				SET chat_status = 'AVAILABLE', accepted_at = NULL, current_case_id = NULL, current_case = NULL,
				    available_since = now(), updated_at = now()
				WHERE user_id = $1
			`, reassignedTo); err != nil {
				t.Logf("rescue: restore reassigned engineer %s: %v", reassignedTo, err)
			}
		})
	}

	row := getEngineerRow(t, pool, userID)
	if row.Status != StatusOffline || row.CurrentCaseID != nil {
		t.Errorf("expected the timed-out engineer to end up OFFLINE with no current case, got %+v", row)
	}
}
