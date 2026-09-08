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

-- Renames escalation_queue to chat_queue and drops order_key, per the
-- 2026-09-07 DB schema review (see the project's
-- db-schema-review-2026-09-07-outcomes.md doc): "escalation" collides with
-- terminology already used elsewhere, and queue order should be derived
-- from when a row was created rather than a maintained integer sequence.
--
-- A pure created_at sort can't express "requeue at the FRONT" on its own,
-- though (Router.Decline / SweepExpiredPending push a declined/timed-out
-- case back ahead of everyone who has been waiting since before it was
-- ever assigned -- see those methods' own doc comments for why: that
-- customer already waited once). The meeting's own transcript raises
-- exactly this scenario without fully reconciling it against a pure
-- timestamp sort. Rather than reintroduce an order_key-shaped integer
-- sequence (the thing the derived-from-created_at conclusion was trying to
-- avoid), this adds one boolean, `requeued`: rows with requeued = true
-- always sort ahead of requeued = false rows, and *within* each of those
-- two groups, ordering is exactly the derived-from-created_at sort the
-- review concluded on. No table lock or MAX/MIN computation needed to
-- enqueue any more (see enqueueCase in internal/router/state.go) -- a nice
-- side effect of dropping order_key, not just parity with it.
ALTER TABLE chat_routing.escalation_queue RENAME TO chat_queue;

DROP INDEX IF EXISTS chat_routing.idx_escalation_queue_order;

ALTER TABLE chat_routing.chat_queue
  DROP COLUMN order_key,
  ADD COLUMN requeued BOOLEAN NOT NULL DEFAULT FALSE;

-- Pop-the-head query: ORDER BY requeued DESC, created_at ASC, id ASC --
-- requeued rows first (oldest-requeued first among themselves), then
-- everyone else oldest-first; id still breaks ties between two rows
-- inserted in the same instant.
CREATE INDEX idx_chat_queue_order ON chat_routing.chat_queue (requeued DESC, created_at ASC, id ASC);
