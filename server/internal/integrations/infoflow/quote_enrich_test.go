package infoflow

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
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
		t.Fatalf("no HTTP expected, token=%d message=%d", token, message)
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
	raw := json.RawMessage(`{"groupid":"12572761","message":{"header":{"fromuserid":"wangjianghua","toid":"12572761","totype":"GROUP","msgtype":"MIXED","messageid":"m9"},"reply":{"messageid":"m8","preview":"引用正文这是一段被截…","body":[{"type":"IMAGE","downloadurl":"https://example.com/q.png"}]},"body":[{"type":"AT","atuserids":["4100104388"]},{"type":"TEXT","content":"帮我看看这张图"}]}}`)

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
	raw := json.RawMessage(`{"groupid":"12572761","message":{"header":{"fromuserid":"wangjianghua","toid":"12572761","totype":"GROUP","msgtype":"TEXT","messageid":"m9"},"reply":{"messageid":"m8","preview":"被引用的预览文本"},"body":[{"type":"AT","atuserids":["4100104388"]},{"type":"TEXT","content":"原消息"}]}}`)

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
	raw := json.RawMessage(`{"agentid":"14534","FromUserId":"u1","Content":"接着上面的问题","MsgId":"m1","MsgType":"text","quote":{"messageid":"m0","preview":"单聊引用的预览…"}}`)

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
	raw := json.RawMessage(`{"FromUserId":"u1","Content":"问题","MsgId":"m1","quote":{"messageid":"m0","preview":"预览…"}}`)

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
