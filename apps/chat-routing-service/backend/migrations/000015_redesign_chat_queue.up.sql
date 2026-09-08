-- Copyright (c) 2026 WSO2 LLC. (https://www.wso2.com).
--
-- WSO2 LLC. licenses this file to you under the Apache License,
-- Version 2.0 (the "License"); you may not use this file except
-- in compliance with the License.
-- You may obtain a copy of the License at
--
-- http://www.apache.org/licenses/LICENSE-2.0
--
-- Unless required by applicable law or agreed to in writing,
-- software distributed under the License is distributed on an
-- "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
-- KIND, either express or implied.  See the License for the
-- specific language governing permissions and limitations
-- under the License.

-- 2026-09-07 DB schema review's chat_queue redesign (see the project's
-- db-schema-review-2026-09-07-outcomes.md doc and its own full transcript,
-- around "work item is the one having a UU ID... you can make it uh what is
-- this big serial", "So once the chat is accepted... are we deleting from
-- this?" / "Yes.", and "we can just get the order [position] by sorting
-- with the created time"):
--
--   - The bigserial `id` PK is dropped in favor of a natural key: the
--     chat's own conversation identifier, chat_conversation_id (this
--     service's existing CaseInfo.ConversationID -- already available at
--     Router.Escalate time, unlike the LOCAL STAND-IN chat_conversation
--     table's own work_item_id, which does not exist yet at that point --
--     see internal/router/workitem.go's package doc comment).
--   - `status` is now a 2-value enum, WAITING_FOR_ENGINEER / ASSIGNED --
--     "here it has only two values right waiting for engineer and engineer
--     [as]signed. That's it." ONE row now exists per escalation from the
--     moment it's created (Router.Escalate) until Router.Accept confirms
--     the engineer -- not only while it is genuinely waiting -- and is
--     deleted only then. That is what makes plain (created_at,
--     chat_conversation_id) ordering enough on its own: a row that gets
--     reassigned (Decline, a timeout sweep) or handed to a newly-available
--     engineer (SetPresence/Completed's queue-drain) is only ever UPDATEd
--     in place, never deleted and re-inserted, so it keeps its ORIGINAL
--     created_at -- exactly the FRONT-of-queue priority the old
--     order_key/requeued mechanism (000002, 000008) existed to provide,
--     with no extra bookkeeping column needed at all.
--   - `requeued` (000008) is dropped along with it -- superseded by the
--     above.
--
-- Deliberate deviation from the review: the meeting also concluded the old
-- `case_info` JSONB blob duplicates data that already lives in "the case
-- table" ("I think you're duplicating lot of info like case info is not
-- required right because that's in the case table") and should be dropped.
-- That reasoning assumes a real, queryable case/work-item table this
-- service can read from at dequeue time. This service has no such table
-- reachable today: entity-service's real case data lives in a separate
-- service, and even this feature's own LOCAL STAND-IN work_item/
-- chat_conversation tables aren't populated until AFTER Router.Escalate
-- returns (csm-portal/backend's HandleEscalate calls CreateWorkItem only
-- once Escalate has already assigned-or-queued the case). Dropping
-- case_info here with nothing to reconstruct it from would mean a customer
-- waiting in the queue -- or handed to a freshly-available engineer via the
-- queue-drain path -- loses their subject/message/customer name the moment
-- they're queued. case_info is kept for that reason (flagged here, and in
-- this service's README, so it doesn't read as an oversight) and should be
-- revisited once this service's LOCAL STAND-IN tables are retired in favor
-- of entity-service's real, atomically-created work item.
CREATE TYPE chat_routing.chat_queue_status AS ENUM ('WAITING_FOR_ENGINEER', 'ASSIGNED');

CREATE TABLE chat_routing.chat_queue_new (
  chat_conversation_id  TEXT PRIMARY KEY,
  case_info             JSONB NOT NULL,
  status                chat_routing.chat_queue_status NOT NULL DEFAULT 'WAITING_FOR_ENGINEER',
  created_at            TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- In-flight rows (there should be none in a normal deploy window, but a
-- migration must not silently drop live queue state): carried across using
-- case_id as the new natural key -- this service's case identifier and its
-- conversation identifier were, in practice, always the same value passed
-- straight through from CaseInfo.CaseID before this migration (see
-- internal/router/state.go's own enqueueCase). Every carried-over row is
-- conservatively marked WAITING_FOR_ENGINEER: the old table's own shape
-- can't tell us which rows had already been handed to an engineer (that was
-- never what it tracked -- a popped row was deleted, full stop), and
-- guessing ASSIGNED risks a customer being silently dropped if wrong.
INSERT INTO chat_routing.chat_queue_new (chat_conversation_id, case_info, status, created_at)
SELECT case_id, case_info, 'WAITING_FOR_ENGINEER', created_at FROM chat_routing.chat_queue
ON CONFLICT (chat_conversation_id) DO NOTHING;

DROP TABLE chat_routing.chat_queue;
ALTER TABLE chat_routing.chat_queue_new RENAME TO chat_queue;

CREATE INDEX idx_chat_queue_waiting_order
  ON chat_routing.chat_queue (created_at, chat_conversation_id)
  WHERE status = 'WAITING_FOR_ENGINEER';
