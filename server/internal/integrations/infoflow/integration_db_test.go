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
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
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

// The Router applies the agent invocation policy before scheduling a run
// (added upstream after this adapter was written); this stub admits everyone so
// the persistence path under test is exercised.
func (infoflowTestTasks) MemberMayInvokeAgent(context.Context, pgtype.UUID, pgtype.UUID) (bool, error) {
	return true, nil
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

func (r *infoflowSendRecorder) sentPathsAndBodies() ([]string, []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.paths...), append([]string(nil), r.bodies...)
}

// numericSuffix extracts the leading decimal digits of a UUID-derived suffix so
// a synthesized group id passes the group-send numeric validation.
func numericSuffix(suffix string) string {
	out := make([]byte, 0, len(suffix))
	for i := 0; i < len(suffix); i++ {
		c := suffix[i]
		if c < '0' || c > '9' {
			break
		}
		out = append(out, c)
	}
	if len(out) == 0 {
		return "0"
	}
	return string(out)
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
	router.Register(TypeInfoFlow, NewResolverSet(queries, pool, nil, nil))

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

// TestInfoFlowDBUnaddressedGroupMessageIsAuditedAndDeduped locks the N17 fix:
// an unaddressed group message must still reach the engine, which writes the
// channel_inbound_audit row (drop_reason=not_addressed_in_group) and runs the
// dedup gate, so a platform redelivery is recorded as duplicate.
func TestInfoFlowDBUnaddressedGroupMessageIsAuditedAndDeduped(t *testing.T) {
	pool := infoflowDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	queries := db.New(pool)
	fx := dbfx.New(pool, "", "")
	suffix := util.UUIDToString(dbid.NewV7())
	fx.UserID = fx.User(t, "Owner", "infoflow-n17-"+suffix+"@multica.test")
	fx.WorkspaceID = fx.Workspace(t, "InfoFlow N17", "infoflow-n17-"+suffix)
	fx.Member(t, fx.WorkspaceID, fx.UserID, "owner")
	runtimeID := fx.Runtime(t, "Runtime")
	agentID := fx.Agent(t, "Agent", runtimeID)
	fx.Cleanup(t, "DELETE FROM channel_installation WHERE workspace_id = $1", fx.WorkspaceID)

	appID := "app-n17-" + suffix
	srv, _ := infoflowTokenServer(t)
	box, err := secretbox.New(bytes.Repeat([]byte{0x6B}, secretbox.KeySize))
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
	// No channel_user_binding row: the engine must still process the message.
	router := engine.NewRouter(infoflowTestIssues{}, infoflowTestTasks{}, queries, engine.RouterConfig{})
	router.Register(TypeInfoFlow, NewResolverSet(queries, pool, nil, nil))

	groupID := "1257" + suffix[:6]
	msgID := "msg-n17-" + suffix
	inbound := channel.InboundMessage{
		EventID:   msgID,
		MessageID: msgID,
		Type:      channel.MsgTypeText,
		Text:      "group chatter, nobody addressed",
		Source: channel.Source{
			ChannelType: TypeInfoFlow,
			ChatID:      groupID,
			ChatType:    channel.ChatTypeGroup,
			SenderID:    "user-" + suffix,
		},
		AddressedToBot: false,
		Raw:            json.RawMessage(`{"agentid":"` + appID + `","groupid":"` + groupID + `"}`),
	}
	if err := router.Handle(ctx, inbound); err != nil {
		t.Fatalf("router.Handle: %v", err)
	}

	// ① the audit row for the ENGINE's drop exists.
	deadline := time.Now().Add(10 * time.Second)
	var dropReason string
	for {
		err := pool.QueryRow(ctx, `
SELECT drop_reason FROM channel_inbound_audit
WHERE installation_id = $1 AND channel_message_id = $2
ORDER BY received_at DESC LIMIT 1`, install.ID, msgID).Scan(&dropReason)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no channel_inbound_audit row written for the unaddressed group message (B1 regression): %v", err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if dropReason != "not_addressed_in_group" {
		t.Fatalf("drop_reason = %q, want the engine enum not_addressed_in_group", dropReason)
	}

	// ② the dedup gate ran: a redelivery of the same message id is a duplicate.
	if err := router.Handle(ctx, inbound); err != nil {
		t.Fatalf("second Handle (redelivery) must be a no-op drop, got %v", err)
	}
	var duplicateAudits int
	if err := pool.QueryRow(ctx, `
SELECT count(*) FROM channel_inbound_audit
WHERE installation_id = $1 AND channel_message_id = $2 AND drop_reason = 'duplicate'`,
		install.ID, msgID).Scan(&duplicateAudits); err != nil {
		t.Fatalf("count duplicate audits: %v", err)
	}
	if duplicateAudits == 0 {
		t.Fatal("redelivery was not recorded as duplicate — the dedup gate was bypassed")
	}

	// And no agent task was enqueued for the unaddressed message.
	var tasks int
	if err := pool.QueryRow(ctx, `
SELECT count(*) FROM agent_task_queue WHERE agent_id = $1`, util.MustParseUUID(agentID)).Scan(&tasks); err != nil {
		t.Fatalf("count tasks: %v", err)
	}
	if tasks != 0 {
		t.Fatalf("unaddressed group message must not enqueue a task, got %d", tasks)
	}
}

// TestInfoFlowDBEngineDropLogIsSearchable proves N17 criterion ③: with the
// engine's logger at debug level, the router's drop line carries
// drop_reason=not_addressed_in_group together with the channel coordinates.
// The engine is unchanged; this only observes its documented log.
func TestInfoFlowDBEngineDropLogIsSearchable(t *testing.T) {
	pool := infoflowDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	queries := db.New(pool)
	fx := dbfx.New(pool, "", "")
	suffix := util.UUIDToString(dbid.NewV7())
	fx.UserID = fx.User(t, "Owner", "infoflow-n17log-"+suffix+"@multica.test")
	fx.WorkspaceID = fx.Workspace(t, "InfoFlow N17 log", "infoflow-n17log-"+suffix)
	fx.Member(t, fx.WorkspaceID, fx.UserID, "owner")
	runtimeID := fx.Runtime(t, "Runtime")
	agentID := fx.Agent(t, "Agent", runtimeID)
	fx.Cleanup(t, "DELETE FROM channel_installation WHERE workspace_id = $1", fx.WorkspaceID)

	appID := "app-n17log-" + suffix
	srv, _ := infoflowTokenServer(t)
	box, err := secretbox.New(bytes.Repeat([]byte{0x7C}, secretbox.KeySize))
	if err != nil {
		t.Fatal(err)
	}
	svc, err := NewInstallService(queries, pool, box, nil)
	if err != nil {
		t.Fatal(err)
	}
	svc.http = srv.Client()
	svc.apiBase = srv.URL
	if _, err := svc.RegisterBYO(ctx, RegisterBYOParams{
		WorkspaceID:   util.MustParseUUID(fx.WorkspaceID),
		AgentID:       util.MustParseUUID(agentID),
		InitiatorID:   util.MustParseUUID(fx.UserID),
		ApplicationID: appID,
		AppKey:        "key-" + suffix,
		AppSecret:     "secret-" + suffix,
		BotUserID:     "bot-user",
	}); err != nil {
		t.Fatalf("RegisterBYO: %v", err)
	}

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	router := engine.NewRouter(infoflowTestIssues{}, infoflowTestTasks{}, queries, engine.RouterConfig{Logger: logger})
	router.Register(TypeInfoFlow, NewResolverSet(queries, pool, nil, logger))

	groupID := "1257" + suffix[:6]
	msgID := "msg-n17log-" + suffix
	if err := router.Handle(ctx, channel.InboundMessage{
		EventID:        msgID,
		MessageID:      msgID,
		Type:           channel.MsgTypeText,
		Text:           "nobody addressed the bot",
		AddressedToBot: false,
		Source: channel.Source{
			ChannelType: TypeInfoFlow, ChatID: groupID, ChatType: channel.ChatTypeGroup, SenderID: "user-" + suffix,
		},
		Raw: json.RawMessage(`{"agentid":"` + appID + `","groupid":"` + groupID + `"}`),
	}); err != nil {
		t.Fatalf("router.Handle: %v", err)
	}

	logged := buf.String()
	if !strings.Contains(logged, "channel router: dispatch outcome") {
		t.Fatalf("engine drop log line missing:\n%s", logged)
	}
	for _, want := range []string{"drop_reason=not_addressed_in_group", "channel_type=infoflow", "event_id=" + msgID} {
		if !strings.Contains(logged, want) {
			t.Fatalf("engine drop log missing %q:\n%s", want, logged)
		}
	}
	// The routing coordinates live on the audit row this drop wrote.
	var auditChat string
	if err := pool.QueryRow(ctx, `
SELECT channel_chat_id FROM channel_inbound_audit
WHERE channel_type='infoflow' AND channel_message_id=$1 AND drop_reason='not_addressed_in_group'`, msgID).Scan(&auditChat); err != nil {
		t.Fatalf("audit row with chat id missing: %v", err)
	}
	if auditChat != groupID {
		t.Fatalf("audit chat id = %q, want %q", auditChat, groupID)
	}
}

// TestInfoFlowDBGroupAddressingGate is the A10 evidence at the Router+DB level:
// a real InfoFlow group payload whose body @s the bot must be ingested and get
// its own session keyed by the GROUP id, while an unaddressed one (including an
// ALL_MESSAGE_FORWARD subscribed message) must be dropped as
// not_addressed_in_group and create no session.
func TestInfoFlowDBGroupAddressingGate(t *testing.T) {
	pool := infoflowDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	queries := db.New(pool)
	fx := dbfx.New(pool, "", "")
	suffix := util.UUIDToString(dbid.NewV7())
	fx.UserID = fx.User(t, "Owner", "infoflow-n36-"+suffix+"@multica.test")
	fx.WorkspaceID = fx.Workspace(t, "InfoFlow N36", "infoflow-n36-"+suffix)
	fx.Member(t, fx.WorkspaceID, fx.UserID, "owner")
	runtimeID := fx.Runtime(t, "Runtime")
	agentID := fx.Agent(t, "Agent", runtimeID)
	fx.Cleanup(t, "DELETE FROM channel_installation WHERE workspace_id = $1", fx.WorkspaceID)

	appID := "app-n36-" + suffix
	const botUserID = "4105001274"
	srv, _ := infoflowTokenServer(t)
	box, err := secretbox.New(bytes.Repeat([]byte{0x4E}, secretbox.KeySize))
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
		BotUserID:     botUserID,
	})
	if err != nil {
		t.Fatalf("RegisterBYO: %v", err)
	}

	router := engine.NewRouter(infoflowTestIssues{}, infoflowTestTasks{}, queries, engine.RouterConfig{})
	router.Register(TypeInfoFlow, NewResolverSet(queries, pool, nil, nil))

	groupID := "1257" + suffix[:6]
	otherGroupID := "1258" + suffix[:6]
	sender := "user-" + suffix
	// N42: group sessions are isolated per (group, sender); the binding key is
	// the composite routing key, not the bare group id.
	groupKey := func(group string) string { return group + groupRoutingSeparator + sender }
	// deliver goes through the REAL adapter normalization (the same function the
	// receive loop calls) so AddressedToBot is produced by production code
	// rather than hand-set by the test.
	deliver := func(msgID, group, body, eventType string) {
		t.Helper()
		raw := json.RawMessage(`{"agentid":"` + appID + `","eventtype":"` + eventType + `","groupid":"` + group +
			`","message":{"header":{"fromuserid":"` + sender + `","toid":"` + group +
			`","totype":"GROUP","msgtype":"MIXED","messageid":"` + msgID + `"},"body":` + body + `}}`)
		msg, _, ok := inboundFromPayloadReason(raw, appID, botUserID)
		if !ok {
			t.Fatalf("adapter refused to normalize %s", msgID)
		}
		if msg.MessageID == "" {
			msg.MessageID = msgID
		}
		if err := router.Handle(ctx, msg); err != nil {
			t.Fatalf("router.Handle(%s): %v", msgID, err)
		}
	}

	// 1) @bot via the documented atuserids shape → must be ingested.
	addressedID := "msg-n36-at-" + suffix
	deliver(addressedID, groupID,
		`[{"type":"AT","atuserids":["`+botUserID+`"]},{"type":"TEXT","content":"@bot please help"}]`,
		"MESSAGE_RECEIVE")

	var chatID string
	if err := pool.QueryRow(ctx, `
SELECT cs.id FROM chat_session cs
JOIN channel_chat_session_binding b ON b.chat_session_id = cs.id
WHERE b.installation_id = $1 AND b.channel_chat_id = $2`, install.ID, groupKey(groupID)).Scan(&chatID); err != nil {
		t.Fatalf("addressed group message did not create a session keyed by the group id: %v", err)
	}
	var addressedDrops int
	if err := pool.QueryRow(ctx, `
SELECT count(*) FROM channel_inbound_audit
WHERE installation_id = $1 AND channel_message_id = $2 AND drop_reason = 'not_addressed_in_group'`,
		install.ID, addressedID).Scan(&addressedDrops); err != nil {
		t.Fatalf("count addressed drops: %v", err)
	}
	if addressedDrops != 0 {
		t.Fatalf("an @bot message was wrongly dropped as not_addressed_in_group (%d rows)", addressedDrops)
	}

	// 2) unaddressed plain group message → dropped, no session for that group.
	plainID := "msg-n36-plain-" + suffix
	deliver(plainID, otherGroupID, `[{"type":"TEXT","content":"group chatter"}]`, "MESSAGE_RECEIVE")
	assertDropped := func(msgID, wantGroup string) {
		t.Helper()
		var reason string
		deadline := time.Now().Add(10 * time.Second)
		for {
			err := pool.QueryRow(ctx, `
SELECT drop_reason FROM channel_inbound_audit
WHERE installation_id = $1 AND channel_message_id = $2
ORDER BY received_at DESC LIMIT 1`, install.ID, msgID).Scan(&reason)
			if err == nil {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("no audit row for %s: %v", msgID, err)
			}
			time.Sleep(50 * time.Millisecond)
		}
		if reason != "not_addressed_in_group" {
			t.Fatalf("%s drop_reason = %q, want not_addressed_in_group", msgID, reason)
		}
		var sessions int
		if err := pool.QueryRow(ctx, `
SELECT count(*) FROM channel_chat_session_binding
WHERE installation_id = $1 AND channel_chat_id = $2`, install.ID, groupKey(wantGroup)).Scan(&sessions); err != nil {
			t.Fatalf("count sessions: %v", err)
		}
		if sessions != 0 {
			t.Fatalf("%s must not create a session, found %d", msgID, sessions)
		}
	}
	assertDropped(plainID, otherGroupID)

	// 3) ALL_MESSAGE_FORWARD but unmentioned → still dropped (fail-closed).
	fwdID := "msg-n36-fwd-" + suffix
	deliver(fwdID, otherGroupID, `[{"type":"TEXT","content":"forwarded chatter"}]`, "ALL_MESSAGE_FORWARD")
	assertDropped(fwdID, otherGroupID)

	// 4) @all → addressed (broadcast).
	allID := "msg-n36-all-" + suffix
	allGroup := "1259" + suffix[:6]
	deliver(allID, allGroup, `[{"type":"AT","atall":true},{"type":"TEXT","content":"@all notice"}]`, "MESSAGE_RECEIVE")
	var allSessions int
	if err := pool.QueryRow(ctx, `
SELECT count(*) FROM channel_chat_session_binding
WHERE installation_id = $1 AND channel_chat_id = $2`, install.ID, groupKey(allGroup)).Scan(&allSessions); err != nil {
		t.Fatalf("count @all sessions: %v", err)
	}
	if allSessions != 1 {
		t.Fatalf("@all must be addressed and create exactly one session, got %d", allSessions)
	}
}

// TestInfoFlowDBDisplayNameGroupMentionIsIngested is the N40 closure: a real
// InfoFlow group @ carries only the robot's DISPLAY NAME in the AT element
// (imId is not sent), so an exact id match alone dropped every genuine mention.
// Here bot_user_id is deliberately WRONG (it holds a group id, the actual
// production misconfiguration) and the mention still has to be recognized via
// either the configured bot_mention_names or the group member lookup. It also
// asserts the diagnostic log's bot_user_id_configured is populated (the
// channel_type-less lookup bug printed "" for a configured installation).
func TestInfoFlowDBDisplayNameGroupMentionIsIngested(t *testing.T) {
	pool := infoflowDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	queries := db.New(pool)
	fx := dbfx.New(pool, "", "")
	suffix := util.UUIDToString(dbid.NewV7())
	fx.UserID = fx.User(t, "Owner", "infoflow-n40-"+suffix+"@multica.test")
	fx.WorkspaceID = fx.Workspace(t, "InfoFlow N40", "infoflow-n40-"+suffix)
	fx.Member(t, fx.WorkspaceID, fx.UserID, "owner")
	runtimeID := fx.Runtime(t, "Runtime")
	agentID := fx.Agent(t, "Agent", runtimeID)
	fx.Cleanup(t, "DELETE FROM channel_installation WHERE workspace_id = $1", fx.WorkspaceID)

	appID := "app-n40-" + suffix
	// Deliberately the GROUP id, mirroring the observed production mistake.
	const misconfiguredBotUserID = "12572761"
	const botDisplayName = "Multica"
	const botImID = "4100104388"

	// The token server also answers the group memberList lookup, whose entries
	// carry the bot's real imId/name (agentId == app id).
	mux := http.NewServeMux()
	mux.HandleFunc("/auth/app_access_token", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"code":"ok","data":{"app_access_token":"at_test","expire":7200}}`))
	})
	mux.HandleFunc(pathGroupMemberList, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"code":"ok","data":{"agentInfoList":[` +
			`{"agentId":"99999","imId":"1234567890","name":"other bot"},` +
			`{"agentId":"` + appID + `","imId":"` + botImID + `","name":"` + botDisplayName + `"}]}}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	box, err := secretbox.New(bytes.Repeat([]byte{0x4E}, secretbox.KeySize))
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
		WorkspaceID:     util.MustParseUUID(fx.WorkspaceID),
		AgentID:         util.MustParseUUID(agentID),
		InitiatorID:     util.MustParseUUID(fx.UserID),
		ApplicationID:   appID,
		AppKey:          "key-" + suffix,
		AppSecret:       "secret-" + suffix,
		BotUserID:       misconfiguredBotUserID,
		BotMentionNames: botDisplayName,
	})
	if err != nil {
		t.Fatalf("RegisterBYO: %v", err)
	}

	router := engine.NewRouter(infoflowTestIssues{}, infoflowTestTasks{}, queries, engine.RouterConfig{})
	router.Register(TypeInfoFlow, NewResolverSet(queries, pool, nil, nil))

	// Deliver through the REAL adapter path, including the group identity
	// resolver wired the way the channel wires it.
	groupResolver := newGroupIdentityResolver(NewClient(srv.Client(), srv.URL), "key-"+suffix, "secret-"+suffix, appID)
	groupID := "1257" + suffix[:6]
	sender := "user-" + suffix
	msgID := "msg-n40-name-" + suffix
	body := `[{"type":"AT","name":"` + botDisplayName + `"},{"type":"TEXT","content":"@bot hello"}]`
	raw := json.RawMessage(`{"agentid":"` + appID + `","eventtype":"MESSAGE_RECEIVE","groupid":"` + groupID +
		`","message":{"header":{"fromuserid":"` + sender + `","toid":"` + groupID +
		`","totype":"GROUP","msgtype":"MIXED","messageid":"` + msgID + `"},"body":` + body + `}}`)

	self := newIdentitySet(misconfiguredBotUserID, []string{botDisplayName})
	if resolved, _ := groupResolver.lookup(ctx, groupID); len(resolved) > 0 {
		self = self.withResolved(resolved)
	}
	msg, _, ok := inboundFromPayloadReasonWithIdentity(raw, appID, self)
	if !ok {
		t.Fatalf("adapter refused to normalize %s", msgID)
	}
	if !msg.AddressedToBot {
		t.Fatal("a display-name @ must be judged addressed-to-bot")
	}
	if msg.MessageID == "" {
		msg.MessageID = msgID
	}
	if err := router.Handle(ctx, msg); err != nil {
		t.Fatalf("router.Handle: %v", err)
	}

	// The mention reached the engine: a session keyed by the group id exists and
	// no not_addressed_in_group audit row was written.
	var sessions int
	if err := pool.QueryRow(ctx, `
SELECT count(*) FROM channel_chat_session_binding
WHERE installation_id = $1 AND channel_chat_id = $2`, install.ID, groupID+groupRoutingSeparator+sender).Scan(&sessions); err != nil {
		t.Fatalf("count sessions: %v", err)
	}
	if sessions != 1 {
		t.Fatalf("display-name @ must create exactly one (group,sender) session, got %d", sessions)
	}
	var drops int
	if err := pool.QueryRow(ctx, `
SELECT count(*) FROM channel_inbound_audit
WHERE installation_id = $1 AND channel_message_id = $2 AND drop_reason = 'not_addressed_in_group'`,
		install.ID, msgID).Scan(&drops); err != nil {
		t.Fatalf("count drops: %v", err)
	}
	if drops != 0 {
		t.Fatalf("display-name @ was wrongly dropped as not_addressed_in_group (%d rows)", drops)
	}

	// Diagnostics: configuredBotIdentity must read the row (the N40 bug dropped
	// channel_type from the query, so it always returned "").
	audit := &auditor{q: queries, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	botUserID, names, read := audit.configuredBotIdentity(ctx, install.ID)
	if !read {
		t.Fatal("configuredBotIdentity must succeed for a real installation")
	}
	if botUserID != misconfiguredBotUserID {
		t.Fatalf("bot_user_id = %q, want %q", botUserID, misconfiguredBotUserID)
	}
	if len(names) != 1 || names[0] != botDisplayName {
		t.Fatalf("bot_mention_names = %v, want [%s]", names, botDisplayName)
	}
}

// TestInfoFlowDBGroupPerSenderIsolationAndDirectedReply is the A12/A13 closure:
// two people @ the bot in the SAME group must land in two different
// chat_sessions (group isolated per sender), and each outbound reply must carry
// an AT element naming its own asker while addressing the real group id.
func TestInfoFlowDBGroupPerSenderIsolationAndDirectedReply(t *testing.T) {
	pool := infoflowDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	queries := db.New(pool)
	fx := dbfx.New(pool, "", "")
	suffix := util.UUIDToString(dbid.NewV7())
	fx.UserID = fx.User(t, "Owner", "infoflow-n42-"+suffix+"@multica.test")
	fx.WorkspaceID = fx.Workspace(t, "InfoFlow N42", "infoflow-n42-"+suffix)
	fx.Member(t, fx.WorkspaceID, fx.UserID, "owner")
	runtimeID := fx.Runtime(t, "Runtime")
	agentID := fx.Agent(t, "Agent", runtimeID)
	fx.Cleanup(t, "DELETE FROM channel_installation WHERE workspace_id = $1", fx.WorkspaceID)

	// Numeric: the group send body validates both toid and agentid as numbers,
	// and InstallService uses the application id as the agent id.
	appID := "145" + numericSuffix(suffix)
	const botUserID = "4105001274"
	srv, rec := infoflowTokenServer(t)
	box, err := secretbox.New(bytes.Repeat([]byte{0x4E}, secretbox.KeySize))
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
		BotUserID:     botUserID,
	})
	if err != nil {
		t.Fatalf("RegisterBYO: %v", err)
	}

	router := engine.NewRouter(infoflowTestIssues{}, infoflowTestTasks{}, queries, engine.RouterConfig{})
	router.Register(TypeInfoFlow, NewResolverSet(queries, pool, nil, nil))

	groupID := "1257" + numericSuffix(suffix)
	alice := "alice-" + suffix
	bob := "bob-" + suffix
	deliver := func(sender, msgID string) {
		t.Helper()
		raw := json.RawMessage(`{"agentid":"` + appID + `","eventtype":"MESSAGE_RECEIVE","groupid":"` + groupID +
			`","message":{"header":{"fromuserid":"` + sender + `","toid":"` + groupID +
			`","totype":"GROUP","msgtype":"MIXED","messageid":"` + msgID + `"},"body":[{"type":"AT","atuserids":["` + botUserID + `"]},{"type":"TEXT","content":"@bot help"}]}}`)
		msg, _, ok := inboundFromPayloadReason(raw, appID, botUserID)
		if !ok {
			t.Fatalf("adapter refused to normalize %s", msgID)
		}
		if !msg.AddressedToBot {
			t.Fatalf("%s must be addressed to the bot", msgID)
		}
		if msg.MessageID == "" {
			msg.MessageID = msgID
		}
		if err := router.Handle(ctx, msg); err != nil {
			t.Fatalf("router.Handle(%s): %v", msgID, err)
		}
	}
	deliver(alice, "n42-a-"+suffix)
	deliver(bob, "n42-b-"+suffix)

	// Two senders in one group ⇒ two distinct sessions, keyed by group:sender.
	var keys []string
	rows, err := pool.Query(ctx, `
SELECT channel_chat_id FROM channel_chat_session_binding
WHERE installation_id = $1 AND chat_type = 'group' ORDER BY channel_chat_id`, install.ID)
	if err != nil {
		t.Fatalf("read group keys: %v", err)
	}
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			t.Fatal(err)
		}
		keys = append(keys, key)
	}
	rows.Close()
	wantKeys := []string{groupID + ":" + alice, groupID + ":" + bob}
	sort.Strings(wantKeys)
	if len(keys) != 2 || keys[0] != wantKeys[0] || keys[1] != wantKeys[1] {
		t.Fatalf("group session keys = %v, want %v (per-sender isolation)", keys, wantKeys)
	}

	// Directed outbound: load each binding, drive the real outbound path, and
	// assert the group body @s that binding's own sender while toid is the group.
	for _, tc := range []struct{ sender, msgID string }{{alice, "n42-a-" + suffix}, {bob, "n42-b-" + suffix}} {
		var binding db.ChannelChatSessionBinding
		var scID pgtype.UUID
		if err := pool.QueryRow(ctx, `
SELECT id, chat_session_id, installation_id, channel_type, channel_chat_id, chat_type, config
FROM channel_chat_session_binding
WHERE installation_id = $1 AND channel_chat_id = $2`, install.ID, groupID+":"+tc.sender).Scan(
			&binding.ID, &scID, &binding.InstallationID, &binding.ChannelType,
			&binding.ChannelChatID, &binding.ChatType, &binding.Config); err != nil {
			t.Fatalf("load binding for %s: %v", tc.sender, err)
		}
		taskID := fx.Task(t, agentID, dbfx.Cols{
			"status":          "completed",
			"chat_session_id": util.UUIDToString(scID),
			"runtime_id":      runtimeID,
		})
		if _, err := pool.Exec(ctx, `
INSERT INTO channel_task_delivery (
    task_id, binding_id, installation_id, channel_type, channel_chat_id, chat_type,
    channel_message_id, channel_sender_id, route_revision, config
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 0, $9)`,
			taskID, util.UUIDToString(binding.ID), util.UUIDToString(install.ID), string(TypeInfoFlow),
			binding.ChannelChatID, binding.ChatType, tc.msgID, tc.sender, string(binding.Config)); err != nil {
			t.Fatalf("insert delivery: %v", err)
		}
		bus := events.New()
		outbound := NewOutbound(queries, box.Open, NewClient(srv.Client(), srv.URL), nil)
		outbound.Register(bus)
		bus.Publish(events.Event{
			Type:          protocol.EventChatDone,
			TaskID:        taskID,
			ChatSessionID: util.UUIDToString(scID),
			Payload: protocol.ChatDonePayload{
				TaskID:        taskID,
				ChatSessionID: util.UUIDToString(scID),
				Content:       "reply to " + tc.sender,
			},
		})
	}

	// Every group send went to groupmsgsend and carried its own asker's AT.
	paths, bodies := rec.sentPathsAndBodies()
	if len(paths) != 2 {
		t.Fatalf("outbound sends = %v, want 2 group sends", paths)
	}
	for i, path := range paths {
		if path != "/robot/msg/groupmsgsend" {
			t.Fatalf("send %d path = %q, want groupmsgsend", i, path)
		}
	}
	aliceBody, bobBody := "", ""
	for _, b := range bodies {
		switch {
		case strings.Contains(b, alice):
			aliceBody = b
		case strings.Contains(b, bob):
			bobBody = b
		}
	}
	if aliceBody == "" || bobBody == "" {
		t.Fatalf("each outbound group body must @ its own asker; bodies=%v", bodies)
	}
	for sender, b := range map[string]string{alice: aliceBody, bob: bobBody} {
		var parsed map[string]any
		if err := json.Unmarshal([]byte(b), &parsed); err != nil {
			t.Fatalf("decode send body: %v", err)
		}
		message := parsed["message"].(map[string]any)
		header := message["header"].(map[string]any)
		if fmt.Sprint(header["toid"]) != groupID {
			t.Fatalf("%s reply toid = %v, want the real group id %s", sender, header["toid"], groupID)
		}
		elems := message["body"].([]any)
		if len(elems) != 2 {
			t.Fatalf("%s reply body = %+v, want AT + TEXT", sender, elems)
		}
		at := elems[0].(map[string]any)
		if at["type"] != "AT" {
			t.Fatalf("%s reply first element must be AT, got %+v", sender, at)
		}
		ids := at["atuserids"].([]any)
		if len(ids) != 1 || fmt.Sprint(ids[0]) != sender {
			t.Fatalf("%s reply atuserids = %v, want [%s]", sender, ids, sender)
		}
	}
}

// TestInfoFlowDBGroupReplyChunksAndFallsBack is the N48 closure: a long agent
// reply is split into ordered batches that all reach the group endpoint (with
// the AT mention on the first batch only), and a send failure produces a
// user-visible fallback notice instead of silence.
func TestInfoFlowDBGroupReplyChunksAndFallsBack(t *testing.T) {
	pool := infoflowDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	queries := db.New(pool)
	fx := dbfx.New(pool, "", "")
	suffix := util.UUIDToString(dbid.NewV7())
	fx.UserID = fx.User(t, "Owner", "infoflow-n48-"+suffix+"@multica.test")
	fx.WorkspaceID = fx.Workspace(t, "InfoFlow N48", "infoflow-n48-"+suffix)
	fx.Member(t, fx.WorkspaceID, fx.UserID, "owner")
	runtimeID := fx.Runtime(t, "Runtime")
	agentID := fx.Agent(t, "Agent", runtimeID)
	fx.Cleanup(t, "DELETE FROM channel_installation WHERE workspace_id = $1", fx.WorkspaceID)

	appID := "145" + numericSuffix(suffix)
	const botUserID = "4105001274"

	// Fake gateway: tokens, memberList, and group send that FAILS the first N
	// sends with 40061 and records every body.
	var mu sync.Mutex
	var bodies []string
	failFirst := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/auth/app_access_token":
			_, _ = w.Write([]byte(`{"code":"ok","data":{"app_access_token":"at_n48","expire":7200}}`))
		case pathGroupMemberList:
			_, _ = w.Write([]byte(`{"code":"ok","data":{"data":{"agentInfoList":[]}}}`))
		case pathSendGroup:
			buf, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
			mu.Lock()
			bodies = append(bodies, string(buf))
			fail := len(bodies) <= failFirst
			mu.Unlock()
			if fail {
				_, _ = w.Write([]byte(`{"code":"ok","data":{"errcode":40061,"errmsg":"message text length exceed limit"}}`))
				return
			}
			_, _ = w.Write([]byte(`{"code":"ok","data":{"data":{"messageid":"m48"}}}`))
		default:
			_, _ = w.Write([]byte(`{"code":"ok","data":{}}`))
		}
	}))
	defer srv.Close()

	box, err := secretbox.New(bytes.Repeat([]byte{0x4E}, secretbox.KeySize))
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
		BotUserID:     botUserID,
	})
	if err != nil {
		t.Fatalf("RegisterBYO: %v", err)
	}

	groupID := "1257" + numericSuffix(suffix)
	sender := "alice-" + suffix
	bindingID := util.UUIDToString(dbid.NewV7())
	sessionID := util.UUIDToString(dbid.NewV7())
	cfg := `{"chat_type":"group","chat_id":"` + groupID + `","sender_id":"` + sender + `","mention_user_id":"` + sender + `"}`
	// A minimal chat_session + binding so the session id is valid for the event.
	if _, err := pool.Exec(ctx, `INSERT INTO chat_session (id, workspace_id, agent_id, title, creator_id)
VALUES ($1, $2, $3, 'n48', $4)`, sessionID, fx.WorkspaceID, agentID, fx.UserID); err != nil {
		t.Fatalf("insert chat_session: %v", err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO channel_chat_session_binding (id, chat_session_id, installation_id, channel_type, channel_chat_id, chat_type, config)
VALUES ($1, $2, $3, 'infoflow', $4, 'group', $5)`,
		bindingID, sessionID, util.UUIDToString(install.ID), groupID+":"+sender, cfg); err != nil {
		t.Fatalf("insert binding: %v", err)
	}

	runOutbound := func(content string) error {
		taskID := fx.Task(t, agentID, dbfx.Cols{
			"status":          "completed",
			"chat_session_id": sessionID,
			"runtime_id":      runtimeID,
		})
		if _, err := pool.Exec(ctx, `
INSERT INTO channel_task_delivery (task_id, binding_id, installation_id, channel_type, channel_chat_id, chat_type,
  channel_message_id, channel_sender_id, route_revision, config)
VALUES ($1,$2,$3,'infoflow',$4,'group','m48',$5,0,$6)`,
			taskID, bindingID, util.UUIDToString(install.ID), groupID+":"+sender, sender, cfg); err != nil {
			t.Fatalf("insert delivery: %v", err)
		}
		bus := events.New()
		outbound := NewOutbound(queries, box.Open, NewClient(srv.Client(), srv.URL), nil).WithAppURL("https://app.example")
		outbound.Register(bus)
		bus.Publish(events.Event{
			Type:          protocol.EventChatDone,
			TaskID:        taskID,
			ChatSessionID: sessionID,
			Payload:       protocol.ChatDonePayload{TaskID: taskID, ChatSessionID: sessionID, Content: content},
		})
		return nil
	}

	// Long reply → multiple ordered batches, AT on batch 0 only.
	long := strings.Repeat("段落line内容 ", 400) // > 1500 runes
	_ = runOutbound(long)
	mu.Lock()
	longBodies := append([]string(nil), bodies...)
	mu.Unlock()
	if len(longBodies) < 2 {
		t.Fatalf("long reply produced %d send(s), want >= 2 batches", len(longBodies))
	}
	var rebuilt strings.Builder
	for i, b := range longBodies {
		var parsed map[string]any
		if err := json.Unmarshal([]byte(b), &parsed); err != nil {
			t.Fatalf("decode body %d: %v", i, err)
		}
		msg := parsed["message"].(map[string]any)
		header := msg["header"].(map[string]any)
		if fmt.Sprint(header["toid"]) != groupID {
			t.Fatalf("batch %d toid = %v, want the real group id %s", i, header["toid"], groupID)
		}
		for _, e := range msg["body"].([]any) {
			el := e.(map[string]any)
			if el["type"] == "AT" && i != 0 {
				t.Fatalf("batch %d carries an AT; the mention must ride only the first batch", i)
			}
			if el["type"] == "TEXT" {
				rebuilt.WriteString(el["content"].(string))
			}
		}
	}
	if got := rebuilt.String(); got != long {
		t.Fatalf("reassembled text differs from the original")
	}

	// Now make the NEXT primary send fail once, and assert a visible fallback
	// (a second send whose body is plain text) is delivered.
	mu.Lock()
	bodies = nil
	failFirst = 1
	mu.Unlock()
	_ = runOutbound("short reply that will fail once")
	mu.Lock()
	failBodies := append([]string(nil), bodies...)
	mu.Unlock()
	if len(failBodies) != 2 {
		t.Fatalf("failed send must be followed by exactly one fallback, got %d sends", len(failBodies))
	}
	var fallback map[string]any
	if err := json.Unmarshal([]byte(failBodies[1]), &fallback); err != nil {
		t.Fatalf("decode fallback: %v", err)
	}
	fmsg := fallback["message"].(map[string]any)
	sawNotice := false
	for _, e := range fmsg["body"].([]any) {
		el := e.(map[string]any)
		if el["type"] == "AT" {
			t.Fatal("the fallback must not carry an AT element")
		}
		if el["type"] == "TEXT" && strings.Contains(el["content"].(string), "Multica") {
			sawNotice = true
		}
	}
	if !sawNotice {
		t.Fatalf("fallback body = %s, want a user-visible Multica notice", failBodies[1])
	}
}

// TestInfoFlowDBSingleChatSessionIsolation is the A10 single-chat half: two
// different InfoFlow users talking to the same bot must get two separate
// sessions (keyed by their own user id), so one person never reads another's
// conversation.
func TestInfoFlowDBSingleChatSessionIsolation(t *testing.T) {
	pool := infoflowDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	queries := db.New(pool)
	fx := dbfx.New(pool, "", "")
	suffix := util.UUIDToString(dbid.NewV7())
	fx.UserID = fx.User(t, "Owner", "infoflow-n36dm-"+suffix+"@multica.test")
	fx.WorkspaceID = fx.Workspace(t, "InfoFlow N36 DM", "infoflow-n36dm-"+suffix)
	fx.Member(t, fx.WorkspaceID, fx.UserID, "owner")
	runtimeID := fx.Runtime(t, "Runtime")
	agentID := fx.Agent(t, "Agent", runtimeID)
	fx.Cleanup(t, "DELETE FROM channel_installation WHERE workspace_id = $1", fx.WorkspaceID)

	appID := "app-n36dm-" + suffix
	srv, _ := infoflowTokenServer(t)
	box, err := secretbox.New(bytes.Repeat([]byte{0x2D}, secretbox.KeySize))
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
		BotUserID:     "4105001274",
	})
	if err != nil {
		t.Fatalf("RegisterBYO: %v", err)
	}
	router := engine.NewRouter(infoflowTestIssues{}, infoflowTestTasks{}, queries, engine.RouterConfig{})
	router.Register(TypeInfoFlow, NewResolverSet(queries, pool, nil, nil))

	send := func(user, msgID string) {
		t.Helper()
		raw := json.RawMessage(`{"agentid":"` + appID + `","FromUserId":"` + user +
			`","FromUserName":"` + user + `","Content":"hello","MsgId":"` + msgID + `","MsgType":"text"}`)
		msg, _, ok := inboundFromPayloadReason(raw, appID, "4105001274")
		if !ok {
			t.Fatalf("adapter refused single-chat message from %s", user)
		}
		if err := router.Handle(ctx, msg); err != nil {
			t.Fatalf("router.Handle(%s): %v", msgID, err)
		}
	}
	send("alice-"+suffix, "dm-a-"+suffix)
	send("bob-"+suffix, "dm-b-"+suffix)

	var n int
	if err := pool.QueryRow(ctx, `
SELECT count(*) FROM channel_chat_session_binding
WHERE installation_id = $1 AND chat_type = 'p2p'`, install.ID).Scan(&n); err != nil {
		t.Fatalf("count p2p bindings: %v", err)
	}
	if n != 2 {
		t.Fatalf("two single-chat users must get two sessions, got %d", n)
	}
	var aliceKey, bobKey string
	if err := pool.QueryRow(ctx, `
SELECT string_agg(channel_chat_id, ',' ORDER BY channel_chat_id) FROM channel_chat_session_binding
WHERE installation_id = $1 AND chat_type = 'p2p'`, install.ID).Scan(&aliceKey); err != nil {
		t.Fatalf("read p2p keys: %v", err)
	}
	aliceKey = strings.TrimSpace(aliceKey)
	bobKey = "alice-" + suffix + ",bob-" + suffix
	if aliceKey != bobKey {
		t.Fatalf("p2p session keys = %q, want %q (each user keyed by their own id)", aliceKey, bobKey)
	}
}
