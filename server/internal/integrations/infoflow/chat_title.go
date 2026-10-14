package infoflow

import (
	"strings"

	"github.com/multica-ai/multica/server/internal/integrations/channel"
)

// infoflowChatTitle names a new InfoFlow chat after the person, not the
// message text. Display name wins when the gateway sent one; otherwise the
// account id (fromuserid) is used. Group titles keep the group id so two
// people in one group stay distinguishable.
func infoflowChatTitle(msg channel.InboundMessage) string {
	user := strings.TrimSpace(msg.Source.SenderStableID)
	if user == "" {
		user = strings.TrimSpace(msg.Source.SenderID)
	}
	if user == "" {
		return ""
	}
	if msg.Source.ChatType == channel.ChatTypeGroup {
		group := strings.TrimSpace(msg.Source.ChatID)
		if group == "" {
			return user + "的聊天"
		}
		return group + "-" + user + "的聊天"
	}
	return user + "的聊天"
}
