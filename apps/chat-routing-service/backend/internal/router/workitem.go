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

// This file is a local stand-in for entity-service's generic work-item
// schema (work_item / chat_conversation / comment), reproduced here so
// this feature's message and engineer-assignment persistence can be built
// and tested before that real schema exists. Once entity-service ships its
// own version, csm-portal/backend's calls should move there instead, and
// this file (plus its migration) should be deleted.
//
// These tables live in this service's own "chat_routing" schema rather
// than entity-service's, so every query below uses plain unqualified table
// names resolved via this service's own search_path. Keeping them here
// also means entity-service's eventual real migration can't collide with
// this stand-in on table names.
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

// ChatConversation mirrors the interim chat_conversation row. CaseID isn't
// part of entity-service's eventual real schema -- it's here because every
// caller of AddComment/Accept looks a conversation up by it.
type ChatConversation struct {
	WorkItemID string `json:"workItemId"`
	CaseID     string `json:"caseId"`
	// AssigneeID is nil until Router.Accept confirms the engineer.
	AssigneeID *string `json:"assigneeId,omitempty"`
	// State is this conversation's lifecycle stage: OPEN until Accept moves
	// it to ACTIVE (see setConversationAssignee). The database enum also
	// has RESOLVED/CONVERTED_CHAT/CONVERTED_CASE/ABANDONED/CLOSED, but
	// nothing sets those yet.
	State string `json:"state"`
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

// CreateWorkItem creates the work_item + chat_conversation pair (starting
// in state OPEN) for a brand-new escalation, plus its first comment if the
// triggering message is non-empty. Called once per case regardless of
// whether Escalate assigns or queues it -- routing outcome doesn't affect
// whether the record exists. This is a create, not an upsert: calling it
// twice for the same caseID is a caller bug this doesn't try to reconcile.
func (r *Router) CreateWorkItem(ctx context.Context, caseID, creatorEmail, subject, initialMessage string) error {
	return r.withTx(ctx, func(tx pgx.Tx) error {
		var workItemID string
		if err := tx.QueryRow(ctx, `
			INSERT INTO work_item (creator_id, subject) VALUES ($1, $2)
			RETURNING id
		`, creatorEmail, subject).Scan(&workItemID); err != nil {
			return fmt.Errorf("insert work_item: %w", err)
		}

		if _, err := tx.Exec(ctx, `
			INSERT INTO chat_conversation (work_item_id, case_id)
			VALUES ($1, $2)
		`, workItemID, caseID); err != nil {
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

// setConversationAssignee records which engineer accepted caseID's chat.
// Called from Accept in the same transaction as the PENDING -> BUSY flip,
// so the two can never disagree about whether an accept went through. A
// no-op if caseID has no chat_conversation row -- Accept's own PENDING/
// caseID check is the real authority on whether the accept is valid; this
// just records the byproduct of a valid one.
func setConversationAssignee(ctx context.Context, tx pgx.Tx, caseID, assigneeUserID string) error {
	if _, err := tx.Exec(ctx, `
		UPDATE chat_conversation SET assignee_id = $1, state = 'ACTIVE', updated_at = now()
		WHERE case_id = $2
	`, assigneeUserID, caseID); err != nil {
		return fmt.Errorf("set conversation assignee: %w", err)
	}
	return nil
}

// AddComment appends one message to caseID's transcript, looking up its
// work_item via chat_conversation.case_id. Used for both directions of the
// live chat so the whole transcript ends up in one place. Returns an error
// rather than a silent no-op if caseID has no chat_conversation row, since
// that means CreateWorkItem was never called for it -- worth surfacing
// even though current callers still treat this call as best-effort.
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

// DebugWorkItem returns caseID's full work item, chat conversation, and
// comment transcript in order -- for verification/debugging only.
func (r *Router) DebugWorkItem(ctx context.Context, caseID string) (WorkItemDetail, error) {
	var (
		detail     WorkItemDetail
		assigneeID *string
	)
	err := r.db.QueryRow(ctx, `
		SELECT w.id, w.creator_id, w.subject, COALESCE(w.work_item_number, ''),
		       c.work_item_id, c.case_id, c.assignee_id, c.state
		FROM chat_conversation c
		JOIN work_item w ON w.id = c.work_item_id
		WHERE c.case_id = $1
	`, caseID).Scan(
		&detail.WorkItem.ID, &detail.WorkItem.CreatorID, &detail.WorkItem.Subject, &detail.WorkItem.WorkItemNumber,
		&detail.Conversation.WorkItemID, &detail.Conversation.CaseID, &assigneeID, &detail.Conversation.State,
	)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return WorkItemDetail{}, fmt.Errorf("no work item for case %s", caseID)
	case err != nil:
		return WorkItemDetail{}, fmt.Errorf("debug work item: %w", err)
	}
	detail.Conversation.AssigneeID = assigneeID

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
