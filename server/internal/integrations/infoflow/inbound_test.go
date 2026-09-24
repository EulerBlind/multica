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
