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

-- Persistent audit trail of assignment outcomes, per the 2026-09-07 DB
-- schema review (see the project's db-schema-review-2026-09-07-outcomes.md
-- doc): "a persistent audit table ... conversation identifiers, engineer
-- identifiers, and statuses (such as connected, rejected, or timed out)
-- should be established for future reference."
--
-- This service never pings more than one candidate engineer at a time for
-- a given case (see Router.Escalate / popAvailableEngineer -- capacity is
-- reserved on the one engineer selected, not broadcast to several and
-- raced), so there is no live "pinged, awaiting response from N
-- candidates" state to persist here. One row is appended per assignment
-- once its outcome is known -- CONNECTED when Router.Accept succeeds,
-- REJECTED when Router.Decline succeeds (for the declining engineer),
-- TIMED_OUT when Router.SweepExpiredPending times an engineer out -- which
-- is exactly the "who was assigned what case and what happened" audit
-- trail the review asked for, without inventing multi-candidate ping state
-- this design doesn't otherwise have. Append-only, same style as
-- assignment_log.
CREATE TYPE chat_routing.assignment_outcome AS ENUM ('CONNECTED', 'REJECTED', 'TIMED_OUT');

CREATE TABLE chat_routing.chat_queue_engineer_assignment (
  id             BIGSERIAL PRIMARY KEY,
  case_id        TEXT NOT NULL,
  engineer_email TEXT NOT NULL REFERENCES chat_routing.engineers (email),
  status         chat_routing.assignment_outcome NOT NULL,
  occurred_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Backs "full outcome history for this case" and "history for this
-- engineer" lookups -- no live query in this service depends on this index
-- yet (see this file's own comment above: this table is audit-only), but
-- either access pattern is an obvious future need for this data.
CREATE INDEX idx_chat_queue_engineer_assignment_case
  ON chat_routing.chat_queue_engineer_assignment (case_id, occurred_at);
CREATE INDEX idx_chat_queue_engineer_assignment_engineer
  ON chat_routing.chat_queue_engineer_assignment (engineer_email, occurred_at);
