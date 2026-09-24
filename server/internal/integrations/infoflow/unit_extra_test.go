package infoflow

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/integrations/channel"
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
		open: func(context.Context) (string, error) { return "ws://fake", nil },
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
		Type: protocol.EventChatDone,
		TaskID: "11111111-1111-1111-1111-111111111111",
		ChatSessionID: "22222222-2222-2222-2222-222222222222",
		Payload: protocol.ChatDonePayload{Content: "hi"},
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
	return db.ChannelInstallation{Status: "active"}, nil
}


func newJSONServer(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv
}
