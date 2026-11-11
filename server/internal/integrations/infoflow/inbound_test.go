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
