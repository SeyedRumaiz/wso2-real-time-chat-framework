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

-- LOCAL STAND-IN, not entity-service's real schema. Sajith Ekanayaka
-- (2026-09-02) described entity-service's incoming generic WORK_ITEM
-- supertype as still evolving (a few days out) but gave an interim shape
-- to build against meanwhile: a work_item table with basic metadata for
-- any task, a chat_conversation table FK'd to it for this feature's own
-- attributes, and a comment table FK'd to work_item (not chat_conversation)
-- shared across every task type. These three tables here are that shape,
-- reproduced inside chat-routing-service's own chat_routing schema purely
-- so this feature's message/assignment persistence can be built and tested
-- today -- see the project's chat-persistence-mapping-plan.md doc for the
-- full field-by-field mapping and open questions. Once entity-service's
-- real version ships, csm-portal/backend's calls should move to that
-- service instead and these three tables should be dropped -- they are not
-- meant to become a second permanent source of truth.
CREATE TABLE chat_routing.work_item (
  id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  creator_id        TEXT NOT NULL,
  subject           TEXT NOT NULL,
  work_item_number  TEXT,
  created_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- conversation_id and case_id aren't in Sajith's description (open
-- questions in the mapping doc) -- included here as this feature's own
-- "specific attributes" since chat_conversation is exactly where he said
-- those belong. case_id is what csm-portal/backend and every message/
-- accept call actually has on hand (see routingclient.CaseInfo), so it's
-- the lookup key AddComment/SetConversationEngineer use to find the right
-- work_item -- not a stated part of the eventual real schema, just how
-- this stand-in bridges to the case identity that already exists
-- independently in entity-service's own cases table.
CREATE TABLE chat_routing.chat_conversation (
  work_item_id     UUID PRIMARY KEY REFERENCES chat_routing.work_item (id),
  conversation_id  TEXT NOT NULL,
  case_id          TEXT NOT NULL,
  -- NULL until Router.Accept confirms the engineer -- see that method's
  -- doc comment for why this is set there rather than at assignment time.
  engineer_id      TEXT,
  created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- AddComment/SetConversationEngineer both look up by case_id first.
CREATE UNIQUE INDEX idx_chat_conversation_case_id ON chat_routing.chat_conversation (case_id);

CREATE TABLE chat_routing.comment (
  id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  work_item_id  UUID NOT NULL REFERENCES chat_routing.work_item (id),
  content       TEXT NOT NULL,
  created_by    TEXT NOT NULL,
  created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Backs "full transcript for this work item, in order" (DebugWorkItem).
CREATE INDEX idx_comment_work_item_id ON chat_routing.comment (work_item_id, created_at);
