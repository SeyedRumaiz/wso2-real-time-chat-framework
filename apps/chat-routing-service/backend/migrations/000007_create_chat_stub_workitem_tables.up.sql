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
-- to build against meanwhile: a work_item table consists basic meta data
-- for any kind of task like id (uuid, PK), creator_id, subject, created
-- time, work_item_number; a chat_conversation table having a FK to
-- work_item table, which contains any specific attributes to the chat; and
-- a comment table which contains a FK to work_item table (not
-- chat_conversation, since work_item is extended for many other task types
-- such as support case, service request, incident etc, and comment covers
-- all of those without a separate comment table per type), content
-- (string), created by, created_at. These three tables here are that
-- shape, reproduced inside chat-routing-service's own chat_routing schema
-- purely so this feature's message/assignment persistence can be built and
-- tested today -- see the project's chat-persistence-mapping-plan.md doc
-- for the full field-by-field mapping and open questions. Once
-- entity-service's real version ships, csm-portal/backend's calls should
-- move to that service instead and these three tables should be dropped --
-- they are not meant to become a second permanent source of truth.
--
-- A same-schema-as-entity-service version of this migration was tried and
-- reverted on 2026-09-03 -- see the project's chat-persistence-mapping-plan.md
-- for why keeping this in its own schema (this service's own, separate
-- from entity-service's) was kept instead: two separate services each
-- owning their own schema is a normal, sound pattern, and this schema's
-- migration history staying independent of entity-service's own
-- (chat_routing_schema_migrations vs entity-service's schema_migrations)
-- avoids any version or table-name collision with entity-service's own
-- eventual real work_item/chat_conversation/comment migration.
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
