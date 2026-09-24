package infoflow

// integration_db_test.go — the DB-backed closure N4 flagged as missing.
//
// The protocol-layer tests prove the adapter before and after the Router; they
// do not prove the Router's persistence segment, which is where the two rework
// findings lived:
//
//   - B1: the issue.origin_type CHECK had no `infoflow_chat` member, so an
//     InfoFlow /issue died at INSERT with SQLSTATE 23514. Asserted here by
//     inserting an issue row with that origin against the migrated schema.
//   - W2: RegisterBYO bypassed the shared app_id slot serialization, so a
//     revoked installation permanently held the slot and a live conflict
//     surfaced as a raw 500. Asserted here by installing, revoking, and
//     re-installing the same application id for a second agent.
//
// It skips when no migrated database is reachable, matching the other
// integration DB tests in this tree.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/integrations/channel"
	"github.com/multica-ai/multica/server/internal/integrations/channel/engine"
	"github.com/multica-ai/multica/server/internal/service"
	dbfx "github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	"github.com/multica-ai/multica/server/internal/util/secretbox"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

func infoflowDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://multica:multica@localhost:5432/multica?sslmode=disable"
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Skipf("no database: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Skipf("database not reachable: %v", err)
	}
	var migrated bool
	if err := pool.QueryRow(ctx, `
SELECT to_regclass('public.channel_installation') IS NOT NULL
   AND to_regclass('public.channel_task_delivery') IS NOT NULL
`).Scan(&migrated); err != nil || !migrated {
		pool.Close()
		t.Skip("channel tables not present (database not migrated)")
	}
	t.Cleanup(pool.Close)
	return pool
}

type infoflowTestIssues struct{}

func (infoflowTestIssues) Create(context.Context, service.IssueCreateParams, service.IssueCreateOpts) (service.IssueCreateResult, error) {
	return service.IssueCreateResult{}, errors.New("no /issue in this test")
}
func (infoflowTestIssues) PublishAttachmentsChanged(context.Context, db.Issue, pgtype.UUID) {}

type infoflowTestTasks struct{}

func (infoflowTestTasks) EnqueueChannelChatTask(context.Context, db.ChatSession, pgtype.UUID, bool, int64, pgtype.UUID, int64) (db.AgentTaskQueue, error) {
	return db.AgentTaskQueue{}, nil
}
func (infoflowTestTasks) PrepareChatTaskEnqueue(context.Context, pgtype.UUID, pgtype.UUID) (service.PreparedChatTaskEnqueue, error) {
	return service.PreparedChatTaskEnqueue{}, nil
}
func (infoflowTestTasks) EnqueuePreparedChannelChatTaskInTx(context.Context, pgx.Tx, db.ChatSession, pgtype.UUID, bool, int64, service.PreparedChatTaskEnqueue) (db.AgentTaskQueue, error) {
	return db.AgentTaskQueue{}, nil
}
func (infoflowTestTasks) FinalizeChatTaskEnqueue(context.Context, db.AgentTaskQueue) {}
func (infoflowTestTasks) PromoteChannelChatTasksIfMediaReady(context.Context, pgtype.UUID) error {
	return nil
}
func (infoflowTestTasks) PromoteDeferredChannelIssueTask(context.Context, pgtype.UUID) error {
	return nil
}

// infoflowTokenServer answers the two InfoFlow calls the install and outbound
// paths make: the token mint and the group send.
type infoflowSendRecorder struct {
	mu     sync.Mutex
	paths  []string
	bodies []string
}

func (r *infoflowSendRecorder) record(path, body string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.paths = append(r.paths, path)
	r.bodies = append(r.bodies, body)
}

func (r *infoflowSendRecorder) sent() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.paths...)
}

func infoflowTokenServer(t *testing.T) (*httptest.Server, *infoflowSendRecorder) {
	t.Helper()
	rec := &infoflowSendRecorder{}
	mux := http.NewServeMux()
	mux.HandleFunc("/auth/app_access_token", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"code":"ok","data":{"app_access_token":"at_test","expire":7200}}`))
	})
	send := func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		rec.record(r.URL.Path, string(body))
		_, _ = w.Write([]byte(`{"code":"ok","data":{"messageid":"sent-1"}}`))
	}
	mux.HandleFunc("/robot/msg/groupmsgsend", send)
	mux.HandleFunc("/app/message/send", send)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, rec
}

func TestInfoFlowDBInstallInboundPersistOutbound(t *testing.T) {
	pool := infoflowDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	queries := db.New(pool)
	fx := dbfx.New(pool, "", "")
	suffix := util.UUIDToString(dbid.NewV7())
	fx.UserID = fx.User(t, "Owner", "infoflow-owner-"+suffix+"@multica.test")
	fx.WorkspaceID = fx.Workspace(t, "InfoFlow", "infoflow-"+suffix)
	fx.Member(t, fx.WorkspaceID, fx.UserID, "owner")
	runtimeID := fx.Runtime(t, "Runtime")
	agentID := fx.Agent(t, "Agent", runtimeID)
	// A second agent for the W2 reclaim/re-install leg.
	agentBID := fx.Agent(t, "Agent B", runtimeID)
	fx.Cleanup(t, "DELETE FROM channel_installation WHERE workspace_id = $1", fx.WorkspaceID)

	appID := "app-" + suffix
	srv, rec := infoflowTokenServer(t)
	box, err := secretbox.New(bytes.Repeat([]byte{0x5A}, secretbox.KeySize))
	if err != nil {
		t.Fatal(err)
	}
	svc, err := NewInstallService(queries, pool, box, nil)
	if err != nil {
		t.Fatal(err)
	}
	svc.http = srv.Client()
	svc.apiBase = srv.URL

	install, err := svc.RegisterBYO(ctx, RegisterBYOParams{
		WorkspaceID:   util.MustParseUUID(fx.WorkspaceID),
		AgentID:       util.MustParseUUID(agentID),
		InitiatorID:   util.MustParseUUID(fx.UserID),
		ApplicationID: appID,
		AppKey:        "key-" + suffix,
		AppSecret:     "secret-" + suffix,
		BotUserID:     "bot-user",
	})
	if err != nil {
		t.Fatalf("RegisterBYO: %v", err)
	}
	if install.Status != "active" {
		t.Fatalf("installation status = %q", install.Status)
	}

	// A live foreign owner must be refused with the precise sentinel, not a raw
	// 23505/500.
	if _, err := svc.RegisterBYO(ctx, RegisterBYOParams{
		WorkspaceID:   util.MustParseUUID(fx.WorkspaceID),
		AgentID:       util.MustParseUUID(agentBID),
		InitiatorID:   util.MustParseUUID(fx.UserID),
		ApplicationID: appID,
		AppKey:        "key-" + suffix,
		AppSecret:     "secret-" + suffix,
	}); !errors.Is(err, ErrAppOwnedBySameWorkspace) {
		t.Fatalf("live same-workspace conflict = %v, want ErrAppOwnedBySameWorkspace", err)
	}

	// B1: the widened origin CHECK accepts infoflow_chat on the migrated schema.
	if _, err := pool.Exec(ctx, `
INSERT INTO issue (workspace_id, number, title, status, priority, creator_id, creator_type, origin_type, origin_id)
VALUES ($1, 9000001, 'infoflow origin', 'backlog', 'none', $2, 'member', 'infoflow_chat', $3)
`, fx.WorkspaceID, fx.UserID, util.UUIDToString(install.ID)); err != nil {
		t.Fatalf("infoflow_chat origin rejected by issue_origin_type_check (B1): %v", err)
	}

	// Bind the sender so the inbound pipeline resolves an identity.
	senderID := "user-" + suffix
	if _, err := queries.CreateChannelUserBinding(ctx, db.CreateChannelUserBindingParams{
		WorkspaceID:    util.MustParseUUID(fx.WorkspaceID),
		MulticaUserID:  util.MustParseUUID(fx.UserID),
		InstallationID: install.ID,
		ChannelType:    string(TypeInfoFlow),
		ChannelUserID:  senderID,
		Config:         []byte(`{}`),
	}); err != nil {
		t.Fatalf("CreateChannelUserBinding: %v", err)
	}

	// Inbound through the REAL Router + shared chat-session service.
	router := engine.NewRouter(infoflowTestIssues{}, infoflowTestTasks{}, queries, engine.RouterConfig{})
	router.Register(TypeInfoFlow, NewResolverSet(queries, pool, nil))

	msgID := "msg-" + suffix
	inbound := channel.InboundMessage{
		EventID:     msgID,
		MessageID:   msgID,
		Type:        channel.MsgTypeText,
		Text:        "hello from infoflow",
		CommandText: "hello from infoflow",
		Source: channel.Source{
			ChannelType: TypeInfoFlow,
			ChatID:      senderID,
			ChatType:    channel.ChatTypeP2P,
			SenderID:    senderID,
		},
		AddressedToBot: true,
		Raw:            json.RawMessage(`{"agentid":"` + appID + `","FromUserId":"` + senderID + `","Content":"hello from infoflow"}`),
	}
	if err := router.Handle(ctx, inbound); err != nil {
		t.Fatalf("router.Handle: %v", err)
	}

	var chatMessageID, sessionID pgtype.UUID
	if err := pool.QueryRow(ctx, `
SELECT cm.id, cm.chat_session_id
FROM chat_message cm
JOIN chat_session cs ON cs.id = cm.chat_session_id
WHERE cs.workspace_id = $1 AND cm.role = 'user'
ORDER BY cm.created_at DESC LIMIT 1`, fx.WorkspaceID).Scan(&chatMessageID, &sessionID); err != nil {
		t.Fatalf("no durable chat_message written (Router persistence gap): %v", err)
	}
	var bindingCount int
	if err := pool.QueryRow(ctx, `
SELECT count(*) FROM channel_chat_session_binding
WHERE chat_session_id = $1 AND channel_type = $2`, sessionID, string(TypeInfoFlow)).Scan(&bindingCount); err != nil {
		t.Fatalf("count channel_chat_session_binding: %v", err)
	}
	if bindingCount != 1 {
		t.Fatalf("channel_chat_session_binding rows = %d, want 1", bindingCount)
	}
	var binding db.ChannelChatSessionBinding
	if err := pool.QueryRow(ctx, `
SELECT id, chat_session_id, installation_id, channel_type, channel_chat_id, chat_type, config
FROM channel_chat_session_binding WHERE chat_session_id = $1 AND channel_type = $2`,
		sessionID, string(TypeInfoFlow)).Scan(
		&binding.ID, &binding.ChatSessionID, &binding.InstallationID, &binding.ChannelType,
		&binding.ChannelChatID, &binding.ChatType, &binding.Config); err != nil {
		t.Fatalf("load binding: %v", err)
	}

	// Outbound leg: a queued task delivery for this session must reach the
	// InfoFlow send endpoint.
	taskID := fx.Task(t, agentID, dbfx.Cols{
		"status":          "completed",
		"chat_session_id": util.UUIDToString(sessionID),
		"runtime_id":      runtimeID,
	})
	if _, err := pool.Exec(ctx, `
INSERT INTO channel_task_delivery (
    task_id, binding_id, installation_id, channel_type, channel_chat_id, chat_type,
    channel_message_id, channel_sender_id, route_revision, config
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 0, $9)`,
		taskID, util.UUIDToString(binding.ID), util.UUIDToString(install.ID), string(TypeInfoFlow),
		binding.ChannelChatID, binding.ChatType, msgID, senderID, string(binding.Config)); err != nil {
		t.Fatalf("insert channel_task_delivery: %v", err)
	}

	bus := events.New()
	outbound := NewOutbound(queries, box.Open, NewClient(srv.Client(), srv.URL), nil)
	outbound.Register(bus)
	bus.Publish(events.Event{
		Type:          protocol.EventChatDone,
		TaskID:        taskID,
		ChatSessionID: util.UUIDToString(sessionID),
		Payload: protocol.ChatDonePayload{
			TaskID:        taskID,
			ChatSessionID: util.UUIDToString(sessionID),
			Content:       "reply from agent",
		},
	})
	// The outbound path also consults TaskInputIsChannelIngested; the task has no
	// chat_input_task_id, so it proceeds. Bus delivery is synchronous, so the
	// send has already happened by the time Publish returns.
	if sent := rec.sent(); len(sent) != 1 || sent[0] != "/app/message/send" {
		t.Fatalf("outbound sends = %v, want exactly one /app/message/send", sent)
	}

	// W2 self-service release: revoke, then re-install the SAME application id
	// for the other agent. Without the reclaim this fails on the app_id slot.
	if err := svc.Revoke(ctx, install.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	reinstalled, err := svc.RegisterBYO(ctx, RegisterBYOParams{
		WorkspaceID:   util.MustParseUUID(fx.WorkspaceID),
		AgentID:       util.MustParseUUID(agentBID),
		InitiatorID:   util.MustParseUUID(fx.UserID),
		ApplicationID: appID,
		AppKey:        "key-" + suffix,
		AppSecret:     "secret-" + suffix,
	})
	if err != nil {
		t.Fatalf("re-install after revoke must self-release the app_id slot (W2): %v", err)
	}
	if reinstalled.Status != "active" {
		t.Fatalf("reinstalled status = %q", reinstalled.Status)
	}
}
