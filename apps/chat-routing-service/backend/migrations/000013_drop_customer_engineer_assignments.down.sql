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

-- Restores customer_engineer_assignments exactly as 000003 created it. Rows
-- are not restorable (the table was dropped, not archived) -- same
-- "ephemeral working state" precedent as 000004's own down migration. By
-- the time this runs, 000014's down migration has already restored
-- chat_routing.engineers (with its email column) ahead of this one, since
-- down migrations run in reverse numeric order.
CREATE TABLE chat_routing.customer_engineer_assignments (
  customer_email  TEXT PRIMARY KEY,
  engineer_email  TEXT NOT NULL REFERENCES chat_routing.engineers (email),
  updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_customer_engineer_assignments_engineer
  ON chat_routing.customer_engineer_assignments (engineer_email);
