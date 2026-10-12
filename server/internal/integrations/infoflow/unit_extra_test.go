package infoflow

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/integrations/channel"
	"github.com/multica-ai/multica/server/internal/integrations/channel/engine"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

func TestDecodeCredentials(t *testing.T) {
	raw := json.RawMessage(`{"app_id":"14534","app_key":"key","app_secret_encrypted":"` + base64.StdEncoding.EncodeToString([]byte("secret")) + `","bot_user_id":"bot"}`)
	creds, err := decodeCredentials(raw, nil)
	if err != nil {
		t.Fatal(err)
	}
	if creds.AppID != "14534" || creds.AppKey != "key" || creds.AppSecret != "secret" || creds.BotUserID != "bot" {
		t.Fatalf("unexpected credentials: %+v", creds)
	}
	if _, err := decodeCredentials(nil, nil); err == nil {
		t.Fatal("expected empty config error")
	}
	if _, err := decodeCredentials(json.RawMessage(`{`), nil); err == nil {
		t.Fatal("expected malformed config error")
	}
	if _, err := decodeCredentials(json.RawMessage(`{"app_secret_encrypted":"!!!"}`), nil); err == nil {
		t.Fatal("expected base64 error")
	}
}

func TestDecryptTokenIdentity(t *testing.T) {
	enc := base64.StdEncoding.EncodeToString([]byte("plain"))
	got, err := decryptToken(enc, func(b []byte) ([]byte, error) { return b, nil })
	if err != nil || got != "plain" {
		t.Fatalf("got %q err %v", got, err)
	}
	if v, err := decryptToken("", nil); err != nil || v != "" {
		t.Fatalf("empty should be empty: %q %v", v, err)
	}
	if _, err := decryptToken(enc, func([]byte) ([]byte, error) { return nil, errors.New("boom") }); err == nil {
		t.Fatal("expected decrypt error")
	}
	if stripWhitespace("a b\nc\r\n") != "abc" {
		t.Fatal("stripWhitespace failed")
	}
}

func TestChannelIdentityAndCapabilities(t *testing.T) {
	ch := &infoflowChannel{}
	if ch.Type() != TypeInfoFlow || ch.Capabilities() != channel.CapText {
		t.Fatalf("unexpected identity: %v %v", ch.Type(), ch.Capabilities())
	}
	if err := ch.Disconnect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := ch.Connect(context.Background()); err == nil {
		t.Fatal("expected error without handler")
	}
	ch.handler = func(context.Context, channel.InboundMessage) error { return nil }
	if err := ch.Connect(context.Background()); err == nil {
		t.Fatal("expected error without credentials")
	}
}

func TestFactoryRegisterAndConfig(t *testing.T) {
	reg := channel.NewRegistry()
	Register(reg, ChannelDeps{})
	if _, ok := reg.Lookup(TypeInfoFlow); !ok {
		t.Fatal("expected registered factory")
	}
	if _, err := reg.Build(channel.Config{Type: TypeInfoFlow, Raw: json.RawMessage(`{`)}); err == nil {
		t.Fatal("expected build error on malformed config")
	}
	if _, err := reg.Build(channel.Config{Type: TypeInfoFlow, Raw: json.RawMessage(`{"app_id":"1"}`)}); err == nil {
		t.Fatal("expected build error on missing secret")
	}
	cfg, err := UnmarshalInstallationConfig(json.RawMessage(`{"app_id":"1","app_key":"k"}`))
	if err != nil || cfg.AppID != "1" || cfg.AppKey != "k" {
		t.Fatalf("unmarshal: %+v %v", cfg, err)
	}
	if _, err := UnmarshalInstallationConfig(json.RawMessage(`{`)); err == nil {
		t.Fatal("expected unmarshal error")
	}
}

func TestSendBodyValidation(t *testing.T) {
	if _, err := buildSendBody(infoflowSendTarget{ChatType: "p2p"}, "1", "hi"); err == nil {
		t.Fatal("expected missing recipient error")
	}
	if _, err := buildSendBody(infoflowSendTarget{ChatType: "group"}, "1", "hi"); err == nil {
		t.Fatal("expected missing group error")
	}
	if _, err := buildSendBody(infoflowSendTarget{ChatType: "group", ChatID: "abc"}, "1", "hi"); err == nil {
		t.Fatal("expected non-numeric group error")
	}
	body, err := buildSendBody(infoflowSendTarget{ChatType: "group", ChatID: "123"}, "14534", "hi")
	if err != nil {
		t.Fatal(err)
	}
	if body["message"] == nil {
		t.Fatalf("unexpected body: %+v", body)
	}
	if body["agentid"] != int64(14534) {
		t.Fatalf("group body missing top-level agentid: %+v", body["agentid"])
	}
	msg := body["message"].(map[string]any)
	header := msg["header"].(map[string]any)
	if _, ok := header["clientmsgid"]; !ok {
		t.Fatalf("group header missing clientmsgid: %+v", header)
	}
	if got := messageIDFromSendResponse(map[string]any{"messageid": 42}); got != "42" {
		t.Fatalf("message id: %q", got)
	}
	if got := messageIDFromSendResponse(map[string]any{"msgkey": "k"}); got != "k" {
		t.Fatalf("msgkey: %q", got)
	}
	if got := messageIDFromSendResponse(nil); got != "" {
		t.Fatalf("nil: %q", got)
	}
}

// N42: a group reply carries an AT element naming the asker ahead of the TEXT
// body; a group reply without a mention target stays a plain TEXT body, and the
// 1:1 path is unchanged (no AT).
func TestGroupSendBodyMentionsAsker(t *testing.T) {
	body, err := buildSendBody(infoflowSendTarget{ChatType: "group", ChatID: "12572761", MentionUserID: "alice"}, "14534", "here you go")
	if err != nil {
		t.Fatal(err)
	}
	msg := body["message"].(map[string]any)
	elements := msg["body"].([]map[string]any)
	if len(elements) != 2 {
		t.Fatalf("group body must be AT + TEXT, got %+v", elements)
	}
	at := elements[0]
	if at["type"] != "AT" || at["atall"] != false {
		t.Fatalf("first element must be a non-broadcast AT, got %+v", at)
	}
	ids, ok := at["atuserids"].([]string)
	if !ok || len(ids) != 1 || ids[0] != "alice" {
		t.Fatalf("atuserids = %+v, want [alice]", at["atuserids"])
	}
	if elements[1]["type"] != "TEXT" || elements[1]["content"] != "here you go" {
		t.Fatalf("second element must be the reply text, got %+v", elements[1])
	}

	// No mention target → plain TEXT only (legacy binding must not break).
	plain, err := buildSendBody(infoflowSendTarget{ChatType: "group", ChatID: "12572761"}, "14534", "hi")
	if err != nil {
		t.Fatal(err)
	}
	plainElements := plain["message"].(map[string]any)["body"].([]map[string]any)
	if len(plainElements) != 1 || plainElements[0]["type"] != "TEXT" {
		t.Fatalf("group body without a mention target must be TEXT only, got %+v", plainElements)
	}

	// 1:1 body never gains an AT, even if MentionUserID is set by mistake.
	p2p, err := buildSendBody(infoflowSendTarget{ChatType: "p2p", ChatID: "u1", SenderID: "u1", MentionUserID: "u1"}, "14534", "hi")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := p2p["message"]; ok {
		t.Fatalf("1:1 body must use the app send shape, got %+v", p2p)
	}
	if _, ok := p2p["text"]; !ok {
		t.Fatalf("1:1 body missing text: %+v", p2p)
	}
}

func TestSessionRoutingAndOutboundTarget(t *testing.T) {
	p2p := channel.InboundMessage{Source: channel.Source{ChatType: channel.ChatTypeP2P, ChatID: "u1", SenderID: "u1"}}
	key, cfgRaw := infoflowSessionRouting(p2p)
	if key != "u1" {
		t.Fatalf("binding key: %q", key)
	}
	var cfg infoflowBindingConfig
	if err := json.Unmarshal(cfgRaw, &cfg); err != nil || cfg.SenderID != "u1" {
		t.Fatalf("cfg: %+v %v", cfg, err)
	}
	target := outboundTarget(db.ChannelChatSessionBinding{ChatType: "group", ChannelChatID: "123", Config: cfgRaw})
	if target.ChatType != "p2p" || target.SenderID != "u1" {
		t.Fatalf("target from config: %+v", target)
	}
	target = outboundTarget(db.ChannelChatSessionBinding{ChatType: "group", ChannelChatID: "123"})
	if target.ChatType != "group" || target.ChatID != "123" {
		t.Fatalf("target fallback: %+v", target)
	}
	target = outboundTarget(db.ChannelChatSessionBinding{ChatType: "group", ChannelChatID: "123", Config: []byte("{")})
	if target.ChatID != "123" {
		t.Fatalf("target malformed config: %+v", target)
	}
}

func TestOutboundHelpers(t *testing.T) {
	failed := func(payload any) events.Event {
		return events.Event{Type: protocol.EventTaskFailed, Payload: payload}
	}
	done := func(content string) events.Event {
		return events.Event{Type: protocol.EventChatDone, TaskID: "11111111-1111-1111-1111-111111111111", ChatSessionID: "22222222-2222-2222-2222-222222222222", Payload: protocol.ChatDonePayload{TaskID: "11111111-1111-1111-1111-111111111111", ChatSessionID: "22222222-2222-2222-2222-222222222222", Content: content}}
	}
	if !eventRetryPending(failed(map[string]any{"retry_pending": true})) {
		t.Fatal("expected retry pending")
	}
	if eventRetryPending(failed(map[string]any{})) {
		t.Fatal("expected not retry pending")
	}
	if eventRetryPending(done("x")) {
		t.Fatal("chat done is never retry pending")
	}
	if got := eventContent(failed(map[string]any{"error": "boom"})); got != "⚠️ boom" {
		t.Fatalf("failure content: %q", got)
	}
	if got := eventContent(failed(map[string]any{"retry_pending": true, "error": "boom"})); got != "" {
		t.Fatalf("retry pending content: %q", got)
	}
	if got := eventContent(done("hello")); got != "hello" {
		t.Fatalf("chat done content: %q", got)
	}
	taskID, sessionID, ok := taskAndSessionFromEvent(done("hello"))
	if !ok || !taskID.Valid || !sessionID.Valid {
		t.Fatalf("task/session: %v %v %v", taskID, sessionID, ok)
	}
	if _, _, ok := taskAndSessionFromEvent(failed(map[string]any{})); ok {
		t.Fatal("expected missing task id")
	}
}

func TestRegisterBYOValidationRejectsBotUserIDEqualToAppID(t *testing.T) {
	svc := &InstallService{}
	_, err := svc.RegisterBYO(context.Background(), RegisterBYOParams{
		ApplicationID: "14534",
		AppKey:        "k",
		AppSecret:     "s",
		BotUserID:     "14534",
	})
	if !errors.Is(err, ErrInvalidBotUserID) {
		t.Fatalf("expected ErrInvalidBotUserID, got %v", err)
	}
}

func TestInstallServiceValidation(t *testing.T) {
	svc, err := NewInstallService(nil, nil, nil, nil)
	if err == nil || svc != nil {
		t.Fatalf("expected nil-query error, got %v %v", svc, err)
	}
	if _, err := NewInstallService(&db.Queries{}, nil, nil, nil); err == nil {
		t.Fatal("expected tx error")
	}
}

func TestRawHelpers(t *testing.T) {
	if rawAppIDString(nil) != "" || rawAppIDString(" x ") != "x" || rawAppIDString(float64(7)) != "7" {
		t.Fatal("rawAppIDString failed")
	}
	if firstURL("", "u") != "u" || firstURL(nil) != "" {
		t.Fatal("firstURL failed")
	}
	if got := nullText(""); got.Valid {
		t.Fatal("nullText empty should be invalid")
	}
	if got := nullText("x"); !got.Valid || got.String != "x" {
		t.Fatal("nullText value failed")
	}
}

func TestDecodeInfoFlowRawError(t *testing.T) {
	if _, err := decodeInfoFlowRaw(channel.InboundMessage{}); err == nil {
		t.Fatal("expected empty raw error")
	}
	if _, err := decodeInfoFlowRaw(channel.InboundMessage{Raw: json.RawMessage(`{`)}); err == nil {
		t.Fatal("expected malformed raw error")
	}
}

func TestGroupReplyAndMsgTypes(t *testing.T) {
	if got := msgTypeFromPrivate(infoflowRaw{PicURL: "u"}); got != channel.MsgTypeImage {
		t.Fatalf("private image: %v", got)
	}
	if got := msgTypeFromPrivate(infoflowRaw{MsgType: "file"}); got != channel.MsgTypeFile {
		t.Fatalf("private file: %v", got)
	}
	if got := msgTypeFromHeader("voice"); got != channel.MsgTypeAudio {
		t.Fatalf("header voice: %v", got)
	}
	if got := msgTypeFromHeader("weird"); got != channel.MsgTypeUnknown {
		t.Fatalf("header unknown: %v", got)
	}
	if got := anyString(nil); got != "" {
		t.Fatalf("anyString nil: %q", got)
	}
}

func TestClientTokenCacheAndInvalidate(t *testing.T) {
	calls := 0
	srv := newJSONServer(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		_, _ = w.Write([]byte(`{"code":"ok","data":{"app_access_token":"at_1","expire":7200}}`))
	})
	client := NewClient(srv.Client(), srv.URL)
	tok, err := client.AccessToken(context.Background(), "k", "s")
	if err != nil || tok != "at_1" {
		t.Fatalf("token: %q %v", tok, err)
	}
	if _, err := client.AccessToken(context.Background(), "k", "s"); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("expected cached token, calls=%d", calls)
	}
	client.Invalidate("k")
	if _, err := client.AccessToken(context.Background(), "k", "s"); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("expected refresh after invalidate, calls=%d", calls)
	}
	if _, err := client.AccessToken(context.Background(), "", ""); err == nil {
		t.Fatal("expected missing credential error")
	}
}

func TestSenderRetriesOnUnauthorized(t *testing.T) {
	var auths []string
	srv := newJSONServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/auth/app_access_token":
			_, _ = w.Write([]byte(`{"code":"ok","data":{"app_access_token":"at_1","expire":7200}}`))
		case "/robot/msg/groupmsgsend":
			auths = append(auths, r.Header.Get("Authorization"))
			if len(auths) == 1 {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_, _ = w.Write([]byte(`{"code":"ok","data":{"data":{"messageid":"m1"}}}`))
		}
	})
	client := NewClient(srv.Client(), srv.URL)
	id, err := (&sender{client: client, appKey: "k", appSecret: "s", agentID: "14534"}).
		sendText(context.Background(), infoflowSendTarget{ChatType: "group", ChatID: "123"}, "hi")
	if err != nil || id != "m1" {
		t.Fatalf("send: id=%q err=%v auths=%v", id, err, auths)
	}
	if len(auths) != 2 {
		t.Fatalf("expected one retry, auths=%v", auths)
	}
}

func TestChannelSendUsesGateway(t *testing.T) {
	srv := newJSONServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/auth/app_access_token":
			_, _ = w.Write([]byte(`{"code":"ok","data":{"app_access_token":"at_1","expire":7200}}`))
		case "/robot/msg/groupmsgsend":
			_, _ = w.Write([]byte(`{"code":"ok","data":{"messageid":"m9"}}`))
		}
	})
	ch := &infoflowChannel{client: NewClient(srv.Client(), srv.URL), appKey: "k", appSecret: "s", appID: "14534"}
	res, err := ch.Send(context.Background(), channel.OutboundMessage{ChatID: "123", Text: "hi"})
	if err != nil || res.MessageID != "m9" {
		t.Fatalf("send: %+v %v", res, err)
	}
}

func TestChannelConnectWithFakeDialer(t *testing.T) {
	payload := json.RawMessage(`{"FromUserId":"u1","Content":"hi","MsgId":"m1"}`)
	frameBytes, err := marshalFrame(Frame{SeqID: 3, Method: FrameData, Payload: payload})
	if err != nil {
		t.Fatal(err)
	}
	conn := &fakeWSConn{reads: [][]byte{frameBytes}}
	ch := &infoflowChannel{appKey: "k", appSecret: "s", appID: "14534", handler: func(context.Context, channel.InboundMessage) error { return nil }}
	connector := &wsConnector{
		open:   func(context.Context) (string, error) { return "ws://fake", nil },
		dialer: fakeDialer{conn: conn},
		onFrame: func(ctx context.Context, frame Frame) error {
			raw, err := parseFramePayload(frame)
			if err != nil {
				return err
			}
			msg, ok := inboundFromPayload(raw, ch.appID, ch.botUserID)
			if !ok {
				return nil
			}
			return ch.handler(ctx, msg)
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := connector.run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("run: %v", err)
	}
}

func TestOutboundProcessEventIgnoresForeignChannel(t *testing.T) {
	o := NewOutbound(fakeOutboundQueries{delivery: db.ChannelTaskDelivery{ChannelType: "slack"}}, nil, nil, nil)
	err := o.processEvent(context.Background(), events.Event{
		Type:          protocol.EventChatDone,
		TaskID:        "11111111-1111-1111-1111-111111111111",
		ChatSessionID: "22222222-2222-2222-2222-222222222222",
		Payload:       protocol.ChatDonePayload{Content: "hi"},
	})
	if err != nil {
		t.Fatalf("foreign channel should be ignored: %v", err)
	}
}

type fakeOutboundQueries struct {
	delivery db.ChannelTaskDelivery
}

func (f fakeOutboundQueries) ListChatInputMessages(context.Context, pgtype.UUID) ([]db.ChatMessage, error) {
	return nil, nil
}
func (f fakeOutboundQueries) GetAgentTask(context.Context, pgtype.UUID) (db.AgentTaskQueue, error) {
	return db.AgentTaskQueue{}, nil
}
func (f fakeOutboundQueries) TaskHasChannelIngestedMessages(context.Context, pgtype.UUID) (bool, error) {
	return true, nil
}
func (f fakeOutboundQueries) GetChannelTaskDelivery(context.Context, pgtype.UUID) (db.ChannelTaskDelivery, error) {
	return f.delivery, nil
}
func (f fakeOutboundQueries) GetChannelInstallation(context.Context, db.GetChannelInstallationParams) (db.ChannelInstallation, error) {
	return db.ChannelInstallation{
		Status: "active",
		Config: []byte(`{"app_id":"8682","app_key":"k","app_secret_encrypted":"cw==","agent_id":"8682"}`),
	}, nil
}
func (f fakeOutboundQueries) GetWorkspace(context.Context, pgtype.UUID) (db.Workspace, error) {
	return db.Workspace{Slug: "acme"}, nil
}

func newJSONServer(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv
}

type fakeMinter struct {
	tokens int
	err    error
}

func (f *fakeMinter) Mint(context.Context, pgtype.UUID, pgtype.UUID, string) (BindingToken, error) {
	if f.err != nil {
		return BindingToken{}, f.err
	}
	f.tokens++
	return BindingToken{Raw: "raw-token"}, nil
}

func newReplierSendServer(t *testing.T) (*httptest.Server, *infoflowSendRecorder) {
	t.Helper()
	rec := &infoflowSendRecorder{}
	mux := http.NewServeMux()
	send := func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		rec.record(r.URL.Path, string(body))
		_, _ = w.Write([]byte(`{"code":"ok","data":{"messageid":"m-1"}}`))
	}
	mux.HandleFunc("/auth/app_access_token", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"code":"ok","data":{"app_access_token":"at_test","expire":7200}}`))
	})
	mux.HandleFunc("/robot/msg/groupmsgsend", send)
	mux.HandleFunc("/app/message/send", send)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, rec
}

func replierInstallation(t *testing.T, srv *httptest.Server) engine.ResolvedInstallation {
	t.Helper()
	cfg, err := json.Marshal(installConfig{
		AppID: "14534", AppKey: "key", AppSecretEncrypted: base64.StdEncoding.EncodeToString([]byte("secret")), AgentID: "14534",
	})
	if err != nil {
		t.Fatal(err)
	}
	return engine.ResolvedInstallation{
		WorkspaceID: util.MustParseUUID("11111111-1111-1111-1111-111111111111"),
		ID:          util.MustParseUUID("22222222-2222-2222-2222-222222222222"),
		Platform:    db.ChannelInstallation{Config: cfg},
	}
}

func TestReplierBindingPromptPrivate(t *testing.T) {
	srv, rec := newReplierSendServer(t)
	minter := &fakeMinter{}
	r := NewOutboundReplier(OutboundReplierConfig{
		Binding: minter, Client: NewClient(srv.Client(), srv.URL), AppURL: "https://app.example",
	})
	msg := channel.InboundMessage{Source: channel.Source{ChatType: channel.ChatTypeP2P, ChatID: "u1", SenderID: "u1"}}
	r.Reply(context.Background(), replierInstallation(t, srv), msg, engine.Result{Outcome: engine.OutcomeNeedsBinding, Sender: "u1"})
	if minter.tokens != 1 {
		t.Fatalf("minted tokens = %d, want 1", minter.tokens)
	}
	sent := rec.sent()
	if len(sent) != 1 || sent[0] != "/app/message/send" {
		t.Fatalf("binding prompt sends = %v, want one private send", sent)
	}
}

func TestReplierBindingPromptGroupNeverCarriesToken(t *testing.T) {
	srv, rec := newReplierSendServer(t)
	minter := &fakeMinter{}
	r := NewOutboundReplier(OutboundReplierConfig{
		Binding: minter, Client: NewClient(srv.Client(), srv.URL), AppURL: "https://app.example",
	})
	msg := channel.InboundMessage{Source: channel.Source{ChatType: channel.ChatTypeGroup, ChatID: "123", SenderID: "u1"}}
	r.Reply(context.Background(), replierInstallation(t, srv), msg, engine.Result{Outcome: engine.OutcomeNeedsBinding})
	if minter.tokens != 0 {
		t.Fatalf("group prompt must not mint a token, minted %d", minter.tokens)
	}
	if sent := rec.sent(); len(sent) != 1 || sent[0] != "/robot/msg/groupmsgsend" {
		t.Fatalf("group hint sends = %v", sent)
	}
}

func TestReplierStatusAndIssueOutcomes(t *testing.T) {
	srv, rec := newReplierSendServer(t)
	r := NewOutboundReplier(OutboundReplierConfig{Client: NewClient(srv.Client(), srv.URL)})
	inst := replierInstallation(t, srv)
	msg := channel.InboundMessage{Source: channel.Source{ChatType: channel.ChatTypeP2P, ChatID: "u1", SenderID: "u1"}}
	for _, outcome := range []engine.Outcome{engine.OutcomeAgentOffline, engine.OutcomeAgentArchived, engine.OutcomeFreshPending, engine.OutcomeChatStarted, engine.OutcomeIssueUsage} {
		r.Reply(context.Background(), inst, msg, engine.Result{Outcome: outcome})
	}
	if got := len(rec.sent()); got != 5 {
		t.Fatalf("status/command notices sent = %d, want 5", got)
	}
	r.Reply(context.Background(), inst, msg, engine.Result{
		Outcome: engine.OutcomeIngested, IssueID: util.MustParseUUID("33333333-3333-3333-3333-333333333333"),
		IssueIdentifier: "MUL-1", IssueTitle: "a title",
	})
	r.Reply(context.Background(), inst, msg, engine.Result{
		Outcome: engine.OutcomeIngested, IssueID: util.MustParseUUID("33333333-3333-3333-3333-333333333333"),
		IssueIdentifier: "MUL-1", IssueDuplicate: true,
	})
	if got := len(rec.sent()); got != 7 {
		t.Fatalf("issue notices sent = %d, want 7 total", got)
	}
}

func TestReplierDroppedIssueRefusal(t *testing.T) {
	srv, rec := newReplierSendServer(t)
	r := NewOutboundReplier(OutboundReplierConfig{Client: NewClient(srv.Client(), srv.URL)})
	inst := replierInstallation(t, srv)
	msg := channel.InboundMessage{
		Source:         channel.Source{ChatType: channel.ChatTypeP2P, ChatID: "u1", SenderID: "u1"},
		AddressedToBot: true,
		CommandText:    "/issue broken",
	}
	r.Reply(context.Background(), inst, msg, engine.Result{Outcome: engine.OutcomeDropped, DropReason: engine.DropReasonNonWorkspaceMember})
	if got := len(rec.sent()); got != 1 {
		t.Fatalf("drop refusal sends = %d, want 1", got)
	}
	// A non-issue drop stays silent.
	r.Reply(context.Background(), inst, msg, engine.Result{Outcome: engine.OutcomeDropped, DropReason: engine.DropReasonDuplicate})
	if got := len(rec.sent()); got != 1 {
		t.Fatalf("duplicate drop must stay silent, sends = %d", got)
	}
}

func TestChannelSendGuardsAndTarget(t *testing.T) {
	srv, rec := newReplierSendServer(t)
	ch := &infoflowChannel{client: NewClient(srv.Client(), srv.URL), appKey: "key", appSecret: "secret", appID: "14534"}
	if _, err := ch.Send(context.Background(), channel.OutboundMessage{Text: "no target"}); !errors.Is(err, ErrSendRequiresTarget) {
		t.Fatalf("empty target error = %v", err)
	}
	if _, err := ch.SendToTarget(context.Background(), infoflowSendTarget{ChatType: "p2p", ChatID: "u1", SenderID: "u1"}, "hi"); err != nil {
		t.Fatalf("SendToTarget p2p: %v", err)
	}
	if sent := rec.sent(); len(sent) != 1 || sent[0] != "/app/message/send" {
		t.Fatalf("p2p send path = %v", sent)
	}
}

func TestAddressedToBotRequiresSignalWithoutBotUserID(t *testing.T) {
	parse := func(body string) infoflowRaw {
		t.Helper()
		var raw infoflowRaw
		if err := json.Unmarshal([]byte(`{"message":{"header":{"msgtype":"TEXT"},"body":`+body+`}}`), &raw); err != nil {
			t.Fatal(err)
		}
		return raw
	}
	named := parse(`[{"type":"AT","name":"someone"}]`)
	if addressedToBot(named, newIdentitySet("", nil), "MESSAGE_RECEIVE") {
		t.Fatal("a named @ must not count as addressed-to-bot when no bot user id is configured")
	}
	atAll := parse(`[{"type":"AT","atall":true}]`)
	if !addressedToBot(atAll, newIdentitySet("", nil), "MESSAGE_RECEIVE") {
		t.Fatal("@all must count as addressed-to-bot")
	}
	matched := parse(`[{"type":"AT","userid":"bot-9"}]`)
	if !addressedToBot(matched, newIdentitySet("bot-9", nil), "MESSAGE_RECEIVE") {
		t.Fatal("a matching @userid must count as addressed-to-bot")
	}
	// Fail-closed: an ALL_MESSAGE_FORWARD subscription only decides what we
	// receive; it must NOT by itself mark an unmentioned message as addressed.
	if addressedToBot(named, newIdentitySet("", nil), "ALL_MESSAGE_FORWARD") {
		t.Fatal("ALL_MESSAGE_FORWARD without a mention must NOT be addressed (fail-closed)")
	}
}

// N36 regression: the InfoFlow group body carries the mention target in
// `atuserids` (documented shape) — the previous parser never read it, so real
// @ messages were judged unaddressed.
func TestAddressedToBotReadsAtUserIDs(t *testing.T) {
	parse := func(body string, reply bool) infoflowRaw {
		t.Helper()
		replyJSON := "null"
		if reply {
			replyJSON = `{"messageid":"m0"}`
		}
		src := `{"message":{"header":{"msgtype":"TEXT"},"reply":` + replyJSON + `,"body":` + body + `}}`
		var raw infoflowRaw
		if err := json.Unmarshal([]byte(src), &raw); err != nil {
			t.Fatal(err)
		}
		return raw
	}
	cases := []struct {
		name         string
		body         string
		botUserID    string
		mentionNames []string
		resolved     []string
		want         bool
	}{
		{name: "atuserids matches bot", body: `[{"type":"AT","atuserids":["4105001274"]},{"type":"TEXT","content":"hi"}]`, botUserID: "4105001274", want: true},
		{name: "atuserids matches among many", body: `[{"type":"AT","atuserids":["u1","4105001274"]}]`, botUserID: "4105001274", want: true},
		{name: "atuserids other user only", body: `[{"type":"AT","atuserids":["someone-else"]},{"type":"TEXT","content":"hi"}]`, botUserID: "4105001274", want: false},
		{name: "atall is addressed", body: `[{"type":"AT","atall":true}]`, botUserID: "4105001274", want: true},
		{name: "atall with empty bot id still addressed", body: `[{"type":"AT","atall":true}]`, want: true},
		{name: "plain text no AT", body: `[{"type":"TEXT","content":"hello group"}]`, botUserID: "4105001274", want: false},
		{name: "AT without target and no bot id", body: `[{"type":"AT","name":"someone"}]`, want: false},
		// Substring regression: a longer id containing the bot id must NOT match.
		{name: "substring must not match", body: `[{"type":"AT","userid":"x4105001274y"}]`, botUserID: "4105001274", want: false},
		// N40: real InfoFlow group @ carries only the robot DISPLAY NAME.
		{name: "display name via bot_mention_names", body: `[{"type":"AT","name":"Multica"}]`, mentionNames: []string{"Multica"}, want: true},
		{name: "display name case-insensitive", body: `[{"type":"AT","name":"multica"}]`, mentionNames: []string{"Multica"}, want: true},
		{name: "display name via auto-resolved identity", body: `[{"type":"AT","name":"Euler's claw"}]`, botUserID: "4105001274", resolved: []string{"4100104388", "Euler's claw"}, want: true},
		// bot_user_id wrongly holding the GROUP id: the name still matches.
		{name: "group id misconfig still matches by name", body: `[{"type":"AT","name":"Multica"}]`, botUserID: "12572761", mentionNames: []string{"Multica"}, want: true},
		{name: "no candidate token and nothing configured is fail-closed", body: `[{"type":"AT"}]`, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			self := newIdentitySet(tc.botUserID, tc.mentionNames).withResolved(tc.resolved)
			if got := addressedToBot(parse(tc.body, false), self, "MESSAGE_RECEIVE"); got != tc.want {
				t.Fatalf("addressedToBot = %v, want %v (body=%s botUserID=%q)", got, tc.want, tc.body, tc.botUserID)
			}
		})
	}
	// A reply to the bot is addressed without any @.
	if !addressedToBot(parse(`[{"type":"TEXT","content":"thanks"}]`, true), newIdentitySet("", nil), "MESSAGE_RECEIVE") {
		t.Fatal("a reply to the bot must be addressed")
	}
}

func TestParseFramePayloadErrors(t *testing.T) {
	if _, err := parseFramePayload(Frame{}); err == nil {
		t.Fatal("empty payload must error")
	}
	if _, err := parseFramePayload(Frame{Payload: []byte("{")}); err == nil {
		t.Fatal("malformed json must error")
	}
	raw, err := parseFramePayload(Frame{Payload: []byte(`{"a":1}`)})
	if err != nil || len(raw) == 0 {
		t.Fatalf("valid payload: %v %v", raw, err)
	}
}

// singleChatOutboundQueries drives Outbound.processEvent for a p2p delivery.
type singleChatOutboundQueries struct {
	installation db.ChannelInstallation
	delivery     db.ChannelTaskDelivery
}

func (f singleChatOutboundQueries) ListChatInputMessages(context.Context, pgtype.UUID) ([]db.ChatMessage, error) {
	return nil, nil
}
func (f singleChatOutboundQueries) GetAgentTask(context.Context, pgtype.UUID) (db.AgentTaskQueue, error) {
	return db.AgentTaskQueue{}, nil
}
func (f singleChatOutboundQueries) TaskHasChannelIngestedMessages(context.Context, pgtype.UUID) (bool, error) {
	return true, nil
}
func (f singleChatOutboundQueries) GetChannelTaskDelivery(context.Context, pgtype.UUID) (db.ChannelTaskDelivery, error) {
	return f.delivery, nil
}
func (f singleChatOutboundQueries) GetChannelInstallation(context.Context, db.GetChannelInstallationParams) (db.ChannelInstallation, error) {
	return f.installation, nil
}
func (f singleChatOutboundQueries) GetWorkspace(context.Context, pgtype.UUID) (db.Workspace, error) {
	return db.Workspace{Slug: "acme"}, nil
}

// A p2p reply that fails with 40000 "Api not allowed" is a known capability gap
// (the platform has not granted /app/message/send). It must be swallowed with a
// warning, not surfaced as a delivery error that retries forever.
func TestOutboundSoftDegradesSingleChatPermissionDenied(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/auth/app_access_token":
			_, _ = w.Write([]byte(`{"code":"ok","data":{"app_access_token":"at_1","expire":7200}}`))
		case "/app/message/send":
			calls++
			_, _ = w.Write([]byte(`{"code":"ok","data":{"errcode":40000,"errmsg":"Api not allowed"}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	cfg, err := json.Marshal(installConfig{
		AppID: "14534", AppKey: "k", AppSecretEncrypted: base64.StdEncoding.EncodeToString([]byte("s")), AgentID: "14534",
	})
	if err != nil {
		t.Fatal(err)
	}
	bindingCfg, err := json.Marshal(infoflowBindingConfig{ChatType: string(channel.ChatTypeP2P), ChatID: "u1", SenderID: "u1"})
	if err != nil {
		t.Fatal(err)
	}
	q := singleChatOutboundQueries{
		installation: db.ChannelInstallation{Status: "active", Config: cfg},
		delivery: db.ChannelTaskDelivery{
			ChannelType: string(TypeInfoFlow), ChatType: string(channel.ChatTypeP2P),
			ChannelChatID: "u1", Config: bindingCfg,
		},
	}
	o := NewOutbound(q, nil, NewClient(srv.Client(), srv.URL), nil)
	err = o.processEvent(context.Background(), events.Event{
		Type:          protocol.EventChatDone,
		TaskID:        "11111111-1111-1111-1111-111111111111",
		ChatSessionID: "22222222-2222-2222-2222-222222222222",
		Payload:       protocol.ChatDonePayload{Content: "hi"},
	})
	if err != nil {
		t.Fatalf("permission-denied single-chat reply must soft-degrade, got %v", err)
	}
	if calls != 1 {
		t.Fatalf("expected exactly one send attempt (no retry), got %d", calls)
	}
}

// Drop reasons exist only for payloads the adapter cannot turn into a message.
// An unaddressed group message is NOT one of them: it must normalize (ok=true)
// so the engine keeps ownership of the drop — writing channel_inbound_audit and
// running the dedup gate (N17 regression guard).
func TestInboundDropReasons(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "missing sender",
			raw:  `{"groupid":"12572761","message":{"header":{"totype":"GROUP","msgtype":"TEXT","messageid":"m3"},"body":[{"type":"AT","userid":"bot"},{"type":"TEXT","content":"hi"}]}}`,
			want: dropReasonMissingSender,
		},
		{
			name: "empty content",
			raw:  `{"groupid":"12572761","message":{"header":{"fromuserid":"u1","totype":"GROUP","msgtype":"TEXT","messageid":"m4"},"body":[]}}`,
			want: dropReasonEmptyContent,
		},
		{
			name: "malformed payload",
			raw:  `{`,
			want: dropReasonMalformed,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msg, reason, ok := inboundFromPayloadReason(json.RawMessage(tc.raw), "14534", "4105001274")
			if ok {
				t.Fatalf("expected the adapter to refuse this payload, got %+v", msg)
			}
			if reason != tc.want {
				t.Fatalf("drop reason = %q, want %q", reason, tc.want)
			}
		})
	}

	// An addressed group message routes.
	_, reason, ok := inboundFromPayloadReason(json.RawMessage(
		`{"groupid":"12572761","message":{"header":{"fromuserid":"u1","totype":"GROUP","msgtype":"TEXT","messageid":"m5"},"body":[{"type":"AT","userid":"4105001274"},{"type":"TEXT","content":"hi"}]}}`),
		"14534", "4105001274")
	if !ok || reason != "" {
		t.Fatalf("addressed group message should route: ok=%v reason=%q", ok, reason)
	}
}

// N17 regression guard: an UNADDRESSED group message must still normalize and be
// handed to the engine, so the engine can record channel_inbound_audit with
// drop_reason=not_addressed_in_group and run the dedup gate. Deciding the drop
// in the adapter would silently remove that audit trail.
func TestUnaddressedGroupMessageStillReachesTheEngine(t *testing.T) {
	const raw = `{"groupid":"12572761","message":{"header":{"fromuserid":"u1","totype":"GROUP","msgtype":"TEXT","messageid":"m-unaddressed"},"body":[{"type":"TEXT","content":"group chatter"}]}}`

	msg, reason, ok := inboundFromPayloadReason(json.RawMessage(raw), "14534", "4105001274")
	if !ok {
		t.Fatalf("unaddressed group message must normalize for the engine, got ok=false reason=%q", reason)
	}
	if reason != "" {
		t.Fatalf("no adapter-side drop reason expected, got %q", reason)
	}
	if msg.AddressedToBot {
		t.Fatal("fixture must be unaddressed so the engine takes its not_addressed_in_group path")
	}
	if msg.Source.ChatType != channel.ChatTypeGroup || msg.Source.ChatID != "12572761" {
		t.Fatalf("unexpected routing identity: %+v", msg.Source)
	}
	if msg.MessageID != "m-unaddressed" {
		t.Fatalf("message id = %q, engine dedup keys on this", msg.MessageID)
	}

	// The plain accessor (used elsewhere) agrees.
	if _, ok := inboundFromPayload(json.RawMessage(raw), "14534", "4105001274"); !ok {
		t.Fatal("inboundFromPayload must agree that the message routes")
	}
}

// S1: the replier's failure fields carry the same filterable keys as outbound.
func TestReplyFailureFields(t *testing.T) {
	inst := replierInstallation(t, nil)
	msg := channel.InboundMessage{Source: channel.Source{ChatType: channel.ChatTypeP2P, ChatID: "u1", SenderID: "u1"}}
	fields := replyFailureFields(inst, msg, errors.New("boom"))
	joined := make(map[string]any)
	for i := 0; i+1 < len(fields); i += 2 {
		key, _ := fields[i].(string)
		joined[key] = fields[i+1]
	}
	for _, key := range []string{"error", "error_kind", "installation_id", "chat_type", "chat_id", "action"} {
		if _, ok := joined[key]; !ok {
			t.Fatalf("missing log field %q in %v", key, fields)
		}
	}
	if joined["error_kind"] != "delivery_failed" {
		t.Fatalf("error_kind = %v, want delivery_failed", joined["error_kind"])
	}
}
