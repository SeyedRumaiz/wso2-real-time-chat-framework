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
-- db-schema-review-2026-09-07-outcomes.md doc): chat_conversation had no
-- field saying whether a chat was still ongoing or finished -- "there's no
-- way to confirm whether the chat is ended or still ongoing... the chat
-- conversation table should hold the state of it." Adds that lifecycle
-- field; the actual transition rules stay in application code (Sajith and
-- Mifraz's explicit conclusion), not the database -- this migration only
-- adds the column and enum, it does not encode which state can move to
-- which.
--
-- Only OPEN -> ACTIVE is wired to any code today (Router.Accept, via
-- setConversationEngineer in internal/router/workitem.go) -- RESOLVED,
-- CONVERTED_CHAT, CONVERTED_CASE, ABANDONED, and CLOSED exist in the enum
-- per the meeting's discussion, but this codebase has no "close this chat"
-- or "convert to a case" feature yet for anything to set them from. They
-- are here so that future work has the column/enum ready rather than
-- needing another migration.
CREATE TYPE chat_routing.chat_conversation_state AS ENUM (
  'OPEN', 'ACTIVE', 'RESOLVED', 'CONVERTED_CHAT', 'CONVERTED_CASE', 'ABANDONED', 'CLOSED'
);

ALTER TABLE chat_routing.chat_conversation
  ADD COLUMN state chat_routing.chat_conversation_state NOT NULL DEFAULT 'OPEN';

-- Existing rows: ACTIVE if already accepted (engineer_id set), otherwise
-- still OPEN -- the two states this codebase can actually tell apart today.
UPDATE chat_routing.chat_conversation SET state = 'ACTIVE' WHERE engineer_id IS NOT NULL;

-- conversation_id duplicated work_item_id/case_id (both already identify
-- the row) and nothing ever queried by it -- see this migration's sibling
-- Go changes (internal/router/workitem.go, the SDK, and csm-portal/
-- backend's HandleEscalate call site), which stop sending/persisting it
-- entirely rather than leaving an unused required field in the API.
ALTER TABLE chat_routing.chat_conversation DROP COLUMN conversation_id;
