#!/usr/bin/env bash
# Manual multi-engineer routing test — covers the 3 things the automated
# suite and your earlier single-engineer curl test deliberately DON'T cover:
#   1. Sticky routing (a returning customer prefers their last engineer)
#   2. Least-busy fallback (no sticky history -> fewest chats today wins)
#   3. Decline -> reassignment to another engineer
#
# Uses fully fake engineer emails/ids and fake case ids -- doesn't touch any
# real engineer or real case. Safe to run against your normal dev database.
# Run from anywhere once chat-routing-service is up on :9096.

set -euo pipefail

BASE="http://localhost:9096"
TOK="${ROUTING_SERVICE_TOKEN:?Set ROUTING_SERVICE_TOKEN first (e.g. cd apps/chat-routing-service/backend && set -a && source .env && set +a)}"
TS=$(date +%s)

ENG_A="test-engineer-a-${TS}@test.local"
ENG_A_ID="test-eng-a-${TS}"
ENG_B="test-engineer-b-${TS}@test.local"
ENG_B_ID="test-eng-b-${TS}"
CUST_X="test-customer-x-${TS}@test.local"
CUST_Y="test-customer-y-${TS}@test.local"

hdr=(-H "X-Routing-Service-Token: $TOK" -H "Content-Type: application/json")

req() { curl -sS "${hdr[@]}" "$@"; echo; }

section() { echo; echo "=================================================="; echo "$1"; echo "=================================================="; }

section "-1. Safety check: empty queue, and no OTHER engineer currently AVAILABLE"
# Plain grep/sed only -- deliberately avoids piping a script into an
# interpreter (python3/perl/etc.), since that pattern gets killed outright
# by endpoint-security software on some managed machines (SIGKILL, no
# useful error) regardless of what the script actually does.
STATE_CHECK=$(curl -sS "${hdr[@]}" "$BASE/route/debug/state")
echo "$STATE_CHECK"

QUEUE_FIELD=$(echo "$STATE_CHECK" | grep -o '"queue":\[[^]]*\]')
AVAILABLE_FIELD=$(echo "$STATE_CHECK" | grep -o '"available":\[[^]]*\]')

PROBLEM=0
if [ "$QUEUE_FIELD" != '"queue":[]' ]; then
  echo "!!! - the queue is NOT empty ($QUEUE_FIELD) -- a fake engineer going AVAILABLE"
  echo "!!!   could auto-steal a real waiting customer's case."
  PROBLEM=1
fi
if [ "$AVAILABLE_FIELD" != '"available":[]' ]; then
  echo "!!! - at least one engineer is ALREADY available right now: $AVAILABLE_FIELD"
  echo "!!!   they could get handed one of this script's fake test cases and see a"
  echo "!!!   fake alert pop up in their UI."
  PROBLEM=1
fi

if [ "$PROBLEM" -eq 1 ]; then
  echo
  echo "!!! STOP -- not safe to run this script right now. Take the engineer(s)"
  echo "!!! above OFFLINE (or clear the queue) first, then re-run."
  exit 1
fi
echo "OK: queue is empty and no engineer is currently AVAILABLE. Safe to proceed."

section "0. Both engineers OFFLINE -> AVAILABLE (A first, then B)"
req -XPOST "$BASE/route/presence" -d "{\"email\":\"$ENG_A\",\"engineerId\":\"$ENG_A_ID\",\"status\":\"AVAILABLE\"}"
sleep 1   # ensure A's available_since is strictly earlier than B's, for a clean tie-break
req -XPOST "$BASE/route/presence" -d "{\"email\":\"$ENG_B\",\"engineerId\":\"$ENG_B_ID\",\"status\":\"AVAILABLE\"}"

section "1. Customer X's FIRST escalation -- expect it to land on A (earliest available, tied on 0 chats today)"
req -XPOST "$BASE/route/escalate" -d "{\"caseId\":\"case1-${TS}\",\"conversationId\":\"conv1-${TS}\",\"customerEmail\":\"$CUST_X\",\"customerName\":\"Test X\",\"subject\":\"test\",\"message\":\"hi\"}"

echo "--- accept + complete case1 as A (so A now has 1 chat today, and is free again) ---"
req -XPOST "$BASE/route/accept" -d "{\"email\":\"$ENG_A\",\"caseId\":\"case1-${TS}\"}"
req -XPOST "$BASE/route/completed" -d "{\"email\":\"$ENG_A\"}"

section "2. SANITY CHECK: raw least-busy alone would now favor B (0 chats vs A's 1)"
echo "If sticky routing works, the next test should go to A anyway, DESPITE this."

section "3. Customer X escalates AGAIN -- expect STICKY: goes back to A even though B is less busy"
req -XPOST "$BASE/route/escalate" -d "{\"caseId\":\"case2-${TS}\",\"conversationId\":\"conv2-${TS}\",\"customerEmail\":\"$CUST_X\",\"customerName\":\"Test X\",\"subject\":\"test\",\"message\":\"hi again\"}"
echo "^ CHECK: engineerEmail above should be $ENG_A. If it's $ENG_B instead, sticky routing did NOT win over least-busy."

echo "--- accept + complete case2 as A (A now has 2 chats today, B still 0) ---"
req -XPOST "$BASE/route/accept" -d "{\"email\":\"$ENG_A\",\"caseId\":\"case2-${TS}\"}"
req -XPOST "$BASE/route/completed" -d "{\"email\":\"$ENG_A\"}"

section "4. A NEW customer Y (no sticky history) escalates -- expect LEAST-BUSY: goes to B (0 chats vs A's 2)"
req -XPOST "$BASE/route/escalate" -d "{\"caseId\":\"case3-${TS}\",\"conversationId\":\"conv3-${TS}\",\"customerEmail\":\"$CUST_Y\",\"customerName\":\"Test Y\",\"subject\":\"test\",\"message\":\"hello\"}"
echo "^ CHECK: engineerEmail above should be $ENG_B."

section "5. DECLINE test -- B declines case3, only A is available -> expect reassignment to A"
req -XPOST "$BASE/route/decline" -d "{\"email\":\"$ENG_B\",\"caseId\":\"case3-${TS}\"}"
echo "^ CHECK: reassignedTo above should be $ENG_A."

echo "--- clean up: accept+complete the reassigned case, then take both engineers OFFLINE ---"
req -XPOST "$BASE/route/accept" -d "{\"email\":\"$ENG_A\",\"caseId\":\"case3-${TS}\"}"
req -XPOST "$BASE/route/completed" -d "{\"email\":\"$ENG_A\"}"
req -XPOST "$BASE/route/presence" -d "{\"email\":\"$ENG_A\",\"engineerId\":\"$ENG_A_ID\",\"status\":\"OFFLINE\"}"
req -XPOST "$BASE/route/presence" -d "{\"email\":\"$ENG_B\",\"engineerId\":\"$ENG_B_ID\",\"status\":\"OFFLINE\"}"

section "6. Final state -- both fake engineers should show OFFLINE, queue empty"
req "$BASE/route/debug/state"

echo
echo "Done. Fake identities used (all end in -${TS}), safe to ignore/leave -- they're"
echo "OFFLINE now and will never be picked by real routing again."
