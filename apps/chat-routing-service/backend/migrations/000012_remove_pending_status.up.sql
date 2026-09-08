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

-- Per the 2026-09-07 DB schema review (see the project's
-- db-schema-review-2026-09-07-outcomes.md doc): "pending" (and "pending
-- offline") should not be values in the engineer status enum -- they
-- should be derivable instead. pending_offline already was a plain
-- boolean column, never an enum value, so that half of the conclusion was
-- already true going into this migration. PENDING, added by
-- 000006_add_pending_status.up.sql, is a real enum value and comes out
-- here.
--
-- What PENDING actually distinguished from BUSY was "has this assignment
-- been confirmed by Router.Accept yet" -- a fact this migration keeps as a
-- new accepted_at TIMESTAMPTZ column instead of a fourth enum value: an
-- engineer with current_case_id set and accepted_at still NULL is pending;
-- once Accept runs, accepted_at is set and they read as genuinely BUSY.
-- See internal/router/state.go's new isPendingAccept/externalStatus
-- helpers, used everywhere this package used to compare status directly
-- against StatusPending.
--
-- Deliberately NOT derived from chat_conversation.state (added by
-- 000010, also OPEN-until-Accept/ACTIVE-after) even though that would
-- look similar: chat_conversation is an explicitly temporary LOCAL
-- STAND-IN table (see internal/router/workitem.go's package doc comment)
-- slated for deletion once entity-service's real schema ships, and every
-- write to it is best-effort from this package's point of view (a failed
-- CreateWorkItem must never block real routing/timeout behavior -- see
-- Router.Accept's own doc comment). Making the core availability state
-- machine's PENDING/BUSY distinction depend on that table would mean an
-- engineer could get stuck showing BUSY forever (never timing out) simply
-- because their case's stand-in row failed to write. accepted_at lives on
-- engineers itself instead, so this real, permanent table's own state
-- machine has no dependency on the stand-in.
--
-- Postgres has no ALTER TYPE ... DROP VALUE, so the enum is rebuilt:
-- create the 3-value type, migrate the column across it (mapping any
-- existing PENDING row to BUSY, with accepted_at backfilled per the two
-- ADD COLUMN/UPDATE statements below so already-accepted sessions aren't
-- mistaken for newly-pending ones), then swap the type in under the old
-- name.
ALTER TABLE chat_routing.engineers ADD COLUMN accepted_at TIMESTAMPTZ;

-- Backfill: a row already BUSY (accepted) keeps looking accepted --
-- approximate accepted_at as updated_at, the closest thing to an actual
-- accept timestamp this table has. A row currently PENDING is left NULL
-- (accepted_at IS NULL is exactly what "pending" now means), and idle
-- engineers keep NULL too (current_case_id is NULL for them regardless).
UPDATE chat_routing.engineers SET accepted_at = updated_at
WHERE status = 'BUSY' AND current_case_id IS NOT NULL;

CREATE TYPE chat_routing.engineer_status_new AS ENUM ('AVAILABLE', 'BUSY', 'OFFLINE');

ALTER TABLE chat_routing.engineers ALTER COLUMN status DROP DEFAULT;

ALTER TABLE chat_routing.engineers
  ALTER COLUMN status TYPE chat_routing.engineer_status_new
  USING (CASE WHEN status::text = 'PENDING' THEN 'BUSY' ELSE status::text END)::chat_routing.engineer_status_new;

ALTER TABLE chat_routing.engineers ALTER COLUMN status SET DEFAULT 'OFFLINE';

DROP TYPE chat_routing.engineer_status;
ALTER TYPE chat_routing.engineer_status_new RENAME TO engineer_status;

ALTER TABLE chat_routing.engineers
  ADD CONSTRAINT chk_accepted_only_with_case
  CHECK (accepted_at IS NULL OR current_case_id IS NOT NULL);
