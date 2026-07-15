package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/multica-ai/multica/server/pkg/agent"
)

func uploadQuickCreateDraft(t *testing.T, filename string) string {
	t.Helper()

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("file", filename)
	if err != nil {
		t.Fatalf("create upload part: %v", err)
	}
	if _, err := part.Write([]byte("quick-create attachment")); err != nil {
		t.Fatalf("write upload part: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close upload body: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/upload-file", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("X-User-ID", testUserID)
	req.Header.Set("X-Workspace-ID", testWorkspaceID)
	w := httptest.NewRecorder()
	testHandler.UploadFile(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("upload draft: got %d: %s", w.Code, w.Body.String())
	}
	var response AttachmentResponse
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode upload response: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM attachment WHERE id = $1`, response.ID)
	})
	return response.ID
}

func enqueueQuickCreateWithAttachments(t *testing.T, agentID string, attachmentIDs []string) string {
	t.Helper()
	w := httptest.NewRecorder()
	req := newRequest(http.MethodPost, "/api/issues/quick-create", map[string]any{
		"agent_id":       agentID,
		"prompt":         "Create an issue with the reserved attachments",
		"attachment_ids": attachmentIDs,
	})
	testHandler.QuickCreateIssue(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("enqueue quick-create: got %d: %s", w.Code, w.Body.String())
	}
	var response QuickCreateIssueResponse
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode quick-create response: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE id = $1`, response.TaskID)
	})
	return response.TaskID
}

func createIssueAsQuickCreateTask(t *testing.T, agentID, taskID string, attachmentIDs []string, taskToken bool) *httptest.ResponseRecorder {
	t.Helper()
	req := newRequest(http.MethodPost, "/api/issues", map[string]any{
		"title":          "quick-create reservation " + uuid.NewString(),
		"attachment_ids": attachmentIDs,
		"origin_type":    "quick_create",
		"origin_id":      taskID,
	})
	if taskToken {
		req.Header.Set("X-Actor-Source", "task_token")
		req.Header.Set("X-Agent-ID", agentID)
		req.Header.Set("X-Task-ID", taskID)
	}
	w := httptest.NewRecorder()
	testHandler.CreateIssue(w, req)
	return w
}

func assertQuickCreateReservation(t *testing.T, attachmentID, taskID string) {
	t.Helper()
	var gotTask, lifecycle string
	if err := testPool.QueryRow(context.Background(), `
		SELECT COALESCE(quick_create_task_id::text, ''), lifecycle
		FROM attachment WHERE id = $1
	`, attachmentID).Scan(&gotTask, &lifecycle); err != nil {
		t.Fatalf("load attachment reservation: %v", err)
	}
	if gotTask != taskID || lifecycle != "draft" {
		t.Fatalf("attachment %s reservation=(%q,%q), want (%q,draft)", attachmentID, gotTask, lifecycle, taskID)
	}
}

func TestQuickCreateAttachmentReservationEndToEnd(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	rootT := t

	originalStorage := testHandler.Storage
	testHandler.Storage = &mockStorage{}
	t.Cleanup(func() { testHandler.Storage = originalStorage })

	runtimeID := handlerTestRuntimeID(t)
	var previousMetadata []byte
	var previousStatus string
	if err := testPool.QueryRow(context.Background(), `
		SELECT metadata, status FROM agent_runtime WHERE id = $1
	`, runtimeID).Scan(&previousMetadata, &previousStatus); err != nil {
		t.Fatalf("load runtime state: %v", err)
	}
	if _, err := testPool.Exec(context.Background(), `
		UPDATE agent_runtime
		SET metadata = jsonb_build_object('cli_version', $1::text), status = 'online'
		WHERE id = $2
	`, agent.MinQuickCreateCLIVersion, runtimeID); err != nil {
		t.Fatalf("prepare quick-create runtime: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `
			UPDATE agent_runtime SET metadata = $1, status = $2 WHERE id = $3
		`, previousMetadata, previousStatus, runtimeID)
	})

	agentID := createHandlerTestAgent(t, "Quick Create Reservation "+uuid.NewString(), []byte("[]"))
	attachmentA := uploadQuickCreateDraft(t, "quick-a-"+uuid.NewString()+".txt")
	attachmentB := createAttachmentContractDraft(t, testUserID, nil)
	unreserved := createAttachmentContractDraft(t, testUserID, nil)

	t.Run("missing transaction starter fails closed", func(t *testing.T) {
		draft := createAttachmentContractDraft(t, testUserID, nil)
		var before int
		if err := testPool.QueryRow(context.Background(), `
			SELECT count(*) FROM agent_task_queue
			WHERE agent_id = $1 AND context->>'type' = 'quick_create'
		`, agentID).Scan(&before); err != nil {
			t.Fatalf("count tasks before missing transaction starter: %v", err)
		}

		originalTxStarter := testHandler.TaskService.TxStarter
		testHandler.TaskService.TxStarter = nil
		defer func() { testHandler.TaskService.TxStarter = originalTxStarter }()

		w := httptest.NewRecorder()
		req := newRequest(http.MethodPost, "/api/issues/quick-create", map[string]any{
			"agent_id":       agentID,
			"prompt":         "must fail without a transaction starter",
			"attachment_ids": []string{draft},
		})
		testHandler.QuickCreateIssue(w, req)
		if w.Code != http.StatusInternalServerError {
			t.Fatalf("missing transaction starter: got %d: %s", w.Code, w.Body.String())
		}

		var after int
		if err := testPool.QueryRow(context.Background(), `
			SELECT count(*) FROM agent_task_queue
			WHERE agent_id = $1 AND context->>'type' = 'quick_create'
		`, agentID).Scan(&after); err != nil {
			t.Fatalf("count tasks after missing transaction starter: %v", err)
		}
		if after != before {
			t.Fatalf("missing transaction starter leaked a task: before=%d after=%d", before, after)
		}
		assertQuickCreateReservation(t, draft, "")
	})

	t.Run("enqueue rejects a foreign draft atomically", func(t *testing.T) {
		foreignMember := createAttachmentContractMember(t)
		foreignDraft := createAttachmentContractDraft(t, foreignMember, nil)
		var before int
		if err := testPool.QueryRow(context.Background(), `
			SELECT count(*) FROM agent_task_queue
			WHERE agent_id = $1 AND context->>'type' = 'quick_create'
		`, agentID).Scan(&before); err != nil {
			t.Fatalf("count tasks before rejection: %v", err)
		}
		w := httptest.NewRecorder()
		req := newRequest(http.MethodPost, "/api/issues/quick-create", map[string]any{
			"agent_id":       agentID,
			"prompt":         "must reject foreign attachment",
			"attachment_ids": []string{foreignDraft},
		})
		testHandler.QuickCreateIssue(w, req)
		if w.Code != http.StatusConflict {
			t.Fatalf("foreign draft enqueue: got %d: %s", w.Code, w.Body.String())
		}
		var after int
		if err := testPool.QueryRow(context.Background(), `
			SELECT count(*) FROM agent_task_queue
			WHERE agent_id = $1 AND context->>'type' = 'quick_create'
		`, agentID).Scan(&after); err != nil {
			t.Fatalf("count tasks after rejection: %v", err)
		}
		if after != before {
			t.Fatalf("rejected reservation leaked a task: before=%d after=%d", before, after)
		}
		assertQuickCreateReservation(t, foreignDraft, "")
	})

	taskID := enqueueQuickCreateWithAttachments(t, agentID, []string{attachmentA, attachmentB})
	assertQuickCreateReservation(t, attachmentA, taskID)
	assertQuickCreateReservation(t, attachmentB, taskID)
	if _, err := testPool.Exec(context.Background(), `
		UPDATE agent_task_queue SET status = 'running', started_at = now() WHERE id = $1
	`, taskID); err != nil {
		t.Fatalf("mark quick-create task running: %v", err)
	}

	t.Run("member cannot forge quick-create origin", func(t *testing.T) {
		w := createIssueAsQuickCreateTask(t, agentID, taskID, []string{attachmentA, attachmentB}, false)
		if w.Code != http.StatusForbidden {
			t.Fatalf("forged origin: got %d: %s", w.Code, w.Body.String())
		}
		assertQuickCreateReservation(t, attachmentA, taskID)
		assertQuickCreateReservation(t, attachmentB, taskID)
	})

	t.Run("subset and unreserved injection roll back", func(t *testing.T) {
		for name, ids := range map[string][]string{
			"subset":    {attachmentA},
			"injected":  {attachmentA, attachmentB, unreserved},
			"duplicate": {attachmentA, attachmentA},
		} {
			t.Run(name, func(t *testing.T) {
				w := createIssueAsQuickCreateTask(t, agentID, taskID, ids, true)
				if w.Code != http.StatusConflict {
					t.Fatalf("mismatched reservation: got %d: %s", w.Code, w.Body.String())
				}
				assertQuickCreateReservation(t, attachmentA, taskID)
				assertQuickCreateReservation(t, attachmentB, taskID)
				assertQuickCreateReservation(t, unreserved, "")
			})
		}
	})

	otherTaskAttachment := createAttachmentContractDraft(t, testUserID, nil)
	otherTaskID := enqueueQuickCreateWithAttachments(t, agentID, []string{otherTaskAttachment})
	assertQuickCreateReservation(t, otherTaskAttachment, otherTaskID)
	t.Run("another task reservation is rejected", func(t *testing.T) {
		w := createIssueAsQuickCreateTask(t, agentID, taskID, []string{attachmentA, attachmentB, otherTaskAttachment}, true)
		if w.Code != http.StatusConflict {
			t.Fatalf("cross-task reservation: got %d: %s", w.Code, w.Body.String())
		}
		assertQuickCreateReservation(t, otherTaskAttachment, otherTaskID)
	})

	t.Run("generic agent owner check remains strict", func(t *testing.T) {
		ordinaryTaskID := createHandlerTestTaskForAgent(t, agentID)
		req := newRequest(http.MethodPost, "/api/issues", map[string]any{
			"title":          "generic owner check " + uuid.NewString(),
			"attachment_ids": []string{unreserved},
		})
		req.Header.Set("X-Agent-ID", agentID)
		req.Header.Set("X-Task-ID", ordinaryTaskID)
		w := httptest.NewRecorder()
		testHandler.CreateIssue(w, req)
		if w.Code != http.StatusConflict {
			t.Fatalf("generic agent binding member draft: got %d: %s", w.Code, w.Body.String())
		}
		assertQuickCreateReservation(t, unreserved, "")
	})

	t.Run("exact task reservation commits atomically", func(t *testing.T) {
		w := createIssueAsQuickCreateTask(t, agentID, taskID, []string{attachmentA, attachmentB}, true)
		if w.Code != http.StatusCreated {
			t.Fatalf("exact reservation: got %d: %s", w.Code, w.Body.String())
		}
		var response IssueResponse
		if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
			t.Fatalf("decode created issue: %v", err)
		}
		rootT.Cleanup(func() {
			testPool.Exec(context.Background(), `DELETE FROM issue WHERE id = $1`, response.ID)
		})
		for _, attachmentID := range []string{attachmentA, attachmentB} {
			var issueID, lifecycle, reservation, uploaderType, uploaderID string
			if err := testPool.QueryRow(context.Background(), `
				SELECT COALESCE(issue_id::text, ''), lifecycle,
				       COALESCE(quick_create_task_id::text, ''), uploader_type, uploader_id::text
				FROM attachment WHERE id = $1
			`, attachmentID).Scan(&issueID, &lifecycle, &reservation, &uploaderType, &uploaderID); err != nil {
				t.Fatalf("load committed attachment: %v", err)
			}
			if issueID != response.ID || lifecycle != "committed" || reservation != "" || uploaderType != "member" || uploaderID != testUserID {
				t.Fatalf("committed attachment state=(%s,%s,%s,%s,%s), issue=%s", issueID, lifecycle, reservation, uploaderType, uploaderID, response.ID)
			}
		}
	})

	t.Run("terminal task states release unused drafts", func(t *testing.T) {
		if _, err := testPool.Exec(context.Background(), `
			UPDATE agent_task_queue SET status = 'cancelled', completed_at = now() WHERE id = $1
		`, otherTaskID); err != nil {
			t.Fatalf("cancel quick-create task: %v", err)
		}
		assertQuickCreateReservation(t, otherTaskAttachment, "")

		failedAttachment := createAttachmentContractDraft(t, testUserID, nil)
		failedTaskID := enqueueQuickCreateWithAttachments(t, agentID, []string{failedAttachment})
		if _, err := testPool.Exec(context.Background(), `
			UPDATE agent_task_queue SET status = 'failed', completed_at = now() WHERE id = $1
		`, failedTaskID); err != nil {
			t.Fatalf("fail quick-create task: %v", err)
		}
		assertQuickCreateReservation(t, failedAttachment, "")
	})

	var leaked int
	if err := testPool.QueryRow(context.Background(), `
		SELECT count(*) FROM issue
		WHERE workspace_id = $1 AND origin_type = 'quick_create' AND origin_id = $2
	`, testWorkspaceID, taskID).Scan(&leaked); err != nil {
		t.Fatalf("count quick-created issue: %v", err)
	}
	if leaked != 1 {
		t.Fatalf("quick-create task produced %d issues, want exactly 1", leaked)
	}
}
