-- chats maps a customer's chat/case to the engineer eventually assigned to
-- it, so the CSM portal, chat-routing-service, and any future reporting
-- can look up "what's the state of this chat" by the one identifier every
-- other part of this feature already agrees on: case_id.
--
-- case_id is intentionally NOT a foreign key into cases(id), and cust_id/
-- engineer_id are intentionally NOT foreign keys into users(id), even
-- though those tables exist locally. As long as this service runs on the
-- ServiceNow data source, cases/users/case_comments are never actually
-- populated locally (CreateCase, CreateCaseComment, and friends post
-- straight to ServiceNow -- see internal/service/sn_case_service.go's
-- CreateCase/CreateCaseComment; the pgFallback passed into
-- NewServiceNowCaseService is constructed but never called). A real FK
-- here would fail on every single insert today, since the referenced rows
-- never exist locally. Once ServiceNow is removed and this database
-- becomes the actual source of truth for cases/users, add the FK
-- constraints back in a follow-up migration -- at that point the
-- referenced rows will actually exist.
--
-- engineer_id is nullable: a chat is inserted the moment a case is
-- created (engineer_id NULL), and updated once chat-routing-service's
-- sticky-match-then-least-busy-pick logic (see
-- apps/chat-routing-service/backend/internal/router/state.go's Escalate)
-- finds someone. Both cust_id and engineer_id are values from the same
-- identifier space -- entity-service's users.id -- just playing different
-- roles in this row (see users.user_type: 'customer' vs 'internal').
CREATE TABLE chats (
  case_id     TEXT PRIMARY KEY,
  cust_id     TEXT NOT NULL,
  engineer_id TEXT,
  created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Supports "what chats is this customer/engineer involved in" lookups.
-- engineer_id's index is partial -- most of the time we're searching for
-- an engineer's chats, which by definition already have one -- no reason
-- to index the NULL rows too.
CREATE INDEX idx_chats_cust_id ON chats (cust_id);
CREATE INDEX idx_chats_engineer_id ON chats (engineer_id) WHERE engineer_id IS NOT NULL;
