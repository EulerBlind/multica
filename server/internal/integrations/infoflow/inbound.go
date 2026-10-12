package infoflow

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/multica-ai/multica/server/internal/integrations/channel"
)

type infoflowRaw struct {
	AppID        any    `json:"agentid"`
	EventType    string `json:"eventtype"`
	GroupID      any    `json:"groupid"`
	MsgID        any    `json:"MsgId"`
	MsgType      string `json:"MsgType"`
	FromUserID   string `json:"FromUserId"`
	FromUserName string `json:"FromUserName"`
	Content      string `json:"Content"`
	PicURL       string `json:"PicUrl"`
	Message      *struct {
		Header struct {
			FromUserID string `json:"fromuserid"`
			ToID       any    `json:"toid"`
			ToType     string `json:"totype"`
			MsgType    string `json:"msgtype"`
			MessageID  any    `json:"messageid"`
		} `json:"header"`
		Body  []map[string]any `json:"body"`
		Reply *struct {
			MessageID any    `json:"messageid"`
			Preview   string `json:"preview"`
		} `json:"reply"`
	} `json:"message"`
}

// inboundFromPayload translates a raw InfoFlow event payload into the
// platform-agnostic channel.InboundMessage. appID is stamped by the owning
// long connection because the platform service itself does not reliably carry
// the application id in every event. botUserID is used to classify group
// @mentions.
// Drop reasons reported by inboundFromPayloadReason. They name WHY a payload
// did not become a routed message so an operator can tell a genuine
// misconfiguration (e.g. a missing bot_user_id) from ordinary group chatter.
const (
	dropReasonMalformed      = "malformed_payload"
	dropReasonMissingSender  = "missing_sender"
	dropReasonEmptyContent   = "empty_content"
	dropReasonMissingGroupID = "missing_group_id"
)

func inboundFromPayload(raw json.RawMessage, appID, botUserID string) (channel.InboundMessage, bool) {
	msg, _, ok := inboundFromPayloadReason(raw, appID, botUserID)
	return msg, ok
}

// inboundFromPayloadReason is inboundFromPayload plus the drop reason, used by
// the receive loop to log actionable diagnostics (N15 W1).
func inboundFromPayloadReason(raw json.RawMessage, appID, botUserID string) (channel.InboundMessage, string, bool) {
	var payload infoflowRaw
	if err := json.Unmarshal(raw, &payload); err != nil {
		return channel.InboundMessage{}, dropReasonMalformed, false
	}
	rawAppID := appID
	if payload.AppID != nil {
		rawAppID = rawAppIDString(payload.AppID)
	}
	payload.AppID = rawAppID
	if isGroupPayload(payload) {
		return normalizeGroupPayloadReason(payload, botUserID)
	}
	msg, ok := normalizePrivatePayload(payload)
	if !ok {
		if strings.TrimSpace(payload.FromUserID) == "" {
			return channel.InboundMessage{}, dropReasonMissingSender, false
		}
		return channel.InboundMessage{}, dropReasonEmptyContent, false
	}
	return msg, "", true
}

func isGroupPayload(payload infoflowRaw) bool {
	return payload.Message != nil && (payload.GroupID != nil || payload.Message.Header.ToID != nil)
}

func normalizePrivatePayload(payload infoflowRaw) (channel.InboundMessage, bool) {
	senderID := strings.TrimSpace(payload.FromUserID)
	if senderID == "" {
		return channel.InboundMessage{}, false
	}
	text := flattenPrivateText(payload)
	if strings.TrimSpace(text) == "" && strings.TrimSpace(payload.PicURL) == "" {
		return channel.InboundMessage{}, false
	}
	msgID := anyString(payload.MsgID)
	return channel.InboundMessage{
		EventID:        msgID,
		MessageID:      msgID,
		Source:         channel.Source{ChannelType: TypeInfoFlow, ChatID: senderID, ChatType: channel.ChatTypeP2P, SenderID: senderID},
		Type:           msgTypeFromPrivate(payload),
		Text:           text,
		CommandText:    text,
		AddressedToBot: true,
		Raw:            mustJSON(payload),
	}, true
}

func normalizeGroupPayload(payload infoflowRaw, botUserID string) (channel.InboundMessage, bool) {
	msg, _, ok := normalizeGroupPayloadReason(payload, botUserID)
	return msg, ok
}

func normalizeGroupPayloadReason(payload infoflowRaw, botUserID string) (channel.InboundMessage, string, bool) {
	h := payload.Message.Header
	groupID := anyString(payload.GroupID)
	if groupID == "" {
		groupID = anyString(h.ToID)
	}
	senderID := strings.TrimSpace(h.FromUserID)
	if groupID == "" {
		return channel.InboundMessage{}, dropReasonMissingGroupID, false
	}
	if senderID == "" {
		return channel.InboundMessage{}, dropReasonMissingSender, false
	}
	text := flattenGroupText(payload.Message.Body)
	if strings.TrimSpace(text) == "" && !containsAT(payload.Message.Body) {
		return channel.InboundMessage{}, dropReasonEmptyContent, false
	}
	eventType := "MESSAGE_RECEIVE"
	if payload.EventType != "" {
		eventType = payload.EventType
	}
	msgID := anyString(h.MessageID)
	msg := channel.InboundMessage{
		EventID:        msgID,
		MessageID:      msgID,
		Source:         channel.Source{ChannelType: TypeInfoFlow, ChatID: groupID, ChatType: channel.ChatTypeGroup, SenderID: senderID},
		Type:           msgTypeFromHeader(h.MsgType),
		Text:           text,
		CommandText:    text,
		ReplyTo:        groupReplyTo(payload.Message),
		AddressedToBot: addressedToBot(payload, botUserID, eventType),
		Raw:            mustJSON(payload),
	}
	// NOTE: an unaddressed group message is deliberately routed (ok=true). The
	// engine owns that decision: it drops with drop_reason=not_addressed_in_group,
	// writes the channel_inbound_audit row and runs the dedup gate. Deciding it
	// here instead would silently remove that audit trail and bypass dedup, so
	// this adapter only reports WHY it could not build a message at all.
	return msg, "", true
}

func flattenPrivateText(payload infoflowRaw) string {
	if strings.TrimSpace(payload.Content) != "" {
		return payload.Content
	}
	if strings.TrimSpace(payload.PicURL) != "" {
		return "[IMAGE] " + payload.PicURL
	}
	return ""
}

func flattenGroupText(body []map[string]any) string {
	var parts []string
	for _, item := range body {
		if item == nil {
			continue
		}
		typeName := strings.ToUpper(stringOrEmpty(item["type"]))
		switch typeName {
		case "AT":
			continue
		case "TEXT":
			if s := stringOrEmpty(item["content"]); s != "" {
				parts = append(parts, s)
			}
		case "IMAGE":
			if url := firstURL(item["downloadurl"], item["url"]); url != "" {
				parts = append(parts, "[IMAGE] "+url)
			} else {
				parts = append(parts, "[IMAGE]")
			}
		case "FILE":
			name := stringOrEmpty(item["name"])
			if url := firstURL(item["downloadurl"], item["url"]); url != "" {
				parts = append(parts, "[FILE: "+name+"] "+url)
			} else {
				parts = append(parts, "[FILE: "+name+"]")
			}
		case "LINK":
			if s := firstURL(item["label"], item["url"]); s != "" {
				parts = append(parts, "链接: "+s)
			}
		default:
			if s := stringOrEmpty(item["content"]); s != "" {
				parts = append(parts, s)
			}
		}
	}
	return strings.TrimSpace(strings.Join(parts, "\n"))
}

// addressedToBot decides whether a GROUP message is directed at the bot.
//
// Semantics are deliberately fail-CLOSED: only a positive, explicit mention of
// the bot (or a reply to it) counts, so ordinary group chatter never reaches the
// Agent. Two independent facts must hold:
//
//  1. the message actually mentions the bot, and
//  2. the installation knows which user id the bot is (botUserID).
//
// If botUserID is unset we cannot positively identify the bot, so the message is
// NOT addressed (except for an explicit @all) — this is the case that previously
// opened the gate and let every group message through.
//
// The mention shape follows the InfoFlow group-message body, whose AT element is
// documented as `{ "type": "AT", "atall": <bool>, "atuserids": [<id>...] }`.
// Deployments have also been observed putting the target in userId/userid/uid/id
// /name, so those are accepted as alternates. Matching is EXACT against the
// configured bot id — never a substring — because a substring test made
// "4105001274" match unrelated ids.
//
// ALL_MESSAGE_FORWARD (an app subscribed to every group message) does NOT imply
// the bot was addressed; that subscription only decides what we receive, not
// what we should act on. Such messages fall through to the mention check and are
// dropped as not_addressed_in_group when the bot is not named.
func addressedToBot(payload infoflowRaw, botUserID string, eventType string) bool {
	if payload.Message == nil {
		return false
	}
	// A reply to the bot is an explicit signal regardless of @.
	if payload.Message.Reply != nil {
		return true
	}
	botUserID = strings.TrimSpace(botUserID)
	for _, item := range payload.Message.Body {
		if !strings.EqualFold(stringOrEmpty(item["type"]), "AT") {
			continue
		}
		// @all is an explicit broadcast: it always addresses the bot.
		if atAll, _ := item["atall"].(bool); atAll {
			return true
		}
		// Without a configured bot id we cannot positively match a named @,
		// so stay closed rather than guessing.
		if botUserID == "" {
			continue
		}
		if atItemMentionsBot(item, botUserID) {
			return true
		}
	}
	return false
}

// atItemMentionsBot reports whether one AT body element names the bot. It reads
// the documented `atuserids` array first, then the alternate scalar shapes seen
// in the wild, and compares each candidate EXACTLY against botUserID.
func atItemMentionsBot(item map[string]any, botUserID string) bool {
	if ids := stringListOrEmpty(item["atuserids"]); len(ids) > 0 {
		for _, id := range ids {
			if id == botUserID {
				return true
			}
		}
	}
	candidates := []string{
		stringOrEmpty(item["userid"]),
		stringOrEmpty(item["userId"]),
		stringOrEmpty(item["uid"]),
		stringOrEmpty(item["id"]),
		stringOrEmpty(item["name"]),
		stringOrEmpty(item["label"]),
	}
	for _, candidate := range candidates {
		if candidate != "" && candidate == botUserID {
			return true
		}
	}
	return false
}

// stringListOrEmpty normalizes a field that may arrive as a string or an array
// of strings into a trimmed, non-empty slice.
func stringListOrEmpty(v any) []string {
	switch t := v.(type) {
	case string:
		if s := strings.TrimSpace(t); s != "" {
			return []string{s}
		}
	case []any:
		out := make([]string, 0, len(t))
		for _, e := range t {
			if s := stringOrEmpty(e); s != "" {
				out = append(out, s)
			}
		}
		return out
	case []string:
		out := make([]string, 0, len(t))
		for _, e := range t {
			if s := strings.TrimSpace(e); s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

func containsAT(body []map[string]any) bool {
	for _, item := range body {
		if strings.EqualFold(stringOrEmpty(item["type"]), "AT") {
			return true
		}
	}
	return false
}

func groupReplyTo(message *struct {
	Header struct {
		FromUserID string `json:"fromuserid"`
		ToID       any    `json:"toid"`
		ToType     string `json:"totype"`
		MsgType    string `json:"msgtype"`
		MessageID  any    `json:"messageid"`
	} `json:"header"`
	Body  []map[string]any `json:"body"`
	Reply *struct {
		MessageID any    `json:"messageid"`
		Preview   string `json:"preview"`
	} `json:"reply"`
}) *channel.ReplyCtx {
	if message == nil || message.Reply == nil {
		return nil
	}
	return &channel.ReplyCtx{MessageID: anyString(message.Reply.MessageID)}
}

func msgTypeFromPrivate(payload infoflowRaw) channel.MsgType {
	if strings.TrimSpace(payload.PicURL) != "" {
		return channel.MsgTypeImage
	}
	switch strings.ToLower(payload.MsgType) {
	case "image":
		return channel.MsgTypeImage
	case "file":
		return channel.MsgTypeFile
	case "voice":
		return channel.MsgTypeAudio
	case "", "text":
		return channel.MsgTypeText
	default:
		return channel.MsgTypeUnknown
	}
}

func msgTypeFromHeader(msgType string) channel.MsgType {
	switch strings.ToLower(strings.TrimSpace(msgType)) {
	case "image":
		return channel.MsgTypeImage
	case "file":
		return channel.MsgTypeFile
	case "voice":
		return channel.MsgTypeAudio
	case "text", "":
		return channel.MsgTypeText
	default:
		return channel.MsgTypeUnknown
	}
}

func anyString(v any) string {
	switch t := v.(type) {
	case string:
		return strings.TrimSpace(t)
	case float64:
		return strings.TrimSpace(fmt.Sprintf("%.0f", t))
	case int:
		return strings.TrimSpace(fmt.Sprintf("%d", t))
	case json.Number:
		return strings.TrimSpace(t.String())
	case nil:
		return ""
	default:
		b, _ := json.Marshal(v)
		return strings.Trim(string(b), `"`)
	}
}

func stringOrEmpty(v any) string {
	if s, ok := v.(string); ok {
		return strings.TrimSpace(s)
	}
	return ""
}

func firstURL(candidates ...any) string {
	for _, candidate := range candidates {
		if s := stringOrEmpty(candidate); s != "" {
			return s
		}
	}
	return ""
}

func rawAppIDString(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return strings.TrimSpace(s)
	}
	return anyString(v)
}

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}
