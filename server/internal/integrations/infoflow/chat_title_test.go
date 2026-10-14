package infoflow

import (
	"testing"

	"github.com/multica-ai/multica/server/internal/integrations/channel"
)

func TestInfoflowChatTitle(t *testing.T) {
	p2p := infoflowChatTitle(channel.InboundMessage{Source: channel.Source{
		ChatType: channel.ChatTypeP2P, ChatID: "liubo53", SenderID: "liubo53", SenderStableID: "李博",
	}})
	if p2p != "李博的聊天" {
		t.Fatalf("p2p display = %q", p2p)
	}
	account := infoflowChatTitle(channel.InboundMessage{Source: channel.Source{
		ChatType: channel.ChatTypeP2P, SenderID: "v_liuchunqi01",
	}})
	if account != "v_liuchunqi01的聊天" {
		t.Fatalf("p2p account = %q", account)
	}
	group := infoflowChatTitle(channel.InboundMessage{Source: channel.Source{
		ChatType: channel.ChatTypeGroup, ChatID: "12563594", SenderID: "v_liuchunqi01",
	}})
	if group != "12563594-v_liuchunqi01的聊天" {
		t.Fatalf("group = %q", group)
	}
	if infoflowChatTitle(channel.InboundMessage{}) != "" {
		t.Fatal("missing sender must not invent a title")
	}
}
