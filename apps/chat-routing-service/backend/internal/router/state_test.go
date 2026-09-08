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

// These are integration tests: Router is backed by a real *pgxpool.Pool
// (see NewRouter), not an interface with a fake implementation, so there is
// no way to exercise its actual SQL without a real PostgreSQL database.
// newTestRouter below connects using this service's own normal DB_HOST/
// DB_PORT/DB_USER/DB_PASSWORD/DB_NAME/DB_SSLMODE environment variables (see
// internal/config.LoadDB and internal/db.NewPool -- the exact same
// connection setup cmd/server/main.go uses), so run these the same way you
// run the server itself, e.g. from this directory:
//
//	set -a && source .env && set +a && go test ./...
//
// If those env vars aren't set, every test here calls t.Skip rather than
// failing -- "no database configured" is a normal, expected state for
// `go test ./...` run somewhere without Postgres reachable (CI, a laptop
// that hasn't set up the dev DB yet), not a test failure.
//
// These tests run against the SAME chat_routing schema your real dev
// database and manual curl testing use -- there is no isolated test schema
// (yet). That's safe because every test here operates only on engineer
// user IDs and case IDs it generates itself (see testUserID/testCaseID) and
// cleans up after itself via t.Cleanup, so it can never touch or be
// confused with a real engineer row (like a live account) or a real queued
// case. What it deliberately does NOT test is Router.Escalate's own
// engineer-selection algorithm (least-busy-today ranking) or the exact
// position a case lands at in the shared queue -- both depend on the full
// set of every engineer/queue row currently in the shared dev database,
// which a test generating its own isolated rows has no way to control or
// predict. Fixture setup below calls assignCaseToEngineer/insertQueueRow
// directly (this file is in package router, so it can) specifically to
// sidestep that ambiguity: a test that needs "engineer X is PENDING on case
// Y" creates exactly that, without going through Escalate's
// pick-any-available-engineer path at all. Escalate's own selection logic
// is exercised by the manual multi-engineer curl walkthrough instead (see
// the project's live-engineer-chat-escalation-summary.md).
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
		t.Skipf("skipping: chat-routing-service database not configured (%v) -- see this file's package doc comment", err)
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

// testUserID returns a collision-free engineer user ID for the calling
// test, registers it (via SetPresence, so it starts OFFLINE and idle like
// any engineer's first-ever contact -- see Router.SetPresence's own doc
// comment), and cleans up every row this test's use of it could have
// touched (cs_engineer_status, chat_queue_engineer_assignment) once the
// test finishes, pass or fail.
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
// ID) for the calling test and registers cleanup of any chat_queue row left
// under it (a test that expects its case to be dequeued during the test
// doesn't need this to do anything at cleanup time; one that leaves a case
// queued -- e.g. because an assertion failed first -- won't leak a row into
// the shared queue).
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
// CaseInfo for caseID, bypassing Escalate's engineer-selection entirely
// (calling the same unexported assignCaseToEngineer Escalate itself uses,
// directly -- see this file's package doc comment for why), and gives the
// case a chat_queue row in the ASSIGNED state, matching what Escalate
// itself would have created (see migrations/000015_redesign_chat_queue's
// own doc comment: every case gets a queue row from Escalate until
// Accept). userID must already exist (see testUserID) and not currently
// hold a case.
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
// below check directly, bypassing Router's own public API so a test can
// confirm what's actually in the database rather than only what a method's
// return value claims. Status here is the externally-visible/derived value
// (see externalStatus in state.go) -- PENDING is no longer a value the
// database itself stores (see migrations/000012_remove_pending_status.up.sql),
// so reading the raw column would no longer tell these tests what they
// actually want to know.
type engineerRowForTest struct {
	Status         Status
	CurrentCaseID  *string
	PendingOffline bool
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
		SELECT chat_status, current_case_id, pending_offline, accepted_at FROM cs_engineer_status WHERE user_id = $1
	`, userID).Scan(&rawStatus, &row.CurrentCaseID, &row.PendingOffline, &acceptedAt)
	if err != nil {
		t.Fatalf("read engineer row %s: %v", userID, err)
	}
	row.Status = externalStatus(rawStatus, row.CurrentCaseID != nil, acceptedAt)
	return row
}

// completedSafely calls Router.Completed for userID, and, if that happened
// to drain a case off the shared chat_queue (result.AssignedCase !=
// nil), immediately flips it straight back to WAITING_FOR_ENGINEER and
// clears it off userID directly via SQL -- bypassing Decline/SetPresence
// entirely to avoid recursively risking the exact same problem. None of
// this file's fixtures ever leave a WAITING_FOR_ENGINEER row behind before
// calling this, so a non-nil AssignedCase here can only be a real,
// pre-existing case that was genuinely waiting in the shared dev
// database's queue (see this file's package doc comment) -- not test data.
// Without this, a test's fixture engineer rejoining AVAILABLE could
// silently steal a real customer's queued escalation, then lose it
// entirely once that fixture engineer's row is deleted at test cleanup.
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

func TestCompleted_RejoinsAvailableWhenNotPendingOffline(t *testing.T) {
	r, pool := newTestRouter(t)
	userID := testUserID(t, r, pool, "completed-rejoin")
	caseID := testCaseID(t, pool, "completed-rejoin")
	assignFixture(t, r, userID, caseID)

	// Note: if the shared dev database's queue is genuinely non-empty right
	// now, this WILL legitimately come back with Rejoined + a real
	// AssignedCase (that's correct behavior, not a bug) -- completedSafely
	// puts that real case straight back afterward either way. This
	// assertion is therefore about Removed/Rejoined only, not about the
	// queue being empty, which this test has no way to guarantee.
	result := completedSafely(t, r, pool, userID)
	if !result.Rejoined || result.Removed {
		t.Errorf("expected Rejoined (not Removed) with pending_offline never set, got %+v", result)
	}

	row := getEngineerRow(t, pool, userID)
	if row.Status != StatusAvailable || row.CurrentCaseID != nil {
		t.Errorf("expected AVAILABLE with no current case after Completed (and any rescued case put back), got status=%s currentCaseID=%v", row.Status, row.CurrentCaseID)
	}
}

func TestCompleted_GoesOfflineWhenPendingOffline(t *testing.T) {
	r, pool := newTestRouter(t)
	userID := testUserID(t, r, pool, "completed-offline")
	caseID := testCaseID(t, pool, "completed-offline")
	assignFixture(t, r, userID, caseID)

	presenceResult, err := r.SetPresence(context.Background(), userID, StatusOffline)
	if err != nil {
		t.Fatalf("SetPresence(OFFLINE) mid-session: %v", err)
	}
	if !presenceResult.Applied || !presenceResult.PendingOffline {
		t.Fatalf("expected Applied+PendingOffline requesting OFFLINE mid-session, got %+v", presenceResult)
	}
	// Mid-session OFFLINE must not touch the session itself yet.
	mid := getEngineerRow(t, pool, userID)
	if mid.CurrentCaseID == nil || *mid.CurrentCaseID != caseID {
		t.Fatalf("requesting OFFLINE mid-session must not clear the current case yet, got %+v", mid)
	}

	result, err := r.Completed(context.Background(), userID)
	if err != nil {
		t.Fatalf("Completed: %v", err)
	}
	if !result.Removed || result.Rejoined || result.AssignedCase != nil {
		t.Errorf("expected Removed (offline, no rejoin) once pending_offline was set, got %+v", result)
	}

	row := getEngineerRow(t, pool, userID)
	if row.Status != StatusOffline || row.CurrentCaseID != nil || row.PendingOffline {
		t.Errorf("expected OFFLINE, no current case, pending_offline cleared, got %+v", row)
	}
}

func TestCompleted_DuplicateCallIsIdempotent(t *testing.T) {
	// This is the exact 2026-09-04 bug: a second Completed call for a
	// session that already ended used to re-derive AVAILABLE-vs-OFFLINE
	// from pending_offline's value AFTER the first call had already reset
	// it, silently overwriting a correct OFFLINE result back to AVAILABLE.
	// See Router.Completed's own doc comment for the current guard.
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

func TestSetPresence_ChangingMindClearsPendingOffline(t *testing.T) {
	r, pool := newTestRouter(t)
	userID := testUserID(t, r, pool, "presence-change-mind")
	caseID := testCaseID(t, pool, "presence-change-mind")
	assignFixture(t, r, userID, caseID)

	if _, err := r.SetPresence(context.Background(), userID, StatusOffline); err != nil {
		t.Fatalf("SetPresence(OFFLINE): %v", err)
	}
	result, err := r.SetPresence(context.Background(), userID, StatusAvailable)
	if err != nil {
		t.Fatalf("SetPresence(AVAILABLE) to change their mind: %v", err)
	}
	if !result.Applied || result.PendingOffline {
		t.Errorf("expected pending_offline cleared by requesting AVAILABLE mid-session, got %+v", result)
	}

	// The session itself must still be untouched by either presence call.
	mid := getEngineerRow(t, pool, userID)
	if mid.CurrentCaseID == nil || *mid.CurrentCaseID != caseID || mid.PendingOffline {
		t.Fatalf("expected session untouched and pending_offline cleared, got %+v", mid)
	}

	completedResult := completedSafely(t, r, pool, userID)
	if !completedResult.Rejoined || completedResult.Removed {
		t.Errorf("expected AVAILABLE rejoin after clearing pending_offline, got %+v", completedResult)
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
	// WITHOUT touching created_at, so it keeps its original place ahead of
	// anything that arrives later -- exercised directly against backCase's
	// own row (bypassing claimOldestWaiting here, since the shared dev
	// database's real queue contents could claim a different row first --
	// see this file's package doc comment) rather than asserting on
	// whichever case happens to be claimed.
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
