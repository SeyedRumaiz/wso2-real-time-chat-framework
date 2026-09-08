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

-- 2026-09-07 DB schema review (see the project's
-- db-schema-review-2026-09-07-outcomes.md doc and its own full transcript):
--
--   - engineers.email is dropped -- "we don't need to store email here
--     because it's there in the user table. It is just the user ID."
--     engineers.engineer_id (the IdP's stable per-account "userid" claim --
--     see migrations/000004) becomes the table's sole identifier, renamed
--     to user_id -- "the engineer ID, we can just rename it like user ID."
--   - The table itself is renamed: "this is too very specific to chat
--     conversation like available, pending and all" / "CS engineer chat
--     status" (Mifraz Murthaja's suggested name, used here in its shorter
--     form to match this feature's other already-short table names).
--   - Its status column is renamed to chat_status for the same reason --
--     "maybe just rename this status to chat status."
--
-- Every caller in this codebase that used to identify an engineer by email
-- now uses this same user_id instead: chat-routing-service's Router methods
-- and HTTP routes, its SDK, and csm-portal/backend's chat handlers + SSE
-- hub keys (engineerHubKey) -- see internal/router/state.go and this
-- service's README for the full list. csm-portal/backend already had this
-- value on hand for every authenticated request (middleware.UserInfo.
-- UserID, decoded from that same "userid" JWT claim) -- no new plumbing was
-- needed there, only switching which field its calls into this service
-- pass.
--
-- chat_queue_engineer_assignment's FK depends on the email-backed unique
-- constraint being dropped below (same reasoning as migrations/000004's own
-- up migration -- CASCADE here would silently drop a real FK constraint
-- instead of just restructuring the table it points at) -- dropped and
-- re-pointed at the new user_id PK explicitly, in the same migration as its
-- own case_id/engineer_email -> conversation_id/engineer_id rename (per the
-- outcomes doc's own wording for this audit table: "conversation
-- identifiers, engineer identifiers, and statuses").
ALTER TABLE chat_routing.chat_queue_engineer_assignment
  DROP CONSTRAINT chat_queue_engineer_assignment_engineer_email_fkey;

ALTER TABLE chat_routing.engineers RENAME TO cs_engineer_status;
ALTER TABLE chat_routing.cs_engineer_status RENAME COLUMN engineer_id TO user_id;
ALTER TABLE chat_routing.cs_engineer_status DROP COLUMN email;
ALTER TABLE chat_routing.cs_engineer_status RENAME COLUMN status TO chat_status;

ALTER TABLE chat_routing.chat_queue_engineer_assignment RENAME COLUMN case_id TO conversation_id;
ALTER TABLE chat_routing.chat_queue_engineer_assignment RENAME COLUMN engineer_email TO engineer_id;
ALTER TABLE chat_routing.chat_queue_engineer_assignment
  ADD CONSTRAINT chat_queue_engineer_assignment_engineer_id_fkey
  FOREIGN KEY (engineer_id) REFERENCES chat_routing.cs_engineer_status (user_id);
