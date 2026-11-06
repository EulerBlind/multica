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
	raw := `{"agentid":"14534","FromUserId":"u1","FromUserName":"alice","Content":"接着说","MsgId":"m1","MsgType":"text","quote":{"messageid":"m0","header":{"fromuserid":"u-quoted"},"preview":"被引用的内容"}}`
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
	if query.ReceiverType != 7 || query.ReceiverID != "14534" || query.MsgID != "m0" || query.FromID != "u-quoted" {
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

// TestFlattenQuotedDeduplicatesBodyText is DEC9: once the full text is resolved,
// body TEXT lines repeating the preview/content/full text (including prefix
// relations) are dropped, while image rows stay untouched.
func TestFlattenQuotedDeduplicatesBodyText(t *testing.T) {
	full := strings.Repeat("完整原文", 50)
	quote := &infoflowQuote{
		MessageID: "m8",
		Preview:   full[:20] + "…",
		Content:   full[:30],
		Body: []map[string]any{
			{"type": "TEXT", "content": full[:12] + "…"},
			{"type": "TEXT", "content": full},
			{"type": "IMAGE", "downloadurl": "https://example.com/a.png"},
		},
	}
	got := flattenQuoted(&infoflowEventMessage{Reply: quote}, &quoteOrigin{QueryID: "m8", State: quoteStateOK, Source: "api", Text: full})
	if n := strings.Count(got, "完整原文"); n != 50 {
		t.Fatalf("body text duplicated the full text: count=%d\n%s", n, got)
	}
	if n := strings.Count(got, "[IMAGE] https://example.com/a.png"); n != 1 {
		t.Fatalf("image row must appear exactly once, count=%d", n)
	}
	if n := strings.Count(got, quoteTagFull); n != 1 {
		t.Fatalf("full-text label must appear once, count=%d", n)
	}
	if strings.Contains(got, full[:20]+"…") {
		t.Fatalf("preview must not survive a successful lookup: %s", got)
	}
}

// TestFlattenQuotedKeepsBodyTextWithoutOrigin protects the pre-lookup path: body
// TEXT is still useful when the original was never resolved.
func TestFlattenQuotedKeepsBodyTextWithoutOrigin(t *testing.T) {
	quote := &infoflowQuote{
		MessageID: "m8",
		Preview:   "预览",
		Body:      []map[string]any{{"type": "TEXT", "content": "body 说明"}},
	}
	got := flattenQuoted(&infoflowEventMessage{Reply: quote}, &quoteOrigin{QueryID: "m8", State: quoteStateDenied})
	if !strings.Contains(got, "body 说明") {
		t.Fatalf("body text must survive an unresolved lookup: %s", got)
	}
}

// TestInboundGroupBodyQuoteIsSelectedContext is the N88 live shape: the selected
// quote arrives as a body element, not as message.reply/quote. It must produce
// the quoted envelope, set selected context and keep the sender's own text.
func TestInboundGroupBodyQuoteIsSelectedContext(t *testing.T) {
	raw := `{"groupid":"12572761","message":{"header":{"fromuserid":"wangjianghua","toid":"12572761","totype":"GROUP","msgtype":"MIXED","messageid":"m9"},"body":[{"type":"AT","atuserids":["4100104388"]},{"type":"QUOTE","content":"逐张建成独..","messageid":"m8","fromuserid":"u-quoted","body":[{"type":"IMAGE","downloadurl":"https://example.com/q.png"}]},{"type":"TEXT","content":"能看到这里的内容么？"}]}}`
	msg, ok := inboundFromPayload(json.RawMessage(raw), "8682", "4100104388")
	if !ok {
		t.Fatal("expected ok")
	}
	if !msg.HasSelectedContext {
		t.Fatalf("body quote must set HasSelectedContext: %+v", msg)
	}
	if msg.ReplyTo == nil || msg.ReplyTo.MessageID != "m8" {
		t.Fatalf("ReplyTo = %+v, want m8", msg.ReplyTo)
	}
	if !strings.Contains(msg.Text, "<quoted_message>") || !strings.Contains(msg.Text, "</quoted_message>") {
		t.Fatalf("missing quoted envelope: %s", msg.Text)
	}
	for _, want := range []string{"逐张建成独..", "[IMAGE] https://example.com/q.png", "能看到这里的内容么？"} {
		if !strings.Contains(msg.Text, want) {
			t.Fatalf("Text missing %q: %s", want, msg.Text)
		}
	}
	if strings.Count(msg.Text, "逐张建成独..") != 1 {
		t.Fatalf("quote preview must not be duplicated in own text: %s", msg.Text)
	}
	if msg.CommandText != "能看到这里的内容么？" {
		t.Fatalf("CommandText = %q", msg.CommandText)
	}
}

// TestInboundGroupBodyQuoteToleratesNestedFieldNames covers DEC15: the live
// field names are unknown, so a nested quote object with camelCase ids works.
func TestInboundGroupBodyQuoteToleratesNestedFieldNames(t *testing.T) {
	raw := `{"groupid":"G1","message":{"header":{"fromuserid":"U1","toid":"G1","totype":"GROUP","messageid":"M9"},"body":[{"type":"quote","quote":{"messageId":"M8","fromUserId":"U-QUOTED","content":"嵌套预览"},"body":[{"type":"TEXT","content":"正文"}]}]}}`
	msg, ok := inboundFromPayload(json.RawMessage(raw), "8682", "BOT")
	if !ok {
		t.Fatalf("expected ok: %+v", msg)
	}
	if !msg.HasSelectedContext || msg.ReplyTo == nil || msg.ReplyTo.MessageID != "M8" {
		t.Fatalf("nested quote not parsed: %+v", msg)
	}
	if strings.Count(msg.Text, "嵌套预览") != 1 || !strings.Contains(msg.Text, "正文") {
		t.Fatalf("Text = %q", msg.Text)
	}
	if quote := quoteForPayload(infoflowRaw{Message: &infoflowEventMessage{Body: []map[string]any{{"type": "QUOTE", "messageid": "M8", "fromuserid": "U-QUOTED"}}}}); quote == nil || quoteFromID(quote) != "U-QUOTED" {
		t.Fatalf("quote sender not extracted: %+v", quote)
	}
}

// TestInboundGroupBodyQuoteWithoutIDsKeepsPreview: no messageid/fromuserid means
// no lookup (DEC6), but the preview and the main text must survive.
func TestInboundGroupBodyQuoteWithoutIDsKeepsPreview(t *testing.T) {
	raw := `{"groupid":"G1","message":{"header":{"fromuserid":"U1","toid":"G1","messageid":"M9"},"body":[{"type":"AT","atuserids":["BOT"]},{"type":"REFERENCE","content":"只有截断预览.."},{"type":"TEXT","content":"我的问题"}]}}`
	msg, ok := inboundFromPayload(json.RawMessage(raw), "8682", "BOT")
	if !ok {
		t.Fatal("expected ok")
	}
	if !msg.HasSelectedContext || msg.ReplyTo != nil {
		t.Fatalf("preview-only quote state: %+v", msg)
	}
	for _, want := range []string{"<quoted_message>", "只有截断预览..", "我的问题"} {
		if !strings.Contains(msg.Text, want) {
			t.Fatalf("Text missing %q: %s", want, msg.Text)
		}
	}
}

// TestInboundGroupBodyQuoteDoesNotAddressBot: a quote element is not an @ or a
// reply to the bot, so ordinary group chatter stays unaddressed (fail closed).
func TestInboundGroupBodyQuoteDoesNotAddressBot(t *testing.T) {
	raw := `{"groupid":"G1","message":{"header":{"fromuserid":"U1","toid":"G1","messageid":"M9"},"body":[{"type":"QUOTE","content":"引用","messageid":"M8","fromuserid":"U2"},{"type":"TEXT","content":"普通群聊"}]}}`
	msg, ok := inboundFromPayload(json.RawMessage(raw), "8682", "BOT")
	if !ok {
		t.Fatal("expected ok")
	}
	if msg.AddressedToBot {
		t.Fatalf("body quote must not address the bot: %+v", msg)
	}
}

// TestInboundGroupBodyQuoteToBotAddresses: quoting the bot itself is a reply to
// the bot and must not need a separate AT.
func TestInboundGroupBodyQuoteToBotAddresses(t *testing.T) {
	raw := `{"groupid":"G1","message":{"header":{"fromuserid":"U1","toid":"G1","messageid":"M9"},"body":[{"type":"QUOTE","content":"机器人上一条","messageid":"M8","fromuserid":"BOT"},{"type":"TEXT","content":"继续"}]}}`
	msg, ok := inboundFromPayload(json.RawMessage(raw), "8682", "BOT")
	if !ok {
		t.Fatal("expected ok")
	}
	if !msg.AddressedToBot {
		t.Fatalf("quote of the bot must address it: %+v", msg)
	}
}

// TestFlattenBodyTextBranches covers every element branch plus the own-vs-quoted
// quote policy introduced by N88.
func TestFlattenBodyTextBranches(t *testing.T) {
	body := []map[string]any{
		nil,
		{"type": "AT"},
		{"type": "TEXT", "content": "文本"},
		{"type": "IMAGE", "downloadurl": "https://example.com/i.png"},
		{"type": "IMAGE"},
		{"type": "FILE", "name": "f.txt", "url": "https://example.com/f.txt"},
		{"type": "FILE"},
		{"type": "LINK", "label": "链接"},
		{"type": "QUOTE", "content": "引用内容", "body": []map[string]any{{"type": "TEXT", "content": "内层"}}},
		{"type": "REPLY", "content": "引用内容"},
		{"type": "UNKNOWN", "content": "未知"},
	}
	own := flattenOwnText(body)
	for _, want := range []string{"文本", "[IMAGE] https://example.com/i.png", "[IMAGE]", "[FILE: f.txt]", "[FILE: ]", "链接: 链接", "未知"} {
		if !strings.Contains(own, want) {
			t.Fatalf("own missing %q: %s", want, own)
		}
	}
	for _, unwanted := range []string{"引用内容", "内层"} {
		if strings.Contains(own, unwanted) {
			t.Fatalf("own must skip quote elements, found %q: %s", unwanted, own)
		}
	}
	full := flattenGroupText(body)
	for _, want := range []string{"引用内容", "内层"} {
		if !strings.Contains(full, want) {
			t.Fatalf("quoted body missing %q: %s", want, full)
		}
	}
}

// TestScalarStringBranches covers the JSON scalar shapes used by DEC15 field
// extraction.
func TestScalarStringBranches(t *testing.T) {
	for _, tc := range []struct {
		in   any
		want string
	}{
		{in: " text ", want: "text"},
		{in: float64(12572761), want: "12572761"},
		{in: 42, want: "42"},
		{in: json.Number("123"), want: "123"},
		{in: nil, want: ""},
		{in: map[string]any{"x": 1}, want: ""},
	} {
		if got := scalarString(tc.in); got != tc.want {
			t.Fatalf("scalarString(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestInboundGroupBodyQuoteIDOnlySelectsContext: a body quote with only
// messageid/fromuserid and no preview must still enter the lookup path.
func TestInboundGroupBodyQuoteIDOnlySelectsContext(t *testing.T) {
	raw := `{"groupid":"G1","message":{"header":{"fromuserid":"U1","toid":"G1","messageid":"M9"},"body":[{"type":"AT","atuserids":["BOT"]},{"type":"QUOTE","messageid":"M8","fromuserid":"U-QUOTED"},{"type":"TEXT","content":"我的问题"}]}}`
	msg, ok := inboundFromPayload(json.RawMessage(raw), "8682", "BOT")
	if !ok {
		t.Fatal("expected ok")
	}
	if !msg.HasSelectedContext || msg.ReplyTo == nil || msg.ReplyTo.MessageID != "M8" {
		t.Fatalf("id-only body quote must be selected: %+v", msg)
	}
	if strings.Contains(msg.Text, "<quoted_message>") {
		t.Fatalf("no preview yet, envelope should appear only after lookup: %s", msg.Text)
	}
}
