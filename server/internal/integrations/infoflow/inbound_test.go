package infoflow

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/integrations/channel"
)

func TestInboundPrivateMessage(t *testing.T) {
	raw := `{"FromUserId":"u1","FromUserName":"alice","Content":"hello","MsgId":"m1","MsgType":"text"}`
	msg, ok := inboundFromPayload(json.RawMessage(raw), "14534", "bot")
	if !ok {
		t.Fatal("expected ok")
	}
	if msg.Source.ChatType != channel.ChatTypeP2P || msg.Source.SenderID != "u1" || msg.MessageID != "m1" || strings.TrimSpace(msg.Text) != "hello" {
		t.Fatalf("unexpected normalized private message: %+v", msg)
	}
}

func TestInboundGroupAtBot(t *testing.T) {
	raw := `{"agentid":14534,"eventtype":"MESSAGE_RECEIVE","groupid":"123","message":{"header":{"fromuserid":"u2","toid":"123","totype":"GROUP","msgtype":"TEXT","messageid":"gm1"},"body":[{"type":"AT","userid":"bot"},{"type":"TEXT","content":"帮我查状态"}]}}`
	msg, ok := inboundFromPayload(json.RawMessage(raw), "14534", "bot")
	if !ok {
		t.Fatalf("expected ok, msg=%+v", msg)
	}
	if msg.Source.ChatType != channel.ChatTypeGroup || msg.Source.ChatID != "123" || !msg.AddressedToBot || strings.TrimSpace(msg.Text) != "帮我查状态" {
		t.Fatalf("unexpected normalized group message: %+v", msg)
	}
}

// TestInboundGroupQuoteKeepsImageURL: a quote/reply whose body is an image
// must land in Text as a URL, while CommandText stays the sender's own line.
func TestInboundGroupQuoteKeepsImageURL(t *testing.T) {
	raw := `{"groupid":"12572761","message":{"header":{"fromuserid":"wangjianghua","totype":"GROUP","msgtype":"MIXED","messageid":"m9"},"reply":{"messageid":"m8","preview":"看下这张图","body":[{"type":"IMAGE","downloadurl":"https://example.com/a.png"},{"type":"TEXT","content":"说明"}]},"body":[{"type":"AT","atuserids":["4100104388"]},{"type":"TEXT","content":"这张图是什么"}]}}`
	msg, ok := inboundFromPayload(json.RawMessage(raw), "8682", "4100104388")
	if !ok {
		t.Fatal("expected ok")
	}
	if !msg.HasSelectedContext || !msg.AddressedToBot {
		t.Fatalf("quote must be selected context and addressed: %+v", msg)
	}
	if msg.CommandText != "这张图是什么" {
		t.Fatalf("CommandText = %q", msg.CommandText)
	}
	for _, want := range []string{"看下这张图", "https://example.com/a.png", "[IMAGE]", "说明", "这张图是什么"} {
		if !strings.Contains(msg.Text, want) {
			t.Fatalf("Text missing %q: %s", want, msg.Text)
		}
	}
	if msg.ReplyTo == nil || msg.ReplyTo.MessageID != "m8" {
		t.Fatalf("ReplyTo = %+v", msg.ReplyTo)
	}
}

// TestFlattenQuotedReplacesPreviewWithOriginText: a resolved lookup replaces the
// truncated preview with the full original text (A25a).
func TestFlattenQuotedReplacesPreviewWithOriginText(t *testing.T) {
	full := strings.Repeat("完整原文", 300)
	message := &infoflowEventMessage{Reply: &infoflowQuote{MessageID: "m8", Preview: "完整原文完整…"}}
	got := flattenQuoted(message, &quoteOrigin{QueryID: "m8", State: quoteStateOK, Source: "api", Text: full})
	if !strings.Contains(got, quoteTagFull) {
		t.Fatalf("missing full-text label: %s", got)
	}
	if !strings.Contains(got, full) {
		t.Fatalf("missing full text: %s", got)
	}
	if strings.Contains(got, "完整原文完整…") {
		t.Fatalf("preview must be dropped when the full text is available: %s", got)
	}
}

// TestFlattenQuotedKeepsImageURLFromBody: getSingleMsg returns no media URL, so
// the quoted image row must still come from the inbound body (A25b).
func TestFlattenQuotedKeepsImageURLFromBody(t *testing.T) {
	message := &infoflowEventMessage{Reply: &infoflowQuote{
		MessageID: "m8",
		Body: []map[string]any{
			{"type": "IMAGE", "downloadurl": "https://example.com/q.png"},
			{"type": "TEXT", "content": "图片说明"},
		},
	}}
	got := flattenQuoted(message, &quoteOrigin{QueryID: "m8", State: quoteStateOK, Source: "api", MsgType: "image"})
	for _, want := range []string{quoteTagFull + "[image]", "[IMAGE] https://example.com/q.png", "图片说明"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in %s", want, got)
		}
	}
}

// TestFlattenQuotedNoMessageIDUsesPreviewLabel: without an id there is nothing to
// look up, so the preview is kept with an explicit label instead of silently
// pretending to be complete (A25c).
func TestFlattenQuotedNoMessageIDUsesPreviewLabel(t *testing.T) {
	message := &infoflowEventMessage{Reply: &infoflowQuote{Preview: "只有预览的引用"}}
	got := flattenQuoted(message, &quoteOrigin{State: quoteStateSkipped})
	if !strings.Contains(got, quoteTagPreview) || !strings.Contains(got, "只有预览的引用") {
		t.Fatalf("got %q, want labelled preview", got)
	}
}

// TestFlattenQuotedNotFoundUsesTruncatedLabel: a withdrawn/expired original can
// never be recovered; say so explicitly.
func TestFlattenQuotedNotFoundUsesTruncatedLabel(t *testing.T) {
	message := &infoflowEventMessage{Reply: &infoflowQuote{MessageID: "m8", Preview: "已撤回的引用"}}
	got := flattenQuoted(message, &quoteOrigin{QueryID: "m8", State: quoteStateNotFound})
	if !strings.Contains(got, quoteTagTruncated) || !strings.Contains(got, "已撤回的引用") {
		t.Fatalf("got %q, want truncated label", got)
	}
}

// TestFlattenQuotedNestedQuoteOneLevel: a quote inside a quote is expanded once
// and its inner text is not duplicated (R64).
func TestFlattenQuotedNestedQuoteOneLevel(t *testing.T) {
	message := &infoflowEventMessage{Reply: &infoflowQuote{
		MessageID: "m8",
		Body: []map[string]any{{
			"type":    "QUOTE",
			"content": "内层引用",
			"body": []map[string]any{
				{"type": "TEXT", "content": "内层正文"},
			},
		}},
	}}
	got := flattenQuoted(message, &quoteOrigin{QueryID: "m8", State: quoteStateOK, Source: "api", Text: "外层全文"})
	if strings.Count(got, "内层正文") != 1 {
		t.Fatalf("nested quote expanded more than once: %s", got)
	}
	if !strings.Contains(got, "外层全文") {
		t.Fatalf("outer full text missing: %s", got)
	}
}

// TestInboundPrivateQuoteIsSelectedContext: the single-chat path now parses the
// top-level quote and sets the reply coordinates (H2′).
func TestInboundPrivateQuoteIsSelectedContext(t *testing.T) {
	raw := `{"agentid":"14534","FromUserId":"u1","FromUserName":"alice","Content":"接着说","MsgId":"m1","MsgType":"text","quote":{"messageid":"m0","preview":"被引用的内容"}}`
	msg, ok := inboundFromPayload(json.RawMessage(raw), "14534", "bot")
	if !ok {
		t.Fatal("expected ok")
	}
	if !msg.HasSelectedContext || msg.ReplyTo == nil || msg.ReplyTo.MessageID != "m0" {
		t.Fatalf("private quote not parsed: %+v", msg)
	}
	if !strings.Contains(msg.Text, "被引用的内容") || !strings.Contains(msg.Text, "接着说") {
		t.Fatalf("Text = %q", msg.Text)
	}
	if msg.CommandText != "接着说" {
		t.Fatalf("CommandText = %q", msg.CommandText)
	}

	var payload infoflowRaw
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		t.Fatal(err)
	}
	query, ok := quoteQueryFor(msg, payload, quoteForPayload(payload))
	if !ok {
		t.Fatal("expected a lookup query")
	}
	if query.ReceiverType != 7 || query.ReceiverID != "14534" || query.MsgID != "m0" || query.FromID != "u1" {
		t.Fatalf("query = %+v", query)
	}
}

// TestInboundPrivateWithoutQuoteIsUnchanged: the no-quote shape must keep the
// pre-N80 output byte for byte (A27).
func TestInboundPrivateWithoutQuoteIsUnchanged(t *testing.T) {
	raw := `{"FromUserId":"u1","FromUserName":"alice","Content":"hello","MsgId":"m1","MsgType":"text"}`
	msg, ok := inboundFromPayload(json.RawMessage(raw), "14534", "bot")
	if !ok {
		t.Fatal("expected ok")
	}
	if msg.Text != "hello" || msg.CommandText != "hello" {
		t.Fatalf("text = %q command = %q", msg.Text, msg.CommandText)
	}
	if msg.HasSelectedContext || msg.ReplyTo != nil {
		t.Fatalf("no-quote private message must not gain quote metadata: %+v", msg)
	}
	if msg.EventID != "m1" || msg.MessageID != "m1" || msg.Source.ChatID != "u1" || msg.Source.ChatType != channel.ChatTypeP2P {
		t.Fatalf("regression: %+v", msg)
	}
}

// TestQuoteQueryGroupFromIDFallsBackToQuoteHeader: the quoted author travels in
// reply.header; the current sender is only the last resort (N79 §5.4).
func TestQuoteQueryGroupFromIDFallsBackToQuoteHeader(t *testing.T) {
	raw := `{"groupid":"12572761","message":{"header":{"fromuserid":"sender","toid":"12572761","totype":"GROUP","messageid":"m9"},"reply":{"messageid":"m8","header":{"fromuserid":"quoted-author"},"preview":"x"},"body":[{"type":"TEXT","content":"hi"}]}}`
	msg, ok := inboundFromPayload(json.RawMessage(raw), "14534", "bot")
	if !ok {
		t.Fatal("expected ok")
	}
	var payload infoflowRaw
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		t.Fatal(err)
	}
	query, ok := quoteQueryFor(msg, payload, quoteForPayload(payload))
	if !ok {
		t.Fatal("expected query")
	}
	if query.FromID != "quoted-author" || query.ReceiverType != 2 || query.ReceiverID != "12572761" || query.MsgID != "m8" {
		t.Fatalf("query = %+v", query)
	}
}

// TestPrefixPrivateQuoteShapes covers both p2p shapes: a quote inside message and
// a top-level quote merged onto an existing message.
func TestPrefixPrivateQuoteShapes(t *testing.T) {
	nested := `{"agentid":"14534","FromUserId":"u1","Content":"hi","MsgId":"m1","message":{"quote":{"messageid":"m0","preview":"消息内引用"}}}`
	msg, ok := inboundFromPayload(json.RawMessage(nested), "14534", "bot")
	if !ok || !msg.HasSelectedContext || msg.ReplyTo == nil || msg.ReplyTo.MessageID != "m0" {
		t.Fatalf("message.quote shape not parsed: %+v", msg)
	}

	merged := `{"agentid":"14534","FromUserId":"u1","Content":"hi","MsgId":"m2","message":{"header":{"fromuserid":"u1"}},"quote":{"messageid":"m0","preview":"顶层引用"}}`
	msg, ok = inboundFromPayload(json.RawMessage(merged), "14534", "bot")
	if !ok || !msg.HasSelectedContext || !strings.Contains(msg.Text, "顶层引用") {
		t.Fatalf("top-level quote merge not parsed: %+v", msg)
	}
}
