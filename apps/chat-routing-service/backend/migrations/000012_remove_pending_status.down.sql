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

ALTER TABLE chat_routing.engineers DROP CONSTRAINT IF EXISTS chk_accepted_only_with_case;

CREATE TYPE chat_routing.engineer_status_old AS ENUM ('AVAILABLE', 'BUSY', 'OFFLINE', 'PENDING');

ALTER TABLE chat_routing.engineers ALTER COLUMN status DROP DEFAULT;

-- A row currently "derived pending" (BUSY with no accepted_at yet) goes
-- back to being stored as PENDING directly.
ALTER TABLE chat_routing.engineers
  ALTER COLUMN status TYPE chat_routing.engineer_status_old
  USING (CASE
           WHEN status::text = 'BUSY' AND accepted_at IS NULL AND current_case_id IS NOT NULL THEN 'PENDING'
           ELSE status::text
         END)::chat_routing.engineer_status_old;

ALTER TABLE chat_routing.engineers ALTER COLUMN status SET DEFAULT 'OFFLINE';

DROP TYPE chat_routing.engineer_status;
ALTER TYPE chat_routing.engineer_status_old RENAME TO engineer_status;

ALTER TABLE chat_routing.engineers DROP COLUMN accepted_at;
