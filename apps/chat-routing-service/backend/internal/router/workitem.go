// Copyright (c) 2026 WSO2 LLC. (https://www.wso2.com).
//
// WSO2 LLC. licenses this file to you under the Apache License,
// Version 2.0 (the "License"); you may not use this file except
// in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied.  See the License for the
// specific language governing permissions and limitations
// under the License.

// This file implements a LOCAL STAND-IN for entity-service's incoming
// generic WORK_ITEM supertype schema (work_item / chat_conversation /
// comment) -- see migrations/000007_create_chat_stub_workitem_tables and
// the project's chat-persistence-mapping-plan.md doc for the full context.
// Sajith Ekanayaka described that real schema as still evolving; these
// three tables reproduce his interim shape inside this service's own
// database purely so this feature's message and engineer-assignment
// persistence can be built and tested today. Once entity-service's real
// version ships, csm-portal/backend's calls should move there instead, and
// everything in this file (plus migrations/000007) should be deleted.
package router

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// WorkItem mirrors the interim work_item row.
type WorkItem struct {
	ID             string `json:"id"`
	CreatorID      string `json:"creatorId"`
	Subject        string `json:"subject"`
	WorkItemNumber string `json:"workItemNumber,omitempty"`
}

// ChatConversation mirrors the interim chat_conversation row. ConversationID
// and CaseID are this stand-in's own additions -- not confirmed as part of
// the eventual real schema (see the mapping doc's open questions) -- added
// here because chat_conversation is exactly where Sajith said this
// feature's own attributes belong, and CaseID is what every caller of
// AddComment/Accept actually has on hand to look a conversation up by.
type ChatConversation struct {
	WorkItemID     string `json:"workItemId"`
	ConversationID string `json:"conversationId"`
	CaseID         string `json:"caseId"`
	// EngineerID is nil until Router.Accept confirms the engineer.
	EngineerID *string `json:"engineerId,omitempty"`
}

// Comment mirrors one row of the shared comment table.
type Comment struct {
	ID        string `json:"id"`
	Content   string `json:"content"`
	CreatedBy string `json:"createdBy"`
	CreatedAt string `json:"createdAt"`
}

// WorkItemDetail is DebugWorkItem's result.
type WorkItemDetail struct {
	WorkItem     WorkItem         `json:"workItem"`
	Conversation ChatConversation `json:"conversation"`
	// Comments is every message so far, oldest first -- the full transcript.
	Comments []Comment `json:"comments"`
}

// CreateWorkItem creates the work_item + chat_conversation pair for a
// brand-new escalation, plus its first comment (the message that triggered
// it, if non-empty). Called once from csm-portal/backend's HandleEscalate
// regardless of whether Router.Escalate assigns or queues the case -- the
// work item and its conversation exist the moment the case does, independent
// of routing outcome. caseID must not already have a chat_conversation row;
// this is a create, not an upsert -- calling it twice for the same caseID
// is a caller bug this method does not try to reconcile.
func (r *Router) CreateWorkItem(ctx context.Context, caseID, conversationID, creatorEmail, subject, initialMessage string) error {
	return r.withTx(ctx, func(tx pgx.Tx) error {
		var workItemID string
		if err := tx.QueryRow(ctx, `
			INSERT INTO work_item (creator_id, subject) VALUES ($1, $2)
			RETURNING id
		`, creatorEmail, subject).Scan(&workItemID); err != nil {
			return fmt.Errorf("insert work_item: %w", err)
		}

		if _, err := tx.Exec(ctx, `
			INSERT INTO chat_conversation (work_item_id, conversation_id, case_id)
			VALUES ($1, $2, $3)
		`, workItemID, conversationID, caseID); err != nil {
			return fmt.Errorf("insert chat_conversation: %w", err)
		}

		if initialMessage != "" {
			if _, err := tx.Exec(ctx, `
				INSERT INTO comment (work_item_id, content, created_by)
				VALUES ($1, $2, $3)
			`, workItemID, initialMessage, creatorEmail); err != nil {
				return fmt.Errorf("insert initial comment: %w", err)
			}
		}
		return nil
	})
}

// setConversationEngineer records which engineer accepted caseID's chat --
// called from Accept (see state.go), in the SAME transaction as the
// PENDING -> BUSY flip, so the two can never disagree about whether an
// accept actually went through. A no-op if caseID has no chat_conversation
// row (e.g. this stand-in was added after some in-flight cases already
// existed) -- Accept's own PENDING/caseID check is the real authority on
// whether the accept itself is valid; this is just where the byproduct of
// a valid accept gets recorded.
func setConversationEngineer(ctx context.Context, tx pgx.Tx, caseID, engineerEmail string) error {
	if _, err := tx.Exec(ctx, `
		UPDATE chat_conversation SET engineer_id = $1, updated_at = now()
		WHERE case_id = $2
	`, engineerEmail, caseID); err != nil {
		return fmt.Errorf("set conversation engineer: %w", err)
	}
	return nil
}

// AddComment appends one message to caseID's transcript, looking up its
// work_item via chat_conversation.case_id. Used for BOTH directions of the
// live chat (see csm-portal/backend's HandleCustomerMessage and
// HandleEngineerMessage) so the whole transcript ends up in one place
// instead of split across two storage systems -- matching Sajith's own
// description of one shared comment mechanism rather than a per-type one.
// Returns an error (not a silent no-op) if caseID has no chat_conversation
// row, since that means CreateWorkItem was never called for it -- a real
// bug worth surfacing even though today's callers of this method still
// treat the call itself as best-effort.
func (r *Router) AddComment(ctx context.Context, caseID, authorEmail, content string) error {
	return r.withTx(ctx, func(tx pgx.Tx) error {
		var workItemID string
		err := tx.QueryRow(ctx, `
			SELECT work_item_id FROM chat_conversation WHERE case_id = $1
		`, caseID).Scan(&workItemID)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			return fmt.Errorf("add comment: no chat_conversation for case %s", caseID)
		case err != nil:
			return fmt.Errorf("add comment: look up work item: %w", err)
		}

		if _, err := tx.Exec(ctx, `
			INSERT INTO comment (work_item_id, content, created_by) VALUES ($1, $2, $3)
		`, workItemID, content, authorEmail); err != nil {
			return fmt.Errorf("insert comment: %w", err)
		}
		return nil
	})
}

// DebugWorkItem returns caseID's full work item + chat conversation +
// comment transcript, in order -- verification-only, mirroring DebugState's
// own reason for existing (see that method's doc comment).
func (r *Router) DebugWorkItem(ctx context.Context, caseID string) (WorkItemDetail, error) {
	var (
		detail     WorkItemDetail
		engineerID *string
	)
	err := r.db.QueryRow(ctx, `
		SELECT w.id, w.creator_id, w.subject, COALESCE(w.work_item_number, ''),
		       c.work_item_id, c.conversation_id, c.case_id, c.engineer_id
		FROM chat_conversation c
		JOIN work_item w ON w.id = c.work_item_id
		WHERE c.case_id = $1
	`, caseID).Scan(
		&detail.WorkItem.ID, &detail.WorkItem.CreatorID, &detail.WorkItem.Subject, &detail.WorkItem.WorkItemNumber,
		&detail.Conversation.WorkItemID, &detail.Conversation.ConversationID, &detail.Conversation.CaseID, &engineerID,
	)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return WorkItemDetail{}, fmt.Errorf("no work item for case %s", caseID)
	case err != nil:
		return WorkItemDetail{}, fmt.Errorf("debug work item: %w", err)
	}
	detail.Conversation.EngineerID = engineerID

	rows, err := r.db.Query(ctx, `
		SELECT id, content, created_by, created_at FROM comment
		WHERE work_item_id = $1 ORDER BY created_at ASC
	`, detail.WorkItem.ID)
	if err != nil {
		return WorkItemDetail{}, fmt.Errorf("debug work item: query comments: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			c         Comment
			createdAt time.Time
		)
		if err := rows.Scan(&c.ID, &c.Content, &c.CreatedBy, &createdAt); err != nil {
			return WorkItemDetail{}, fmt.Errorf("debug work item: scan comment: %w", err)
		}
		c.CreatedAt = createdAt.Format(time.RFC3339)
		detail.Comments = append(detail.Comments, c)
	}
	return detail, rows.Err()
}
