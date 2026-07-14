package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
)

func createAttachmentContractMember(t *testing.T) string {
	t.Helper()

	var userID string
	email := fmt.Sprintf("attachment-contract-%s@multica.ai", uuid.NewString())
	if err := testPool.QueryRow(context.Background(), `
		INSERT INTO "user" (name, email)
		VALUES ('Attachment Contract Member', $1)
		RETURNING id
	`, email).Scan(&userID); err != nil {
		t.Fatalf("create member user: %v", err)
	}
	if _, err := testPool.Exec(context.Background(), `
		INSERT INTO member (workspace_id, user_id, role)
		VALUES ($1, $2, 'member')
	`, testWorkspaceID, userID); err != nil {
		t.Fatalf("add workspace member: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM "user" WHERE id = $1`, userID)
	})
	return userID
}

func createAttachmentContractDraft(t *testing.T, uploaderID string, issueID *string) string {
	t.Helper()

	attachmentID := uuid.NewString()
	if _, err := testPool.Exec(context.Background(), `
		INSERT INTO attachment (
			id, workspace_id, issue_id, uploader_type, uploader_id,
			filename, url, content_type, size_bytes, lifecycle
		)
		VALUES ($1, $2, $3, 'member', $4, $5, $6, 'text/plain', 4, 'draft')
	`, attachmentID, testWorkspaceID, issueID, uploaderID, attachmentID+".txt", "https://cdn.example.com/"+attachmentID); err != nil {
		t.Fatalf("create draft attachment: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM attachment WHERE id = $1`, attachmentID)
	})
	return attachmentID
}

func createAttachmentContractIssue(t *testing.T, userID, title string, attachmentIDs []string) (string, *httptest.ResponseRecorder) {
	t.Helper()

	req := newRequest(http.MethodPost, "/api/issues", map[string]any{
		"title":          title,
		"attachment_ids": attachmentIDs,
	})
	req.Header.Set("X-User-ID", userID)
	w := httptest.NewRecorder()
	testHandler.CreateIssue(w, req)

	if w.Code != http.StatusCreated {
		return "", w
	}
	var response struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode create issue response: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM issue WHERE id = $1`, response.ID)
	})
	return response.ID, w
}

func deleteAttachmentContractAttachment(t *testing.T, userID, attachmentID string) *httptest.ResponseRecorder {
	t.Helper()
	req := newRequest(http.MethodDelete, "/api/attachments/"+attachmentID, nil)
	req.Header.Set("X-User-ID", userID)
	req = withURLParam(req, "id", attachmentID)
	w := httptest.NewRecorder()
	testHandler.DeleteAttachment(w, req)
	return w
}

func TestCreateIssueRejectsMixedOwnerAttachmentsAtomically(t *testing.T) {
	memberID := createAttachmentContractMember(t)
	ownedID := createAttachmentContractDraft(t, memberID, nil)
	foreignID := createAttachmentContractDraft(t, testUserID, nil)
	title := "attachment owner rollback " + uuid.NewString()

	_, w := createAttachmentContractIssue(t, memberID, title, []string{ownedID, foreignID})
	if w.Code != http.StatusConflict {
		t.Fatalf("CreateIssue: status = %d, want 409; body = %s", w.Code, w.Body.String())
	}

	var issueCount int
	if err := testPool.QueryRow(context.Background(), `
		SELECT COUNT(*) FROM issue WHERE workspace_id = $1 AND title = $2
	`, testWorkspaceID, title).Scan(&issueCount); err != nil {
		t.Fatalf("count rolled back issue: %v", err)
	}
	if issueCount != 0 {
		t.Fatalf("mixed-owner request created %d issue rows, want 0", issueCount)
	}

	var boundCount int
	if err := testPool.QueryRow(context.Background(), `
		SELECT COUNT(*) FROM attachment
		WHERE id = ANY($1::uuid[]) AND (issue_id IS NOT NULL OR lifecycle <> 'draft')
	`, []string{ownedID, foreignID}).Scan(&boundCount); err != nil {
		t.Fatalf("count attachment side effects: %v", err)
	}
	if boundCount != 0 {
		t.Fatalf("mixed-owner request changed %d attachments, want 0", boundCount)
	}
}

func TestCreateCommentRejectsMixedOwnerAttachmentsAtomically(t *testing.T) {
	memberID := createAttachmentContractMember(t)
	issueID, w := createAttachmentContractIssue(t, testUserID, "comment attachment owner "+uuid.NewString(), nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("seed issue: status = %d; body = %s", w.Code, w.Body.String())
	}
	ownedID := createAttachmentContractDraft(t, memberID, &issueID)
	foreignID := createAttachmentContractDraft(t, testUserID, &issueID)
	content := "must roll back " + uuid.NewString()

	req := newRequest(http.MethodPost, "/api/issues/"+issueID+"/comments", map[string]any{
		"content":        content,
		"attachment_ids": []string{ownedID, foreignID},
	})
	req.Header.Set("X-User-ID", memberID)
	req = withURLParam(req, "id", issueID)
	w = httptest.NewRecorder()
	testHandler.CreateComment(w, req)
	if w.Code != http.StatusConflict {
		t.Fatalf("CreateComment: status = %d, want 409; body = %s", w.Code, w.Body.String())
	}

	var commentCount int
	if err := testPool.QueryRow(context.Background(), `
		SELECT COUNT(*) FROM comment WHERE issue_id = $1 AND content = $2
	`, issueID, content).Scan(&commentCount); err != nil {
		t.Fatalf("count rolled back comment: %v", err)
	}
	if commentCount != 0 {
		t.Fatalf("mixed-owner request created %d comment rows, want 0", commentCount)
	}

	var boundCount int
	if err := testPool.QueryRow(context.Background(), `
		SELECT COUNT(*) FROM attachment
		WHERE id = ANY($1::uuid[]) AND (comment_id IS NOT NULL OR lifecycle <> 'draft')
	`, []string{ownedID, foreignID}).Scan(&boundCount); err != nil {
		t.Fatalf("count attachment side effects: %v", err)
	}
	if boundCount != 0 {
		t.Fatalf("mixed-owner request changed %d attachments, want 0", boundCount)
	}
}

func TestAttachmentDraftCommittedDeleteBoundary(t *testing.T) {
	t.Run("draft can be deleted", func(t *testing.T) {
		attachmentID := createAttachmentContractDraft(t, testUserID, nil)
		w := deleteAttachmentContractAttachment(t, testUserID, attachmentID)
		if w.Code != http.StatusNoContent {
			t.Fatalf("DeleteAttachment(draft): status = %d, want 204; body = %s", w.Code, w.Body.String())
		}
	})

	t.Run("issue bind wins before delete", func(t *testing.T) {
		attachmentID := createAttachmentContractDraft(t, testUserID, nil)
		issueID, w := createAttachmentContractIssue(t, testUserID, "bind before delete "+uuid.NewString(), []string{attachmentID})
		if w.Code != http.StatusCreated {
			t.Fatalf("CreateIssue: status = %d; body = %s", w.Code, w.Body.String())
		}

		w = deleteAttachmentContractAttachment(t, testUserID, attachmentID)
		if w.Code != http.StatusConflict {
			t.Fatalf("DeleteAttachment(committed): status = %d, want 409; body = %s", w.Code, w.Body.String())
		}

		var lifecycle, boundIssueID string
		if err := testPool.QueryRow(context.Background(), `
			SELECT lifecycle, issue_id::text FROM attachment WHERE id = $1
		`, attachmentID).Scan(&lifecycle, &boundIssueID); err != nil {
			t.Fatalf("load committed attachment: %v", err)
		}
		if lifecycle != "committed" || boundIssueID != issueID {
			t.Fatalf("attachment state = (%q, %q), want (committed, %q)", lifecycle, boundIssueID, issueID)
		}
	})

	t.Run("delete wins before bind", func(t *testing.T) {
		attachmentID := createAttachmentContractDraft(t, testUserID, nil)
		w := deleteAttachmentContractAttachment(t, testUserID, attachmentID)
		if w.Code != http.StatusNoContent {
			t.Fatalf("DeleteAttachment(draft): status = %d, want 204; body = %s", w.Code, w.Body.String())
		}

		title := "delete before bind " + uuid.NewString()
		_, w = createAttachmentContractIssue(t, testUserID, title, []string{attachmentID})
		if w.Code != http.StatusConflict {
			t.Fatalf("CreateIssue(deleted attachment): status = %d, want 409; body = %s", w.Code, w.Body.String())
		}
		var issueCount int
		if err := testPool.QueryRow(context.Background(), `
			SELECT COUNT(*) FROM issue WHERE workspace_id = $1 AND title = $2
		`, testWorkspaceID, title).Scan(&issueCount); err != nil {
			t.Fatalf("count rolled back issue: %v", err)
		}
		if issueCount != 0 {
			t.Fatalf("deleted attachment request created %d issue rows, want 0", issueCount)
		}
	})

	t.Run("comment bind wins before delete", func(t *testing.T) {
		issueID, w := createAttachmentContractIssue(t, testUserID, "comment bind before delete "+uuid.NewString(), nil)
		if w.Code != http.StatusCreated {
			t.Fatalf("seed issue: status = %d; body = %s", w.Code, w.Body.String())
		}
		attachmentID := createAttachmentContractDraft(t, testUserID, &issueID)

		req := newRequest(http.MethodPost, "/api/issues/"+issueID+"/comments", map[string]any{
			"content":        "bind attachment",
			"attachment_ids": []string{attachmentID},
		})
		req = withURLParam(req, "id", issueID)
		w = httptest.NewRecorder()
		testHandler.CreateComment(w, req)
		if w.Code != http.StatusCreated {
			t.Fatalf("CreateComment: status = %d; body = %s", w.Code, w.Body.String())
		}

		w = deleteAttachmentContractAttachment(t, testUserID, attachmentID)
		if w.Code != http.StatusConflict {
			t.Fatalf("DeleteAttachment(comment attachment): status = %d, want 409; body = %s", w.Code, w.Body.String())
		}
	})

	t.Run("foreign draft cannot be deleted", func(t *testing.T) {
		memberID := createAttachmentContractMember(t)
		attachmentID := createAttachmentContractDraft(t, testUserID, nil)
		w := deleteAttachmentContractAttachment(t, memberID, attachmentID)
		if w.Code != http.StatusForbidden {
			t.Fatalf("DeleteAttachment(foreign draft): status = %d, want 403; body = %s", w.Code, w.Body.String())
		}
	})
}
