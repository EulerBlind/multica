package infoflow

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/integrations/channel"
)

// quoteGateway is a fake InfoFlow OpenAPI that answers the token endpoint and
// getSingleMsg, and records how often each was hit.
type quoteGateway struct {
	mu         sync.Mutex
	tokenCalls int
	msgCalls   int
	lastQuery  url.Values
	message    func(call int, w http.ResponseWriter, r *http.Request)
}

func (g *quoteGateway) start(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/auth/app_access_token":
			g.mu.Lock()
			g.tokenCalls++
			g.mu.Unlock()
			_, _ = w.Write([]byte(`{"code":"ok","data":{"app_access_token":"at_1","expire":7200}}`))
		case "/im/message/getSingleMsg":
			g.mu.Lock()
			g.msgCalls++
			call := g.msgCalls
			g.lastQuery = r.URL.Query()
			handler := g.message
			g.mu.Unlock()
			handler(call, w, r)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (g *quoteGateway) calls() (token, message int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.tokenCalls, g.msgCalls
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func testQuoteRequest(msgID string) quoteEnrichRequest {
	return quoteEnrichRequest{
		AppID:     "14534",
		AppKey:    "app-key",
		AppSecret: "app-secret",
		Query: quoteQuery{
			FromID:       "wangjianghua",
			ReceiverID:   "12572761",
			ReceiverType: 2,
			MsgID:        msgID,
		},
	}
}

func TestQuoteEnricherReturnsFullText(t *testing.T) {
	full := strings.Repeat("这是一段被引用的长文本。", 80)
	gw := &quoteGateway{message: func(_ int, w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"code":"ok","data":{"msgId":"m8","msgType":"text","content":"` + full + `"}}`))
	}}
	srv := gw.start(t)
	enricher := newQuoteEnricher(NewClient(srv.Client(), srv.URL), discardLogger())

	origin := enricher.Enrich(context.Background(), testQuoteRequest("m8"))
	if origin.State != quoteStateOK || origin.Text != full {
		t.Fatalf("origin = %+v, want ok with full text", origin)
	}
	if origin.Source != "api" {
		t.Fatalf("source = %q, want api", origin.Source)
	}
	query := gw.lastQuery
	for key, want := range map[string]string{
		"fromId":       "wangjianghua",
		"receiverId":   "12572761",
		"receiverType": "2",
		"msgId":        "m8",
	} {
		if got := query.Get(key); got != want {
			t.Fatalf("%s = %q, want %q", key, got, want)
		}
	}
}

func TestQuoteEnricherSkipsWithoutMessageID(t *testing.T) {
	gw := &quoteGateway{message: func(_ int, w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"code":"ok","data":{"content":"should not be fetched"}}`))
	}}
	srv := gw.start(t)
	enricher := newQuoteEnricher(NewClient(srv.Client(), srv.URL), discardLogger())

	origin := enricher.Enrich(context.Background(), testQuoteRequest(""))
	if origin.State != quoteStateSkipped {
		t.Fatalf("state = %q, want skipped", origin.State)
	}
	if token, message := gw.calls(); token != 0 || message != 0 {
		t.Fatalf("no HTTP for a quote without msgId, token=%d message=%d", token, message)
	}
}

func TestQuoteEnricherDeniedStartsCooldown(t *testing.T) {
	gw := &quoteGateway{message: func(_ int, w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"code":"ok","data":{"errcode":40000,"errmsg":"Api not allowed"}}`))
	}}
	srv := gw.start(t)
	enricher := newQuoteEnricher(NewClient(srv.Client(), srv.URL), discardLogger())

	first := enricher.Enrich(context.Background(), testQuoteRequest("m1"))
	if first.State != quoteStateDenied {
		t.Fatalf("state = %q, want denied (%v)", first.State, first.Error)
	}
	second := enricher.Enrich(context.Background(), testQuoteRequest("m2"))
	if second.State != quoteStateDenied {
		t.Fatalf("state = %q, want denied", second.State)
	}
	if _, message := gw.calls(); message != 1 {
		t.Fatalf("cooldown must suppress the second lookup, message calls=%d", message)
	}
}

func TestQuoteEnricherClassifiesTopLevelErrcode(t *testing.T) {
	gw := &quoteGateway{message: func(_ int, w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"code":"error","errcode":40070,"errmsg":"x"}`))
	}}
	srv := gw.start(t)
	enricher := newQuoteEnricher(NewClient(srv.Client(), srv.URL), discardLogger())

	origin := enricher.Enrich(context.Background(), testQuoteRequest("m8"))
	if origin.State != quoteStateUnavailable {
		t.Fatalf("state = %q, want unavailable", origin.State)
	}
	if !strings.Contains(origin.Error, "40070") {
		t.Fatalf("error must carry the top-level errcode, got %q", origin.Error)
	}
}

func TestQuoteEnricherTimeout(t *testing.T) {
	gw := &quoteGateway{message: func(_ int, w http.ResponseWriter, _ *http.Request) {
		time.Sleep(500 * time.Millisecond)
		_, _ = w.Write([]byte(`{"code":"ok","data":{"content":"late"}}`))
	}}
	srv := gw.start(t)
	enricher := newQuoteEnricher(NewClient(srv.Client(), srv.URL), discardLogger())
	enricher.timeout = 50 * time.Millisecond

	start := time.Now()
	origin := enricher.Enrich(context.Background(), testQuoteRequest("m8"))
	elapsed := time.Since(start)
	if origin.State != quoteStateTimeout {
		t.Fatalf("state = %q, want timeout", origin.State)
	}
	if elapsed > time.Second {
		t.Fatalf("lookup must respect its timeout, took %s", elapsed)
	}
}

func TestQuoteEnricherRetriesOnceOnUnauthorized(t *testing.T) {
	gw := &quoteGateway{message: func(call int, w http.ResponseWriter, _ *http.Request) {
		if call == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{}`))
			return
		}
		_, _ = w.Write([]byte(`{"code":"ok","data":{"msgType":"text","content":"全文"}}`))
	}}
	srv := gw.start(t)
	enricher := newQuoteEnricher(NewClient(srv.Client(), srv.URL), discardLogger())

	origin := enricher.Enrich(context.Background(), testQuoteRequest("m8"))
	if origin.State != quoteStateOK || origin.Text != "全文" {
		t.Fatalf("origin = %+v, want ok after one retry", origin)
	}
	if token, message := gw.calls(); token != 2 || message != 2 {
		t.Fatalf("expected one token refresh and one retry, token=%d message=%d", token, message)
	}
}

func TestQuoteEnricherCachesSuccess(t *testing.T) {
	gw := &quoteGateway{message: func(_ int, w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"code":"ok","data":{"content":"全文"}}`))
	}}
	srv := gw.start(t)
	enricher := newQuoteEnricher(NewClient(srv.Client(), srv.URL), discardLogger())

	for i := 0; i < 3; i++ {
		if origin := enricher.Enrich(context.Background(), testQuoteRequest("m8")); origin.State != quoteStateOK {
			t.Fatalf("state = %q", origin.State)
		}
	}
	if _, message := gw.calls(); message != 1 {
		t.Fatalf("cache must collapse repeated lookups, message calls=%d", message)
	}
}

// TestChannelHandleFrameEnrichesGroupQuote drives a real frame through the
// adapter: the truncated preview must be replaced by the full text while the
// quoted image URL still comes from the inbound body.
func TestChannelHandleFrameEnrichesGroupQuote(t *testing.T) {
	full := strings.Repeat("引用正文", 400)
	gw := &quoteGateway{message: func(_ int, w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"code":"ok","data":{"msgType":"text","content":"` + full + `"}}`))
	}}
	srv := gw.start(t)
	raw := json.RawMessage(`{"groupid":"12572761","message":{"header":{"fromuserid":"wangjianghua","toid":"12572761","totype":"GROUP","msgtype":"MIXED","messageid":"m9"},"reply":{"messageid":"m8","header":{"fromuserid":"u-quoted"},"preview":"引用正文这是一段被截…","body":[{"type":"IMAGE","downloadurl":"https://example.com/q.png"}]},"body":[{"type":"AT","atuserids":["4100104388"]},{"type":"TEXT","content":"帮我看看这张图"}]}}`)

	var got channel.InboundMessage
	ch := &infoflowChannel{
		appID:         "14534",
		appKey:        "app-key",
		appSecret:     "app-secret",
		botUserID:     "4100104388",
		client:        NewClient(srv.Client(), srv.URL),
		quoteEnricher: newQuoteEnricher(NewClient(srv.Client(), srv.URL), discardLogger()),
		logger:        discardLogger(),
		handler: func(_ context.Context, msg channel.InboundMessage) error {
			got = msg
			return nil
		},
	}
	if err := ch.handleFrame(context.Background(), Frame{Method: FrameData, Payload: raw}); err != nil {
		t.Fatalf("handleFrame: %v", err)
	}
	for _, want := range []string{quoteTagFull, full, "[IMAGE] https://example.com/q.png", "帮我看看这张图"} {
		if !strings.Contains(got.Text, want) {
			t.Fatalf("Text missing %q: %s", want, got.Text)
		}
	}
	if strings.Contains(got.Text, "这是一段被截…") {
		t.Fatalf("truncated preview must be replaced by the full text: %s", got.Text)
	}
	if got.CommandText != "帮我看看这张图" {
		t.Fatalf("CommandText = %q", got.CommandText)
	}
	if !strings.Contains(string(got.Raw), `"multica_quote_origin"`) || !strings.Contains(string(got.Raw), `"state":"ok"`) {
		t.Fatalf("Raw must carry the origin: %s", got.Raw)
	}
	if !got.HasSelectedContext || got.ReplyTo == nil || got.ReplyTo.MessageID != "m8" {
		t.Fatalf("selected context regression: %+v", got)
	}
}

// TestChannelHandleFrameDegradesOnDenied: a permission failure must not drop the
// main message; the preview stays with an explicit label.
func TestChannelHandleFrameDegradesOnDenied(t *testing.T) {
	gw := &quoteGateway{message: func(_ int, w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"code":"ok","data":{"errcode":40000,"errmsg":"Api not allowed"}}`))
	}}
	srv := gw.start(t)
	raw := json.RawMessage(`{"groupid":"12572761","message":{"header":{"fromuserid":"wangjianghua","toid":"12572761","totype":"GROUP","msgtype":"TEXT","messageid":"m9"},"reply":{"messageid":"m8","header":{"fromuserid":"u-quoted"},"preview":"被引用的预览文本"},"body":[{"type":"AT","atuserids":["4100104388"]},{"type":"TEXT","content":"原消息"}]}}`)

	var got channel.InboundMessage
	ch := &infoflowChannel{
		appID:         "14534",
		appKey:        "app-key",
		appSecret:     "app-secret",
		botUserID:     "4100104388",
		client:        NewClient(srv.Client(), srv.URL),
		quoteEnricher: newQuoteEnricher(NewClient(srv.Client(), srv.URL), discardLogger()),
		logger:        discardLogger(),
		handler: func(_ context.Context, msg channel.InboundMessage) error {
			got = msg
			return nil
		},
	}
	if err := ch.handleFrame(context.Background(), Frame{Method: FrameData, Payload: raw}); err != nil {
		t.Fatalf("handleFrame: %v", err)
	}
	if !strings.Contains(got.Text, quoteTagPreview) || !strings.Contains(got.Text, "被引用的预览文本") {
		t.Fatalf("denied lookup must keep the labelled preview: %s", got.Text)
	}
	if !strings.Contains(got.Text, "原消息") {
		t.Fatalf("main message must survive: %s", got.Text)
	}
}

// TestChannelHandleFrameSingleChatQuote: the p2p path used to ignore quotes
// entirely; it now renders them and looks the original up with receiverType=7.
func TestChannelHandleFrameSingleChatQuote(t *testing.T) {
	gw := &quoteGateway{message: func(_ int, w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"code":"ok","data":{"msgType":"text","content":"单聊引用的完整原文"}}`))
	}}
	srv := gw.start(t)
	raw := json.RawMessage(`{"agentid":"14534","FromUserId":"u1","Content":"接着上面的问题","MsgId":"m1","MsgType":"text","quote":{"messageid":"m0","header":{"fromuserid":"u-quoted"},"preview":"单聊引用的预览…"}}`)

	var got channel.InboundMessage
	ch := &infoflowChannel{
		appID:         "14534",
		appKey:        "app-key",
		appSecret:     "app-secret",
		client:        NewClient(srv.Client(), srv.URL),
		quoteEnricher: newQuoteEnricher(NewClient(srv.Client(), srv.URL), discardLogger()),
		logger:        discardLogger(),
		handler: func(_ context.Context, msg channel.InboundMessage) error {
			got = msg
			return nil
		},
	}
	if err := ch.handleFrame(context.Background(), Frame{Method: FrameData, Payload: raw}); err != nil {
		t.Fatalf("handleFrame: %v", err)
	}
	if !got.HasSelectedContext || got.ReplyTo == nil || got.ReplyTo.MessageID != "m0" {
		t.Fatalf("p2p quote must be selected context: %+v", got)
	}
	if !strings.Contains(got.Text, "单聊引用的完整原文") || !strings.Contains(got.Text, "接着上面的问题") {
		t.Fatalf("p2p quote text = %q", got.Text)
	}
	if origin := got.Raw; !strings.Contains(string(origin), `"state":"ok"`) {
		t.Fatalf("Raw = %s", origin)
	}
	query := gw.lastQuery
	if got := query.Get("receiverType"); got != "7" {
		t.Fatalf("receiverType = %q, want 7", got)
	}
	if got := query.Get("receiverId"); got != "14534" {
		t.Fatalf("receiverId = %q, want agent id", got)
	}
}

func TestQuoteOriginEnabledSwitch(t *testing.T) {
	t.Setenv(quoteOriginEnv, "off")
	if quoteOriginEnabled() {
		t.Fatal("off must disable the lookup")
	}
	t.Setenv(quoteOriginEnv, "")
	if !quoteOriginEnabled() {
		t.Fatal("unset must keep the lookup on")
	}
}

func TestTruncateQuoteTextCapsAt100KB(t *testing.T) {
	full := strings.Repeat("长", (100<<10)+10)
	got := truncateQuoteText(full)
	if !strings.HasSuffix(got, quoteTagOverlong) {
		t.Fatalf("overlong text must be marked, tail=%q", got[len(got)-20:])
	}
	if len(got) > (100<<10)+(len(quoteTagOverlong)*3) {
		t.Fatalf("truncated text still too large: %d bytes", len(got))
	}
	if short := truncateQuoteText("短文本"); short != "短文本" {
		t.Fatalf("short text must be untouched, got %q", short)
	}
}

func TestQuoteCacheEvictsOldest(t *testing.T) {
	cache := newQuoteCache(2)
	now := time.Now()
	cache.set("a", &quoteOrigin{State: quoteStateOK, Text: "a"}, time.Minute, now)
	cache.set("b", &quoteOrigin{State: quoteStateOK, Text: "b"}, time.Minute, now)
	cache.set("c", &quoteOrigin{State: quoteStateOK, Text: "c"}, time.Minute, now)
	if _, ok := cache.get("a", now); ok {
		t.Fatal("oldest entry must be evicted")
	}
	if origin, ok := cache.get("c", now); !ok || origin.Text != "c" {
		t.Fatal("newest entry must be cached")
	}
	if _, ok := cache.get("c", now.Add(2*time.Minute)); ok {
		t.Fatal("expired entry must be dropped")
	}
}

func TestQuoteEnricherNotFound(t *testing.T) {
	gw := &quoteGateway{message: func(_ int, w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{}`))
	}}
	srv := gw.start(t)
	enricher := newQuoteEnricher(NewClient(srv.Client(), srv.URL), discardLogger())
	if origin := enricher.Enrich(context.Background(), testQuoteRequest("m8")); origin.State != quoteStateNotFound {
		t.Fatalf("state = %q, want not_found", origin.State)
	}
}

// TestChannelHandleFrameSingleChatFallsBackToInstallAppID: a p2p event without
// agentid must still look the quote up against the installation's own app id.
func TestChannelHandleFrameSingleChatFallsBackToInstallAppID(t *testing.T) {
	gw := &quoteGateway{message: func(_ int, w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"code":"ok","data":{"msgType":"text","content":"单聊全文"}}`))
	}}
	srv := gw.start(t)
	raw := json.RawMessage(`{"FromUserId":"u1","Content":"问题","MsgId":"m1","quote":{"messageid":"m0","header":{"fromuserid":"u-quoted"},"preview":"预览…"}}`)

	var got channel.InboundMessage
	ch := &infoflowChannel{
		appID:         "14534",
		appKey:        "app-key",
		appSecret:     "app-secret",
		client:        NewClient(srv.Client(), srv.URL),
		quoteEnricher: newQuoteEnricher(NewClient(srv.Client(), srv.URL), discardLogger()),
		logger:        discardLogger(),
		handler: func(_ context.Context, msg channel.InboundMessage) error {
			got = msg
			return nil
		},
	}
	if err := ch.handleFrame(context.Background(), Frame{Method: FrameData, Payload: raw}); err != nil {
		t.Fatalf("handleFrame: %v", err)
	}
	if !strings.Contains(got.Text, "单聊全文") {
		t.Fatalf("Text = %q", got.Text)
	}
	if receiver := gw.lastQuery.Get("receiverId"); receiver != "14534" {
		t.Fatalf("receiverId = %q, want the installation app id", receiver)
	}
}

// TestQuoteQueryRequiresCurrentSender locks DEC17: fromId is the current
// sender; a quote without identity or without a current sender makes no query.
func TestQuoteQueryRequiresCurrentSender(t *testing.T) {
	payload := infoflowRaw{FromUserID: "U-X", Message: &infoflowEventMessage{
		Header: infoflowHeader{FromUserID: "U-X"},
		Reply:  &infoflowQuote{MessageID: "Q-10", Preview: "只有预览", Header: infoflowHeader{FromUserID: "U-QUOTED"}},
	}}
	msg := channel.InboundMessage{Source: channel.Source{ChannelType: TypeInfoFlow, ChatType: channel.ChatTypeGroup, ChatID: "G-1"}}
	if query, ok := quoteQueryFor(msg, payload, payload.Message.Reply); ok {
		t.Fatalf("quote without a current sender must not be looked up: %+v", query)
	}
	msg.Source.SenderID = "U-CURRENT"
	query, ok := quoteQueryFor(msg, payload, payload.Message.Reply)
	if !ok || query.FromID != "U-CURRENT" || query.MsgID != "Q-10" {
		t.Fatalf("current sender must be fromId: %+v ok=%v", query, ok)
	}
	payload.Message.Reply.MessageID = ""
	if query, ok := quoteQueryFor(msg, payload, payload.Message.Reply); ok {
		t.Fatalf("quote without msgId must not be looked up: %+v", query)
	}
}

// TestChannelHandleFrameQuoteWithoutMsgIDMakesNoRequest: DEC17 keeps the
// zero-request guard for a quote that carries no identity. The current sender
// alone is not enough.
func TestChannelHandleFrameQuoteWithoutMsgIDMakesNoRequest(t *testing.T) {
	gw := &quoteGateway{message: func(_ int, w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"code":"ok","data":{"content":"不应被请求"}}`))
	}}
	srv := gw.start(t)
	raw := json.RawMessage(`{"groupid":"G-Q4","message":{"header":{"fromuserid":"U-X","toid":"G-Q4","totype":"GROUP","msgtype":"TEXT","messageid":"M-Q4"},"reply":{"messageid":"","preview":"只有预览的引用"},"body":[{"type":"AT","atuserids":["4100104388"]},{"type":"TEXT","content":"主消息"}]}}`)

	var got channel.InboundMessage
	ch := &infoflowChannel{
		appID:         "14534",
		appKey:        "app-key",
		appSecret:     "app-secret",
		botUserID:     "4100104388",
		client:        NewClient(srv.Client(), srv.URL),
		quoteEnricher: newQuoteEnricher(NewClient(srv.Client(), srv.URL), discardLogger()),
		logger:        discardLogger(),
		handler: func(_ context.Context, msg channel.InboundMessage) error {
			got = msg
			return nil
		},
	}
	if err := ch.handleFrame(context.Background(), Frame{Method: FrameData, Payload: raw}); err != nil {
		t.Fatalf("handleFrame: %v", err)
	}
	if token, message := gw.calls(); token != 0 || message != 0 {
		t.Fatalf("quote without msgId must make zero requests, token=%d message=%d", token, message)
	}
	for _, want := range []string{quoteTagPreview, "只有预览的引用", "主消息"} {
		if !strings.Contains(got.Text, want) {
			t.Fatalf("Text missing %q: %s", want, got.Text)
		}
	}
	if !strings.Contains(string(got.Raw), `"state":"skipped"`) {
		t.Fatalf("Raw must record the skipped origin: %s", got.Raw)
	}
}

// bodyQuoteRaw builds the live N88 shape: the selected quote is a body element.
func bodyQuoteRaw(quoteFields string, own string) json.RawMessage {
	return json.RawMessage(`{"groupid":"12572761","message":{"header":{"fromuserid":"wangjianghua","toid":"12572761","totype":"GROUP","msgtype":"MIXED","messageid":"m9"},"body":[{"type":"AT","atuserids":["4100104388"]},{"type":"QUOTE",` + quoteFields + `},{"type":"TEXT","content":"` + own + `"}]}}`)
}

// TestChannelHandleFrameEnrichesBodyQuote is A28 end to end: a body QUOTE element
// with a messageid triggers getSingleMsg using the current sender as fromId and
// is replaced by the full text while the main message survives.
func TestChannelHandleFrameEnrichesBodyQuote(t *testing.T) {
	full := strings.Repeat("完整引用正文", 300)
	gw := &quoteGateway{message: func(_ int, w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"code":"ok","data":{"code":200,"message":"ok","data":{"items":[{"msgId":"m8","msgType":"text","content":"` + full + `"}]}}}`))
	}}
	srv := gw.start(t)
	raw := bodyQuoteRaw(`"content":"逐张建成独..","messageid":"m8","fromuserid":"u-quoted"`, "能看到这里的内容么？")

	var got channel.InboundMessage
	ch := &infoflowChannel{
		appID:         "14534",
		appKey:        "app-key",
		appSecret:     "app-secret",
		botUserID:     "4100104388",
		client:        NewClient(srv.Client(), srv.URL),
		quoteEnricher: newQuoteEnricher(NewClient(srv.Client(), srv.URL), discardLogger()),
		logger:        discardLogger(),
		handler: func(_ context.Context, msg channel.InboundMessage) error {
			got = msg
			return nil
		},
	}
	if err := ch.handleFrame(context.Background(), Frame{Method: FrameData, Payload: raw}); err != nil {
		t.Fatalf("handleFrame: %v", err)
	}
	for _, want := range []string{quoteTagFull, full, "能看到这里的内容么？"} {
		if !strings.Contains(got.Text, want) {
			t.Fatalf("Text missing %q: %s", want, got.Text)
		}
	}
	if strings.Contains(got.Text, "逐张建成独..") {
		t.Fatalf("truncated preview must be replaced: %s", got.Text)
	}
	if !got.HasSelectedContext || got.ReplyTo == nil || got.ReplyTo.MessageID != "m8" {
		t.Fatalf("selected context regression: %+v", got)
	}
	if !strings.Contains(string(got.Raw), `"state":"ok"`) {
		t.Fatalf("Raw must carry ok origin: %s", got.Raw)
	}
	query := gw.lastQuery
	for key, want := range map[string]string{"fromId": "wangjianghua", "receiverId": "12572761", "receiverType": "2", "msgId": "m8"} {
		if query.Get(key) != want {
			t.Fatalf("%s = %q, want %q", key, query.Get(key), want)
		}
	}
}

// TestChannelHandleFrameBodyQuoteWithoutPreviewRecoversFullText: even when the
// body element has only ids, the lookup result must still render the quote.
func TestChannelHandleFrameBodyQuoteWithoutPreviewRecoversFullText(t *testing.T) {
	full := strings.Repeat("无预览的引用全文", 20)
	gw := &quoteGateway{message: func(_ int, w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"code":"ok","data":{"code":200,"data":{"items":[{"msgId":"m8","content":"` + full + `"}]}}}`))
	}}
	srv := gw.start(t)
	raw := bodyQuoteRaw(`"messageid":"m8","fromuserid":"u-quoted"`, "我的问题")

	var got channel.InboundMessage
	ch := &infoflowChannel{
		appID:         "14534",
		appKey:        "app-key",
		appSecret:     "app-secret",
		botUserID:     "4100104388",
		client:        NewClient(srv.Client(), srv.URL),
		quoteEnricher: newQuoteEnricher(NewClient(srv.Client(), srv.URL), discardLogger()),
		logger:        discardLogger(),
		handler: func(_ context.Context, msg channel.InboundMessage) error {
			got = msg
			return nil
		},
	}
	if err := ch.handleFrame(context.Background(), Frame{Method: FrameData, Payload: raw}); err != nil {
		t.Fatalf("handleFrame: %v", err)
	}
	for _, want := range []string{quoteTagFull, full, "我的问题"} {
		if !strings.Contains(got.Text, want) {
			t.Fatalf("Text missing %q: %s", want, got.Text)
		}
	}
}

// TestChannelHandleFrameBodyQuoteEmptyItemsIsNotFound: items:null must classify
// as not_found (truncated label), never as ok with empty text (DEC16).
func TestChannelHandleFrameBodyQuoteEmptyItemsIsNotFound(t *testing.T) {
	gw := &quoteGateway{message: func(_ int, w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"code":"ok","data":{"code":200,"message":"ok","data":{"items":null}}}`))
	}}
	srv := gw.start(t)
	raw := bodyQuoteRaw(`"content":"已过期引用的预览","messageid":"m8","fromuserid":"u-quoted"`, "主消息")

	var got channel.InboundMessage
	ch := &infoflowChannel{
		appID:         "14534",
		appKey:        "app-key",
		appSecret:     "app-secret",
		botUserID:     "4100104388",
		client:        NewClient(srv.Client(), srv.URL),
		quoteEnricher: newQuoteEnricher(NewClient(srv.Client(), srv.URL), discardLogger()),
		logger:        discardLogger(),
		handler: func(_ context.Context, msg channel.InboundMessage) error {
			got = msg
			return nil
		},
	}
	if err := ch.handleFrame(context.Background(), Frame{Method: FrameData, Payload: raw}); err != nil {
		t.Fatalf("handleFrame: %v", err)
	}
	for _, want := range []string{quoteTagTruncated, "已过期引用的预览", "主消息"} {
		if !strings.Contains(got.Text, want) {
			t.Fatalf("Text missing %q: %s", want, got.Text)
		}
	}
	if !strings.Contains(string(got.Raw), `"state":"not_found"`) {
		t.Fatalf("Raw must record not_found: %s", got.Raw)
	}
}

// TestChannelHandleFrameBodyQuoteBusinessCodes maps data.code 720001/720027 to
// denied/invalid_param and keeps the preview plus the main message.
func TestChannelHandleFrameBodyQuoteBusinessCodes(t *testing.T) {
	for _, tc := range []struct {
		name  string
		code  int
		state string
	}{
		{name: "denied", code: 720001, state: "denied"},
		{name: "invalid_param", code: 720027, state: "invalid_param"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gw := &quoteGateway{message: func(_ int, w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(`{"code":"ok","data":{"code":` + strconv.Itoa(tc.code) + `,"message":"business failure"}}`))
			}}
			srv := gw.start(t)
			raw := bodyQuoteRaw(`"content":"业务错误预览","messageid":"m8","fromuserid":"u-quoted"`, "主消息")

			var got channel.InboundMessage
			ch := &infoflowChannel{
				appID:         "14534",
				appKey:        "app-key",
				appSecret:     "app-secret",
				botUserID:     "4100104388",
				client:        NewClient(srv.Client(), srv.URL),
				quoteEnricher: newQuoteEnricher(NewClient(srv.Client(), srv.URL), discardLogger()),
				logger:        discardLogger(),
				handler: func(_ context.Context, msg channel.InboundMessage) error {
					got = msg
					return nil
				},
			}
			if err := ch.handleFrame(context.Background(), Frame{Method: FrameData, Payload: raw}); err != nil {
				t.Fatalf("handleFrame: %v", err)
			}
			for _, want := range []string{quoteTagPreview, "业务错误预览", "主消息"} {
				if !strings.Contains(got.Text, want) {
					t.Fatalf("Text missing %q: %s", want, got.Text)
				}
			}
			if !strings.Contains(string(got.Raw), `"state":"`+tc.state+`"`) {
				t.Fatalf("Raw must record %s: %s", tc.state, got.Raw)
			}
		})
	}
}

// TestChannelHandleFrameBodyQuoteWithoutMsgIDMakesNoRequest: a body quote with
// preview but no identity must stay preview-only with zero HTTP.
func TestChannelHandleFrameBodyQuoteWithoutMsgIDMakesNoRequest(t *testing.T) {
	gw := &quoteGateway{message: func(_ int, w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"code":"ok","data":{"code":200,"data":{"items":[{"content":"must not fetch"}]}}}`))
	}}
	srv := gw.start(t)
	raw := bodyQuoteRaw(`"content":"只有预览的引用"`, "主消息")

	var got channel.InboundMessage
	ch := &infoflowChannel{
		appID:         "14534",
		appKey:        "app-key",
		appSecret:     "app-secret",
		botUserID:     "4100104388",
		client:        NewClient(srv.Client(), srv.URL),
		quoteEnricher: newQuoteEnricher(NewClient(srv.Client(), srv.URL), discardLogger()),
		logger:        discardLogger(),
		handler: func(_ context.Context, msg channel.InboundMessage) error {
			got = msg
			return nil
		},
	}
	if err := ch.handleFrame(context.Background(), Frame{Method: FrameData, Payload: raw}); err != nil {
		t.Fatalf("handleFrame: %v", err)
	}
	if token, message := gw.calls(); token != 0 || message != 0 {
		t.Fatalf("no HTTP for a quote without msgId, token=%d message=%d", token, message)
	}
	for _, want := range []string{quoteTagPreview, "只有预览的引用", "主消息"} {
		if !strings.Contains(got.Text, want) {
			t.Fatalf("Text missing %q: %s", want, got.Text)
		}
	}
}

// TestProcessFrameLogsRedactedShape is A29: the shape line names types/keys but
// leaks no content, URL or user id.
func TestProcessFrameLogsRedactedShape(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	ch := &infoflowChannel{
		appID:  "14534",
		logger: logger,
		handler: func(_ context.Context, _ channel.InboundMessage) error {
			return nil
		},
	}
	frameWithQuote := json.RawMessage(`{"groupid":"G1","message":{"header":{"fromuserid":"SECRET-USER","toid":"G1","messageid":"M9"},"body":[{"type":"AT","atuserids":["SECRET-USER"]},{"type":"TEXT","content":"SECRET-CONTENT"},{"type":"IMAGE","downloadurl":"https://secret.example/x.png"},{"type":"QUOTE","content":"SECRET-PREVIEW","messageid":"M8","fromuserid":"SECRET-QUOTED","header":{"fromuserid":"SECRET-QUOTED"}}]}}`)
	frameWithReply := json.RawMessage(`{"groupid":"G1","message":{"header":{"fromuserid":"SECRET-USER","toid":"G1","messageid":"M10"},"reply":{"messageid":"M11","preview":"SECRET-REPLY"},"body":[{"type":"TEXT","content":"SECRET-OTHER"}]}}`)
	frameWithReplyData := replyDataRaw(`"content":{"type":"text","text":"SECRET-PREVIEW"},"imid":"SECRET-QUOTED","replyImages":["https://secret.example/q.png"],"sBasemsgId":"SECRET-MSGID"`, "SECRET-OTHER")
	for _, raw := range []json.RawMessage{frameWithQuote, frameWithReply, frameWithReplyData} {
		if err := ch.processFrame(context.Background(), raw); err != nil {
			t.Fatalf("processFrame: %v", err)
		}
	}
	logged := buf.String()
	for _, secret := range []string{"SECRET-CONTENT", "SECRET-PREVIEW", "secret.example", "SECRET-USER", "SECRET-QUOTED", "SECRET-REPLY", "SECRET-OTHER", "SECRET-MSGID"} {
		if strings.Contains(logged, secret) {
			t.Fatalf("log leaked %q: %s", secret, logged)
		}
	}
	lines := strings.Split(strings.TrimSpace(logged), "\n")
	if len(lines) != 3 {
		t.Fatalf("want one shape line per frame, got %d: %s", len(lines), logged)
	}
	var first map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatalf("decode log: %v", err)
	}
	if first["body_types"] == nil || first["body_keys"] == nil || first["quote_element_fields"] == nil {
		t.Fatalf("shape fields missing: %v", first)
	}
	if first["reply_present"] != false || first["quote_present"] != false || first["chat_type"] != "group" {
		t.Fatalf("shape flags wrong: %v", first)
	}
	if fields, _ := first["quote_element_fields"].([]any); len(fields) == 0 {
		t.Fatalf("quote_element_fields empty: %v", first)
	}
	var second map[string]any
	if err := json.Unmarshal([]byte(lines[1]), &second); err != nil {
		t.Fatalf("decode log: %v", err)
	}
	if second["reply_present"] != true {
		t.Fatalf("reply_present must be true: %v", second)
	}
	var third map[string]any
	if err := json.Unmarshal([]byte(lines[2]), &third); err != nil {
		t.Fatalf("decode log: %v", err)
	}
	if third["quote_from_id_source"] != "current_sender" {
		t.Fatalf("quote_from_id_source = %v", third["quote_from_id_source"])
	}
	fields, _ := third["quote_element_fields"].([]any)
	if !containsAny(fields, "sBasemsgId") || containsAny(fields, "SECRET-MSGID") {
		t.Fatalf("REPLYDATA field names missing or value leaked: %v", fields)
	}
}

func containsAny(values []any, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

// TestChannelHandleFrameEnrichesReplyDataQuote is the N93 end-to-end live shape:
// REPLYDATA object content, sBasemsgId query and full text back-fill.
func TestChannelHandleFrameEnrichesReplyDataQuote(t *testing.T) {
	full := strings.Repeat("完整引用正文", 300)
	gw := &quoteGateway{message: func(_ int, w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"code":"ok","data":{"code":200,"message":"ok","data":{"items":[{"msgId":"1881116886473755392","msgType":"app_msg","sendTime":1791987034577,"content":{"type":"text","text":"` + full + `"}}]}}}`))
	}}
	srv := gw.start(t)
	raw := replyDataRaw(`"content":{"type":"text","text":"逐张建成独.."},"imid":"4100111317","replyImages":["https://example.com/q.png"],"sBasemsgId":"1881116886473755392","sMsgId2":"1881116886473755000"`, "能看到这里的内容么？")

	var got channel.InboundMessage
	ch := &infoflowChannel{
		appID:         "14534",
		appKey:        "app-key",
		appSecret:     "app-secret",
		botUserID:     "4100104388",
		client:        NewClient(srv.Client(), srv.URL),
		quoteEnricher: newQuoteEnricher(NewClient(srv.Client(), srv.URL), discardLogger()),
		logger:        discardLogger(),
		handler: func(_ context.Context, msg channel.InboundMessage) error {
			got = msg
			return nil
		},
	}
	if err := ch.handleFrame(context.Background(), Frame{Method: FrameData, Payload: raw}); err != nil {
		t.Fatalf("handleFrame: %v", err)
	}
	for _, want := range []string{quoteTagFull, full, "[IMAGE] https://example.com/q.png", "能看到这里的内容么？"} {
		if !strings.Contains(got.Text, want) {
			t.Fatalf("Text missing %q: %s", want, got.Text)
		}
	}
	if strings.Contains(got.Text, "逐张建成独..") {
		t.Fatalf("preview must be replaced by full text: %s", got.Text)
	}
	if !strings.Contains(string(got.Raw), `"create_time":1791987034577`) {
		t.Fatalf("sendTime must map to origin create_time: %s", got.Raw)
	}
	query := gw.lastQuery
	for key, want := range map[string]string{"fromId": "wangjianghua", "receiverId": "12572761", "receiverType": "2", "msgId": "1881116886473755392"} {
		if query.Get(key) != want {
			t.Fatalf("%s = %q, want %q", key, query.Get(key), want)
		}
	}
}

// TestQuoteMessageDetailContentAndTime covers DEC19: string/object content and
// sendTime/createTime in the same response model.
func TestQuoteMessageDetailContentAndTime(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
		want string
		time int64
	}{
		{name: "string createTime", raw: `{"msgId":"m","content":"字符串全文","createTime":111}`, want: "字符串全文", time: 111},
		{name: "object sendTime", raw: `{"msgId":"m","content":{"type":"text","text":"对象全文"},"sendTime":222}`, want: "对象全文", time: 222},
		{name: "object createTime", raw: `{"msgId":"m","content":{"type":"markdown","text":"对象旧时间"},"createTime":333}`, want: "对象旧时间", time: 333},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var detail quoteMessageDetail
			if err := json.Unmarshal([]byte(tc.raw), &detail); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if detail.Content != tc.want || detail.CreateTime != tc.time {
				t.Fatalf("detail = %+v, want content=%q time=%d", detail, tc.want, tc.time)
			}
		})
	}
}

// TestChannelHandleFramePreserves19DigitQuoteID is DEC23's end-to-end guard:
// JSON number -> json.Number -> exact query, never the float64-rounded value.
func TestChannelHandleFramePreserves19DigitQuoteID(t *testing.T) {
	const exact = "1879034596937550622"
	const rounded = "1879034596937550592"
	gw := &quoteGateway{message: func(_ int, w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("msgId"); got != exact {
			t.Errorf("query msgId = %q, want %q (rounded=%q)", got, exact, rounded)
			_, _ = w.Write([]byte(`{"code":"ok","data":{"code":200,"data":{"items":null}}}`))
			return
		}
		_, _ = w.Write([]byte(`{"code":"ok","data":{"code":200,"data":{"items":[{"msgId":"` + exact + `","msgType":"app_msg","content":{"type":"text","text":"机器人完整引用"}}]}}}`))
	}}
	srv := gw.start(t)
	raw := replyDataRaw(`"content":{"type":"text","text":"机器人引用.."},"sBasemsgId":1879034596937550622`, "问题")

	var got channel.InboundMessage
	ch := &infoflowChannel{
		appID: "14534", appKey: "app-key", appSecret: "app-secret", botUserID: "4100104388",
		client: NewClient(srv.Client(), srv.URL), quoteEnricher: newQuoteEnricher(NewClient(srv.Client(), srv.URL), discardLogger()), logger: discardLogger(),
		handler: func(_ context.Context, msg channel.InboundMessage) error { got = msg; return nil },
	}
	if err := ch.handleFrame(context.Background(), Frame{Method: FrameData, Payload: raw}); err != nil {
		t.Fatalf("handleFrame: %v", err)
	}
	if got.ReplyTo == nil || got.ReplyTo.MessageID != exact {
		t.Fatalf("ReplyTo.MessageID = %+v, want exact %s", got.ReplyTo, exact)
	}
	if query := gw.lastQuery.Get("msgId"); query != exact {
		t.Fatalf("actual query msgId = %q, want %q", query, exact)
	} else if query == rounded {
		t.Fatalf("actual query msgId is the float64-rounded value %q", query)
	}
	if !strings.Contains(got.Text, quoteTagFull) || !strings.Contains(got.Text, "机器人完整引用") {
		t.Fatalf("full text was not rendered: %s", got.Text)
	}
	if strings.Contains(got.Text, rounded) {
		t.Fatalf("rounded id leaked into output: %s", got.Text)
	}
}

// TestChannelHandleFrameRetriesSMsgID2OnNotFound is DEC24: first not_found,
// second candidate ok => full text, attempts=2 and matched=smsgid2.
func TestChannelHandleFrameRetriesSMsgID2OnNotFound(t *testing.T) {
	var mu sync.Mutex
	var ids []string
	gw := &quoteGateway{message: func(call int, w http.ResponseWriter, r *http.Request) {
		id := r.URL.Query().Get("msgId")
		mu.Lock()
		ids = append(ids, id)
		mu.Unlock()
		if call == 1 {
			_, _ = w.Write([]byte(`{"code":"ok","data":{"code":200,"data":{"items":null}}}`))
			return
		}
		_, _ = w.Write([]byte(`{"code":"ok","data":{"code":200,"data":{"items":[{"msgId":"ALT","content":{"type":"text","text":"sMsgId2 命中全文"}}]}}}`))
	}}
	srv := gw.start(t)
	raw := replyDataRaw(`"content":{"type":"text","text":"base 预览.."},"sBasemsgId":"BASE","sMsgId2":"ALT"`, "问题")

	var got channel.InboundMessage
	ch := &infoflowChannel{
		appID: "14534", appKey: "app-key", appSecret: "app-secret", botUserID: "4100104388",
		client: NewClient(srv.Client(), srv.URL), quoteEnricher: newQuoteEnricher(NewClient(srv.Client(), srv.URL), discardLogger()), logger: discardLogger(),
		handler: func(_ context.Context, msg channel.InboundMessage) error { got = msg; return nil },
	}
	if err := ch.handleFrame(context.Background(), Frame{Method: FrameData, Payload: raw}); err != nil {
		t.Fatalf("handleFrame: %v", err)
	}
	mu.Lock()
	gotIDs := append([]string(nil), ids...)
	mu.Unlock()
	if len(gotIDs) != 2 || gotIDs[0] != "BASE" || gotIDs[1] != "ALT" {
		t.Fatalf("candidate order = %v, want [BASE ALT]", gotIDs)
	}
	if !strings.Contains(got.Text, quoteTagFull) || !strings.Contains(got.Text, "sMsgId2 命中全文") {
		t.Fatalf("fallback full text missing: %s", got.Text)
	}
	if !strings.Contains(string(got.Raw), `"matched":"smsgid2"`) || !strings.Contains(string(got.Raw), `"attempts":2`) {
		t.Fatalf("origin missing matched/attempts: %s", got.Raw)
	}
}

// TestChannelHandleFrameBothCandidatesNotFound: both not_found => two requests,
// truncated preview, attempts=2.
func TestChannelHandleFrameBothCandidatesNotFound(t *testing.T) {
	gw := &quoteGateway{message: func(_ int, w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"code":"ok","data":{"code":200,"data":{"items":null}}}`))
	}}
	srv := gw.start(t)
	raw := replyDataRaw(`"content":{"type":"text","text":"两次都没有.."},"sBasemsgId":"BASE","sMsgId2":"ALT"`, "主消息")

	var got channel.InboundMessage
	ch := &infoflowChannel{
		appID: "14534", appKey: "app-key", appSecret: "app-secret", botUserID: "4100104388",
		client: NewClient(srv.Client(), srv.URL), quoteEnricher: newQuoteEnricher(NewClient(srv.Client(), srv.URL), discardLogger()), logger: discardLogger(),
		handler: func(_ context.Context, msg channel.InboundMessage) error { got = msg; return nil },
	}
	if err := ch.handleFrame(context.Background(), Frame{Method: FrameData, Payload: raw}); err != nil {
		t.Fatalf("handleFrame: %v", err)
	}
	if _, message := gw.calls(); message != 2 {
		t.Fatalf("both candidates must be attempted, message calls=%d", message)
	}
	if !strings.Contains(got.Text, quoteTagTruncated) || !strings.Contains(got.Text, "两次都没有..") {
		t.Fatalf("truncated preview missing: %s", got.Text)
	}
	if !strings.Contains(string(got.Raw), `"state":"not_found"`) || !strings.Contains(string(got.Raw), `"attempts":2`) {
		t.Fatalf("origin must record two not_found attempts: %s", got.Raw)
	}
	if strings.Contains(string(got.Raw), `"matched"`) {
		t.Fatalf("not_found must not claim a matched candidate: %s", got.Raw)
	}
}

// TestChannelHandleFrameSameCandidateSkipped: duplicate ids make one request.
func TestChannelHandleFrameSameCandidateSkipped(t *testing.T) {
	gw := &quoteGateway{message: func(_ int, w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"code":"ok","data":{"code":200,"data":{"items":[{"content":{"type":"text","text":"同 id 全文"}}]}}}`))
	}}
	srv := gw.start(t)
	raw := replyDataRaw(`"content":{"type":"text","text":"同 id.."},"sBasemsgId":"SAME","sMsgId2":"SAME"`, "主消息")

	var got channel.InboundMessage
	ch := &infoflowChannel{
		appID: "14534", appKey: "app-key", appSecret: "app-secret", botUserID: "4100104388",
		client: NewClient(srv.Client(), srv.URL), quoteEnricher: newQuoteEnricher(NewClient(srv.Client(), srv.URL), discardLogger()), logger: discardLogger(),
		handler: func(_ context.Context, msg channel.InboundMessage) error { got = msg; return nil },
	}
	if err := ch.handleFrame(context.Background(), Frame{Method: FrameData, Payload: raw}); err != nil {
		t.Fatalf("handleFrame: %v", err)
	}
	if _, message := gw.calls(); message != 1 {
		t.Fatalf("duplicate ids must make one request, message calls=%d", message)
	}
	if !strings.Contains(string(got.Raw), `"attempts":1`) || !strings.Contains(string(got.Raw), `"matched":"sbasemsgid"`) {
		t.Fatalf("origin = %s", got.Raw)
	}
}

// TestQuoteEnricherRetryStopsOnDeniedOrTimeout locks the no-retry boundary.
func TestQuoteEnricherRetryStopsOnDeniedOrTimeout(t *testing.T) {
	t.Run("denied", func(t *testing.T) {
		gw := &quoteGateway{message: func(_ int, w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"code":"ok","data":{"code":720001,"message":"denied"}}`))
		}}
		srv := gw.start(t)
		enricher := newQuoteEnricher(NewClient(srv.Client(), srv.URL), discardLogger())
		req := testQuoteRequest("BASE")
		req.Candidates = []quoteIDCandidate{{Field: "sbasemsgid", ID: "BASE"}, {Field: "smsgid2", ID: "ALT"}}
		origin := enricher.Enrich(context.Background(), req)
		if origin.State != quoteStateDenied || origin.Attempts != 1 {
			t.Fatalf("origin = %+v, want denied attempts=1", origin)
		}
		if _, message := gw.calls(); message != 1 {
			t.Fatalf("denied must not retry, message calls=%d", message)
		}
	})
	t.Run("timeout", func(t *testing.T) {
		gw := &quoteGateway{message: func(_ int, w http.ResponseWriter, _ *http.Request) {
			time.Sleep(200 * time.Millisecond)
			_, _ = w.Write([]byte(`{"code":"ok","data":{"content":"late"}}`))
		}}
		srv := gw.start(t)
		enricher := newQuoteEnricher(NewClient(srv.Client(), srv.URL), discardLogger())
		enricher.timeout = 50 * time.Millisecond
		req := testQuoteRequest("BASE")
		req.Candidates = []quoteIDCandidate{{Field: "sbasemsgid", ID: "BASE"}, {Field: "smsgid2", ID: "ALT"}}
		origin := enricher.Enrich(context.Background(), req)
		if origin.State != quoteStateTimeout || origin.Attempts != 1 {
			t.Fatalf("origin = %+v, want timeout attempts=1", origin)
		}
		if _, message := gw.calls(); message != 1 {
			t.Fatalf("timeout must not retry, message calls=%d", message)
		}
	})
}

// TestProcessFrameLogsQuoteIDMetadata is DEC25: candidate diagnostics contain
// field/type/length/hash only, never the 19-digit id itself.
func TestProcessFrameLogsQuoteIDMetadata(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	ch := &infoflowChannel{
		appID:  "14534",
		logger: logger,
		handler: func(_ context.Context, _ channel.InboundMessage) error {
			return nil
		},
	}
	raw := replyDataRaw(`"content":{"type":"text","text":"secret preview"},"sBasemsgId":1879034596937550622,"sMsgId2":"SECRET-ALT"`, "secret own")
	if err := ch.processFrame(context.Background(), raw); err != nil {
		t.Fatalf("processFrame: %v", err)
	}
	logged := buf.String()
	if strings.Contains(logged, "1879034596937550622") || strings.Contains(logged, "SECRET-ALT") {
		t.Fatalf("candidate id leaked: %s", logged)
	}
	var entry map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(logged)), &entry); err != nil {
		t.Fatalf("decode log: %v", err)
	}
	candidates, ok := entry["quote_id_candidates"].([]any)
	if !ok || len(candidates) != 2 {
		t.Fatalf("quote_id_candidates = %#v", entry["quote_id_candidates"])
	}
	wantFields := []string{"sBasemsgId", "sMsgId2"}
	wantTypes := []string{"number", "string"}
	wantLengths := []int{19, 10}
	for i, rawCandidate := range candidates {
		candidate, ok := rawCandidate.(map[string]any)
		if !ok {
			t.Fatalf("candidate %d = %#v", i, rawCandidate)
		}
		if candidate["field"] != wantFields[i] || candidate["json_type"] != wantTypes[i] || candidate["length"] != float64(wantLengths[i]) || candidate["sha256_8"] == "" {
			t.Fatalf("candidate %d = %#v", i, candidate)
		}
	}
	if got := entry["quote_id_candidates"].([]any)[0].(map[string]any)["sha256_8"]; got != quoteIDFingerprint("1879034596937550622") {
		t.Fatalf("hash = %v, want %s", got, quoteIDFingerprint("1879034596937550622"))
	}
}

// TestQuoteIDValueRejectsFloat64 guards the exact-id path against the original
// 19-digit bug: a rounded float64 must never become a candidate.
func TestQuoteIDValueRejectsFloat64(t *testing.T) {
	if id := quoteIDString(float64(1879034596937550622)); id != "" {
		t.Fatalf("float64 must not become an id, got %q", id)
	}
	if id, typ := quoteIDValue(json.Number("1879034596937550622")); id != "1879034596937550622" || typ != "number" {
		t.Fatalf("json.Number = %q/%q", id, typ)
	}
}
