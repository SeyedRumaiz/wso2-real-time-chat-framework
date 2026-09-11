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

-- Supports converting a chat into a real entity-service case
-- (Router.ConvertToCase), per the chat-first escalation plan: a chat starts
-- with only chat-routing-service's own LOCAL STAND-IN identity (case_id,
-- already just an opaque TEXT key -- see workitem.go), and a real
-- entity-service case is only ever created later, explicitly, by the
-- assigned engineer. entity_case_id records that real case's ID once (and
-- only if) that happens.
--
-- No FK: entity-service's cases table lives in a different service's data
-- model entirely, so this is a plain reference, not a database-enforced
-- one. NULL for the entire life of a chat that never converts.
ALTER TABLE chat_routing.chat_conversation
  ADD COLUMN entity_case_id TEXT NULL;
