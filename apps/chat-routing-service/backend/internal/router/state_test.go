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
// emails and case IDs it generates itself (see testEmail/testCaseID) and
// cleans up after itself via t.Cleanup, so it can never touch or be
// confused with a real engineer row (like a live @wso2.com account) or a
// real queued case. What it deliberately does NOT test is Router.Escalate's
// own engineer-selection algorithm (sticky preference, least-busy-today
// ranking) or the exact position a case lands at in the shared queue --
// both depend on the full set of every engineer/queue row currently in the
// shared dev database, which a test generating its own isolated rows has
// no way to control or predict. Fixture setup below calls
// assignCaseToEngineer/enqueueCase directly (this file is in package
// router, so it can) specifically to sidestep that ambiguity: a test that
// needs "engineer X is PENDING on case Y" creates exactly that, without
// going through Escalate's pick-any-available-engineer path at all.
// Escalate's own selection logic is exercised by the manual multi-engineer
// curl walkthrough instead (see the project's
// live-engineer-chat-escalation-summary.md).
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

// testEmail returns a collision-free engineer email for the calling test,
// registers it (via SetPresence, so it starts OFFLINE and idle like any
// engineer's first-ever contact -- see Router.SetPresence's own doc
// comment), and cleans up every row this test's use of it could have
// touched (engineers, assignment_log, customer_engineer_assignments) once
// the test finishes, pass or fail.
func testEmail(t *testing.T, r *Router, pool *pgxpool.Pool, tag string) string {
	t.Helper()
	suffix := time.Now().UnixNano()
	email := fmt.Sprintf("router-test-%s-%d@router-test.internal", tag, suffix)
	ctx := context.Background()

	// engineer_id must be unique per call, not a shared literal: it's the
	// engineers table's actual primary key (see migrations/000004), so
	// reusing the same string across different tests' different emails
	// would hit a real "duplicate key value violates unique constraint
	// engineers_pkey" on the second test to run, not a logic bug -- just a
	// fixture mistake. Every *later* SetPresence call in this file passes
	// the literal "test-engineer-id" for an email that already has a row,
	// which is safe: ensureAndLockEngineer's INSERT ... ON CONFLICT (email)
	// DO NOTHING resolves against the existing email before Postgres ever
	// gets to checking engineer_id's uniqueness, so only this first-ever
	// insert for a brand-new email needs a genuinely unique value.
	engineerID := fmt.Sprintf("test-engineer-id-%s-%d", tag, suffix)
	if _, err := r.SetPresence(ctx, email, engineerID, StatusOffline); err != nil {
		t.Fatalf("seed test engineer %s: %v", email, err)
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := pool.Exec(cleanupCtx, `DELETE FROM engineers WHERE email = $1`, email); err != nil {
			t.Logf("cleanup: delete engineer %s: %v", email, err)
		}
		if _, err := pool.Exec(cleanupCtx, `DELETE FROM assignment_log WHERE email = $1`, email); err != nil {
			t.Logf("cleanup: delete assignment_log for %s: %v", email, err)
		}
		if _, err := pool.Exec(cleanupCtx, `DELETE FROM customer_engineer_assignments WHERE engineer_email = $1`, email); err != nil {
			t.Logf("cleanup: delete customer_engineer_assignments for %s: %v", email, err)
		}
	})
	return email
}

// testCaseID returns a collision-free case ID for the calling test and
// registers cleanup of any escalation_queue row left under it (a test that
// expects its case to be dequeued during the test doesn't need this to do
// anything at cleanup time; one that leaves a case queued -- e.g. because
// an assertion failed first -- won't leak a row into the shared queue).
func testCaseID(t *testing.T, pool *pgxpool.Pool, tag string) string {
	t.Helper()
	caseID := fmt.Sprintf("router-test-%s-%d", tag, time.Now().UnixNano())
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := pool.Exec(cleanupCtx, `DELETE FROM escalation_queue WHERE case_id = $1`, caseID); err != nil {
			t.Logf("cleanup: delete escalation_queue row %s: %v", caseID, err)
		}
	})
	return caseID
}

// assignFixture puts email directly into PENDING on a freshly-built
// CaseInfo for caseID, bypassing Escalate's engineer-selection entirely
// (calling the same unexported assignCaseToEngineer Escalate itself uses,
// directly -- see this file's package doc comment for why). email must
// already exist (see testEmail) and not currently hold a case.
func assignFixture(t *testing.T, r *Router, email, caseID string) CaseInfo {
	t.Helper()
	ci := CaseInfo{CaseID: caseID, ConversationID: "conv-" + caseID}
	caseInfoJSON, err := json.Marshal(ci)
	if err != nil {
		t.Fatalf("marshal fixture case info: %v", err)
	}

	err = r.withTx(context.Background(), func(tx pgx.Tx) error {
		return assignCaseToEngineer(context.Background(), tx, email, ci, caseInfoJSON)
	})
	if err != nil {
		t.Fatalf("assignFixture: %v", err)
	}
	return ci
}

// engineerRowForTest is the subset of an engineers row assertions below
// check directly, bypassing Router's own public API so a test can confirm
// what's actually in the database rather than only what a method's return
// value claims.
type engineerRowForTest struct {
	Status         Status
	CurrentCaseID  *string
	PendingOffline bool
}

func getEngineerRow(t *testing.T, pool *pgxpool.Pool, email string) engineerRowForTest {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var row engineerRowForTest
	err := pool.QueryRow(ctx, `
		SELECT status, current_case_id, pending_offline FROM engineers WHERE email = $1
	`, email).Scan(&row.Status, &row.CurrentCaseID, &row.PendingOffline)
	if err != nil {
		t.Fatalf("read engineer row %s: %v", email, err)
	}
	return row
}

// completedSafely calls Router.Completed for email, and, if that happened
// to drain a case off the shared escalation_queue (result.AssignedCase !=
// nil), immediately puts it straight back at the front of the queue and
// clears it off email directly via SQL -- bypassing Decline/SetPresence
// entirely to avoid recursively risking the exact same problem. None of
// this file's fixtures ever enqueue anything before calling this, so a
// non-nil AssignedCase here can only be a real, pre-existing case that was
// genuinely waiting in the shared dev database's queue (see this file's
// package doc comment) -- not test data. Without this, a test's fixture
// engineer rejoining AVAILABLE could silently steal a real customer's
// queued escalation, then lose it entirely once that fixture engineer's
// row is deleted at test cleanup.
func completedSafely(t *testing.T, r *Router, pool *pgxpool.Pool, email string) CompletedResult {
	t.Helper()
	ctx := context.Background()
	result, err := r.Completed(ctx, email)
	if err != nil {
		t.Fatalf("Completed: %v", err)
	}
	if result.AssignedCase == nil {
		return result
	}

	t.Logf("Completed drained a real queued case (caseId=%s) onto the test fixture engineer -- restoring it to the queue", result.AssignedCase.CaseID)
	caseInfoJSON, err := json.Marshal(*result.AssignedCase)
	if err != nil {
		t.Fatalf("re-marshal rescued case: %v", err)
	}
	rescueCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tx, err := pool.Begin(rescueCtx)
	if err != nil {
		t.Fatalf("rescue: begin transaction: %v", err)
	}
	defer func() { _ = tx.Rollback(rescueCtx) }()
	if _, err := enqueueCase(rescueCtx, tx, *result.AssignedCase, caseInfoJSON, true); err != nil {
		t.Fatalf("rescue: re-enqueue drained case: %v", err)
	}
	if _, err := tx.Exec(rescueCtx, `
		UPDATE engineers SET status = 'AVAILABLE', current_case_id = NULL, current_case = NULL,
		    available_since = now(), updated_at = now()
		WHERE email = $1
	`, email); err != nil {
		t.Fatalf("rescue: clear fixture engineer's stolen case: %v", err)
	}
	if err := tx.Commit(rescueCtx); err != nil {
		t.Fatalf("rescue: commit: %v", err)
	}
	return result
}

func TestCompleted_NoOpWhenIdle(t *testing.T) {
	r, pool := newTestRouter(t)
	email := testEmail(t, r, pool, "completed-idle")

	result, err := r.Completed(context.Background(), email)
	if err != nil {
		t.Fatalf("Completed: %v", err)
	}
	if result.Removed || result.Rejoined || result.AssignedCase != nil {
		t.Errorf("Completed on an idle engineer should be a pure no-op, got %+v", result)
	}
}

func TestCompleted_NoOpForUnknownEmail(t *testing.T) {
	r, _ := newTestRouter(t)
	result, err := r.Completed(context.Background(), fmt.Sprintf("nobody-%d@router-test.internal", time.Now().UnixNano()))
	if err != nil {
		t.Fatalf("Completed: %v", err)
	}
	if result.Removed || result.Rejoined || result.AssignedCase != nil {
		t.Errorf("Completed for an email with no row at all should be a pure no-op, got %+v", result)
	}
}

func TestCompleted_RejoinsAvailableWhenNotPendingOffline(t *testing.T) {
	r, pool := newTestRouter(t)
	email := testEmail(t, r, pool, "completed-rejoin")
	caseID := testCaseID(t, pool, "completed-rejoin")
	assignFixture(t, r, email, caseID)

	// Note: if the shared dev database's queue is genuinely non-empty right
	// now, this WILL legitimately come back with Rejoined + a real
	// AssignedCase (that's correct behavior, not a bug) -- completedSafely
	// puts that real case straight back afterward either way. This
	// assertion is therefore about Removed/Rejoined only, not about the
	// queue being empty, which this test has no way to guarantee.
	result := completedSafely(t, r, pool, email)
	if !result.Rejoined || result.Removed {
		t.Errorf("expected Rejoined (not Removed) with pending_offline never set, got %+v", result)
	}

	row := getEngineerRow(t, pool, email)
	if row.Status != StatusAvailable || row.CurrentCaseID != nil {
		t.Errorf("expected AVAILABLE with no current case after Completed (and any rescued case put back), got status=%s currentCaseID=%v", row.Status, row.CurrentCaseID)
	}
}

func TestCompleted_GoesOfflineWhenPendingOffline(t *testing.T) {
	r, pool := newTestRouter(t)
	email := testEmail(t, r, pool, "completed-offline")
	caseID := testCaseID(t, pool, "completed-offline")
	assignFixture(t, r, email, caseID)

	presenceResult, err := r.SetPresence(context.Background(), email, "test-engineer-id", StatusOffline)
	if err != nil {
		t.Fatalf("SetPresence(OFFLINE) mid-session: %v", err)
	}
	if !presenceResult.Applied || !presenceResult.PendingOffline {
		t.Fatalf("expected Applied+PendingOffline requesting OFFLINE mid-session, got %+v", presenceResult)
	}
	// Mid-session OFFLINE must not touch the session itself yet.
	mid := getEngineerRow(t, pool, email)
	if mid.CurrentCaseID == nil || *mid.CurrentCaseID != caseID {
		t.Fatalf("requesting OFFLINE mid-session must not clear the current case yet, got %+v", mid)
	}

	result, err := r.Completed(context.Background(), email)
	if err != nil {
		t.Fatalf("Completed: %v", err)
	}
	if !result.Removed || result.Rejoined || result.AssignedCase != nil {
		t.Errorf("expected Removed (offline, no rejoin) once pending_offline was set, got %+v", result)
	}

	row := getEngineerRow(t, pool, email)
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
	email := testEmail(t, r, pool, "completed-duplicate")
	caseID := testCaseID(t, pool, "completed-duplicate")
	assignFixture(t, r, email, caseID)

	if _, err := r.SetPresence(context.Background(), email, "test-engineer-id", StatusOffline); err != nil {
		t.Fatalf("SetPresence(OFFLINE) mid-session: %v", err)
	}

	first, err := r.Completed(context.Background(), email)
	if err != nil {
		t.Fatalf("first Completed: %v", err)
	}
	if !first.Removed {
		t.Fatalf("expected the first Completed call to go OFFLINE (Removed), got %+v", first)
	}

	second, err := r.Completed(context.Background(), email)
	if err != nil {
		t.Fatalf("second Completed: %v", err)
	}
	if second.Removed || second.Rejoined || second.AssignedCase != nil {
		t.Errorf("a duplicate Completed call for an already-ended session must be a no-op, got %+v", second)
	}

	row := getEngineerRow(t, pool, email)
	if row.Status != StatusOffline {
		t.Errorf("a duplicate Completed call must not have changed status away from OFFLINE, got %s", row.Status)
	}
}

func TestSetPresence_ChangingMindClearsPendingOffline(t *testing.T) {
	r, pool := newTestRouter(t)
	email := testEmail(t, r, pool, "presence-change-mind")
	caseID := testCaseID(t, pool, "presence-change-mind")
	assignFixture(t, r, email, caseID)

	if _, err := r.SetPresence(context.Background(), email, "test-engineer-id", StatusOffline); err != nil {
		t.Fatalf("SetPresence(OFFLINE): %v", err)
	}
	result, err := r.SetPresence(context.Background(), email, "test-engineer-id", StatusAvailable)
	if err != nil {
		t.Fatalf("SetPresence(AVAILABLE) to change their mind: %v", err)
	}
	if !result.Applied || result.PendingOffline {
		t.Errorf("expected pending_offline cleared by requesting AVAILABLE mid-session, got %+v", result)
	}

	// The session itself must still be untouched by either presence call.
	mid := getEngineerRow(t, pool, email)
	if mid.CurrentCaseID == nil || *mid.CurrentCaseID != caseID || mid.PendingOffline {
		t.Fatalf("expected session untouched and pending_offline cleared, got %+v", mid)
	}

	completedResult := completedSafely(t, r, pool, email)
	if !completedResult.Rejoined || completedResult.Removed {
		t.Errorf("expected AVAILABLE rejoin after clearing pending_offline, got %+v", completedResult)
	}
}

func TestSetPresence_RejectsDirectPendingOrBusyRequest(t *testing.T) {
	r, pool := newTestRouter(t)
	email := testEmail(t, r, pool, "presence-reject-derived")

	for _, want := range []Status{StatusPending, StatusBusy} {
		result, err := r.SetPresence(context.Background(), email, "test-engineer-id", want)
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
	email := testEmail(t, r, pool, "accept-valid")
	caseID := testCaseID(t, pool, "accept-valid")
	assignFixture(t, r, email, caseID)

	result, err := r.Accept(context.Background(), email, caseID)
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	if !result.Applied {
		t.Fatalf("expected Accept to apply for the case the engineer is actually PENDING on, got %+v", result)
	}

	row := getEngineerRow(t, pool, email)
	if row.Status != StatusBusy {
		t.Errorf("expected BUSY after a valid Accept, got %s", row.Status)
	}
}

func TestAccept_RejectsWhenIdle(t *testing.T) {
	r, pool := newTestRouter(t)
	email := testEmail(t, r, pool, "accept-idle")

	result, err := r.Accept(context.Background(), email, "some-case-id")
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	if result.Applied {
		t.Errorf("expected Accept to reject an engineer with no PENDING case at all, got %+v", result)
	}
}

func TestAccept_RejectsStaleCaseID(t *testing.T) {
	r, pool := newTestRouter(t)
	email := testEmail(t, r, pool, "accept-stale")
	caseID := testCaseID(t, pool, "accept-stale")
	assignFixture(t, r, email, caseID)

	result, err := r.Accept(context.Background(), email, "a-different-case-id-entirely")
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	if result.Applied {
		t.Errorf("expected Accept to reject a caseID that doesn't match what the engineer is actually PENDING on, got %+v", result)
	}

	// The real, still-PENDING case must be untouched by the failed accept.
	row := getEngineerRow(t, pool, email)
	if row.Status != StatusPending || row.CurrentCaseID == nil || *row.CurrentCaseID != caseID {
		t.Errorf("a stale Accept must not disturb the engineer's real PENDING case, got %+v", row)
	}
}

func TestAccept_RejectsDoubleAccept(t *testing.T) {
	r, pool := newTestRouter(t)
	email := testEmail(t, r, pool, "accept-double")
	caseID := testCaseID(t, pool, "accept-double")
	assignFixture(t, r, email, caseID)

	first, err := r.Accept(context.Background(), email, caseID)
	if err != nil || !first.Applied {
		t.Fatalf("first Accept should apply, got %+v, err=%v", first, err)
	}

	second, err := r.Accept(context.Background(), email, caseID)
	if err != nil {
		t.Fatalf("second Accept: %v", err)
	}
	if second.Applied {
		t.Errorf("a second Accept for a case already BUSY (not PENDING) must not apply again, got %+v", second)
	}
}

func TestDecline_NoOpForStaleCaseID(t *testing.T) {
	r, pool := newTestRouter(t)
	email := testEmail(t, r, pool, "decline-stale")
	caseID := testCaseID(t, pool, "decline-stale")
	assignFixture(t, r, email, caseID)

	result, err := r.Decline(context.Background(), email, "a-different-case-id-entirely")
	if err != nil {
		t.Fatalf("Decline: %v", err)
	}
	if result.ReassignedTo != "" || result.Requeued || result.AssignedCase != nil {
		t.Errorf("Decline for a caseID the engineer isn't actually holding must be a no-op, got %+v", result)
	}

	// The real case must be untouched.
	row := getEngineerRow(t, pool, email)
	if row.Status != StatusPending || row.CurrentCaseID == nil || *row.CurrentCaseID != caseID {
		t.Errorf("a stale Decline must not disturb the engineer's real PENDING case, got %+v", row)
	}
}

// Decline's reassignment path (as opposed to the no-op tested above) is
// deliberately not exercised here: it can hand the declined case to
// whichever OTHER engineer is currently AVAILABLE in the shared dev
// database, which could be a real account (see this file's package doc
// comment on Escalate's own selection logic for the same reason) --
// polluting that real engineer's live current_case and assignment_log with
// a fake test case is a worse outcome than the coverage gap. That path was
// exercised manually via curl during this feature's own testing instead
// (see the project's live-engineer-chat-escalation-summary.md).

func TestEnqueueCase_FrontGoesAheadOfBack(t *testing.T) {
	r, pool := newTestRouter(t)
	ctx := context.Background()

	backID := testCaseID(t, pool, "enqueue-back")
	frontID := testCaseID(t, pool, "enqueue-front")
	backCase := CaseInfo{CaseID: backID, ConversationID: "conv-" + backID}
	frontCase := CaseInfo{CaseID: frontID, ConversationID: "conv-" + frontID}
	backJSON, _ := json.Marshal(backCase)
	frontJSON, _ := json.Marshal(frontCase)

	err := r.withTx(ctx, func(tx pgx.Tx) error {
		if _, err := enqueueCase(ctx, tx, backCase, backJSON, false); err != nil {
			return err
		}
		if _, err := enqueueCase(ctx, tx, frontCase, frontJSON, true); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		t.Fatalf("enqueue fixture: %v", err)
	}

	var backKey, frontKey int64
	if err := pool.QueryRow(ctx, `SELECT order_key FROM escalation_queue WHERE case_id = $1`, backID).Scan(&backKey); err != nil {
		t.Fatalf("read back order_key: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT order_key FROM escalation_queue WHERE case_id = $1`, frontID).Scan(&frontKey); err != nil {
		t.Fatalf("read front order_key: %v", err)
	}
	if frontKey >= backKey {
		t.Errorf("expected the front=true enqueue's order_key (%d) to sort before the back one (%d)", frontKey, backKey)
	}
}
