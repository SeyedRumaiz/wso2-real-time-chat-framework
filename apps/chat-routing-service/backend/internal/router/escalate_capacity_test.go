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
	"testing"
)

// TestEscalate_FourRealEscalationsLandOnCapacityFourEngineer exercises the
// full concurrent-capacity path end to end through the real Escalate()
// entry point (not the assignFixture shortcut used by
// TestConcurrentCapacity_SecondCaseAssignedWithoutQueueingWhenCapacityTwo),
// at a capacity higher than the 2 already covered elsewhere. An engineer
// configured for max_concurrent_chats = 4 must actually receive all 4
// escalations directly (no queueing), and a 5th, over-capacity escalation
// must not land on them.
func TestEscalate_FourRealEscalationsLandOnCapacityFourEngineer(t *testing.T) {
	r, pool := newTestRouter(t)
	ctx := context.Background()

	userID := testUserID(t, r, pool, "capacity-four-escalate")
	setMaxConcurrent(t, pool, userID, 4)
	if _, err := r.SetPresence(ctx, userID, StatusAvailable); err != nil {
		t.Fatalf("SetPresence(AVAILABLE): %v", err)
	}

	for i := 0; i < 4; i++ {
		id := testCaseID(t, pool, "capacity-four-case")
		ci := conversationFixture(t, r, pool, id)
		res, err := r.Escalate(ctx, ci)
		if err != nil {
			t.Fatalf("Escalate #%d: %v", i+1, err)
		}
		if res.Queued {
			t.Errorf("Escalate #%d: expected direct routing (capacity 4, %d active so far), got queued: %+v", i+1, i, res)
		}
	}

	// A 5th, over capacity, must not land on this already-full engineer.
	fifthID := testCaseID(t, pool, "capacity-four-fifth")
	fifthCI := conversationFixture(t, r, pool, fifthID)
	fifthRes, err := r.Escalate(ctx, fifthCI)
	if err != nil {
		t.Fatalf("Escalate #5: %v", err)
	}
	if fifthRes.EngineerUserID == userID {
		t.Errorf("expected the 5th escalation to NOT land on the already-at-capacity engineer, got: %+v", fifthRes)
	}

	var activeCount int
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM chat_conversation
		WHERE assignee_id = $1 AND state IN ('OPEN','ACTIVE') AND session_ended_at IS NULL
	`, userID).Scan(&activeCount); err != nil {
		t.Fatalf("count active: %v", err)
	}
	if activeCount != 4 {
		t.Fatalf("expected exactly 4 active chats actually assigned to the capacity-4 engineer, got %d", activeCount)
	}
}
