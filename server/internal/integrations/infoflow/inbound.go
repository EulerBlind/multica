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
			FromUserID   string `json:"fromuserid"`
			FromUserName string `json:"fromusername"`
			ToID         any    `json:"toid"`
			ToType       string `json:"totype"`
			MsgType      string `json:"msgtype"`
			MessageID    any    `json:"messageid"`
		} `json:"header"`
		Body  []map[string]any `json:"body"`
		Reply *infoflowQuote   `json:"reply"`
		Quote *infoflowQuote   `json:"quote"`
	} `json:"message"`
}

// infoflowQuote is a quoted/replied message carried beside the sender's own
// body. The live gateway puts text in preview and images in a nested body;
// both shapes are read so a quoted image URL is not dropped.
type infoflowQuote struct {
	MessageID any              `json:"messageid"`
	Preview   string           `json:"preview"`
	Content   string           `json:"content"`
	Body      []map[string]any `json:"body"`
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
	return inboundFromPayloadWithIdentity(raw, appID, newIdentitySet(botUserID, nil))
}

// inboundFromPayloadWithIdentity normalizes a payload using the given self
// identity set. The channel layer passes an identity set enriched with
// group-resolved tokens; tests can pass a bare configured set.
func inboundFromPayloadWithIdentity(raw json.RawMessage, appID string, self identitySet) (channel.InboundMessage, bool) {
	msg, _, ok := inboundFromPayloadReasonWithIdentity(raw, appID, self)
	return msg, ok
}

// inboundFromPayloadReason is inboundFromPayload plus the drop reason, used by
// the receive loop to log actionable diagnostics (N15 W1).
func inboundFromPayloadReason(raw json.RawMessage, appID, botUserID string) (channel.InboundMessage, string, bool) {
	return inboundFromPayloadReasonWithIdentity(raw, appID, newIdentitySet(botUserID, nil))
}

// inboundFromPayloadReasonWithIdentity is inboundFromPayloadReason over an
// explicit identity set (configured + group-resolved).
func inboundFromPayloadReasonWithIdentity(raw json.RawMessage, appID string, self identitySet) (channel.InboundMessage, string, bool) {
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
		return normalizeGroupPayloadReasonWithIdentity(payload, self)
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
		Source:         channel.Source{ChannelType: TypeInfoFlow, ChatID: senderID, ChatType: channel.ChatTypeP2P, SenderID: senderID, SenderStableID: strings.TrimSpace(payload.FromUserName)},
		Type:           msgTypeFromPrivate(payload),
		Text:           text,
		CommandText:    text,
		AddressedToBot: true,
		Raw:            mustJSON(payload),
	}, true
}

func normalizeGroupPayload(payload infoflowRaw, botUserID string) (channel.InboundMessage, bool) {
	return normalizeGroupPayloadWithIdentity(payload, newIdentitySet(botUserID, nil))
}

func normalizeGroupPayloadWithIdentity(payload infoflowRaw, self identitySet) (channel.InboundMessage, bool) {
	msg, _, ok := normalizeGroupPayloadReasonWithIdentity(payload, self)
	return msg, ok
}

func normalizeGroupPayloadReason(payload infoflowRaw, botUserID string) (channel.InboundMessage, string, bool) {
	return normalizeGroupPayloadReasonWithIdentity(payload, newIdentitySet(botUserID, nil))
}

func normalizeGroupPayloadReasonWithIdentity(payload infoflowRaw, self identitySet) (channel.InboundMessage, string, bool) {
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
	own := flattenGroupText(payload.Message.Body)
	quoted := flattenQuoted(payload.Message)
	text := own
	if quoted != "" {
		text = "<quoted_message>\n" + quoted + "\n</quoted_message>"
		if strings.TrimSpace(own) != "" {
			text += "\n\n" + own
		}
	}
	if strings.TrimSpace(text) == "" && !containsAT(payload.Message.Body) {
		return channel.InboundMessage{}, dropReasonEmptyContent, false
	}
	command := own
	if command == "" && quoted != "" {
		// Router copies an empty CommandText from the already-enriched Text,
		// which would make the quote the chat title. A space is not a command
		// and is not the quoted body.
		command = " "
	}
	eventType := "MESSAGE_RECEIVE"
	if payload.EventType != "" {
		eventType = payload.EventType
	}
	msgID := anyString(h.MessageID)
	msg := channel.InboundMessage{
		EventID:            msgID,
		MessageID:          msgID,
		Source:             channel.Source{ChannelType: TypeInfoFlow, ChatID: groupID, ChatType: channel.ChatTypeGroup, SenderID: senderID, SenderStableID: strings.TrimSpace(h.FromUserName)},
		Type:               msgTypeFromHeader(h.MsgType),
		Text:               text,
		CommandText:        command,
		HasSelectedContext: quoted != "",
		ReplyTo:            groupReplyTo(payload.Message),
		AddressedToBot:     addressedToBot(payload, self, eventType),
		Raw:                mustJSON(payload),
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
			url := firstURL(item["downloadurl"], item["url"])
			if url == "" {
				if c := stringOrEmpty(item["content"]); strings.HasPrefix(c, "http") {
					url = c
				}
			}
			if url != "" {
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
		case "QUOTE", "REPLY", "REFERENCE":
			if nested := flattenGroupText(bodyItems(item["body"])); nested != "" {
				parts = append(parts, nested)
			} else if s := stringOrEmpty(item["content"]); s != "" {
				parts = append(parts, s)
			}
			if url := firstURL(item["downloadurl"], item["url"], item["content"]); strings.HasPrefix(url, "http") && !strings.Contains(strings.Join(parts, "\n"), url) {
				parts = append(parts, "[IMAGE] "+url)
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
// Agent.
//
// Identification is a two-tier exact match against a set of "self identities":
//
//  1. the configured bot_user_id (the robot's own InfoFlow user id), and
//  2. any configured bot_mention_names plus names/ids resolved from the group
//     membership (see groupSelfIdentity) — InfoFlow commonly puts only the
//     robot's DISPLAY NAME in the AT element, and the name changes on rename.
//
// Matching is EXACT (trimmed, then case-insensitive as a fallback) — never a
// substring, because a substring test made "4105001274" match unrelated ids.
// `@all` always addresses the bot; a reply to the bot always addresses it.
//
// The function is pure: all I/O (member resolution) and caching happen in the
// caller, which passes the resolved identities in `extra`.
func addressedToBot(payload infoflowRaw, self identitySet, eventType string) bool {
	if payload.Message == nil {
		return false
	}
	// A reply to the bot is an explicit signal regardless of @.
	if payload.Message.Reply != nil || payload.Message.Quote != nil {
		return true
	}
	for _, item := range payload.Message.Body {
		if !strings.EqualFold(stringOrEmpty(item["type"]), "AT") {
			continue
		}
		// @all is an explicit broadcast: it always addresses the bot.
		if atAll, _ := item["atall"].(bool); atAll {
			return true
		}
		if atItemMentionsBot(item, self) {
			return true
		}
	}
	return false
}

// identitySet holds the tokens that identify the bot itself. configured comes
// from the installation config (authoritative id + optional names); resolved
// comes from the group membership lookup (imId + display name).
type identitySet struct {
	configured []string
	resolved   []string
}

// newIdentitySet builds the set from the configured bot id and mention names.
func newIdentitySet(botUserID string, mentionNames []string) identitySet {
	out := make([]string, 0, 1+len(mentionNames))
	if id := strings.TrimSpace(botUserID); id != "" {
		out = append(out, id)
	}
	for _, name := range mentionNames {
		if n := strings.TrimSpace(name); n != "" {
			out = append(out, n)
		}
	}
	return identitySet{configured: out}
}

// withResolved returns a copy carrying the identities resolved from a group.
func (s identitySet) withResolved(tokens []string) identitySet {
	if len(tokens) == 0 {
		return s
	}
	merged := make([]string, 0, len(s.resolved)+len(tokens))
	merged = append(merged, s.resolved...)
	for _, token := range tokens {
		if t := strings.TrimSpace(token); t != "" {
			merged = append(merged, t)
		}
	}
	return identitySet{configured: s.configured, resolved: merged}
}

// matches reports whether a candidate token identifies the bot (exact, then
// case-insensitive).
func (s identitySet) matches(candidate string) bool {
	candidate = strings.TrimSpace(candidate)
	if candidate == "" {
		return false
	}
	for _, set := range [][]string{s.configured, s.resolved} {
		for _, token := range set {
			if token == candidate {
				return true
			}
		}
	}
	lower := strings.ToLower(candidate)
	for _, set := range [][]string{s.configured, s.resolved} {
		for _, token := range set {
			if strings.ToLower(token) == lower {
				return true
			}
		}
	}
	return false
}

// empty reports whether no identity token is available at all; callers stay
// fail-closed in that case.
func (s identitySet) empty() bool {
	return len(s.configured) == 0 && len(s.resolved) == 0
}

// atItemMentionsBot reports whether one AT body element names the bot. It reads
// the documented `atuserids` array first, then the alternate scalar shapes seen
// in the wild, and matches each candidate against the bot's identity set.
func atItemMentionsBot(item map[string]any, self identitySet) bool {
	for _, id := range stringListOrEmpty(item["atuserids"]) {
		if self.matches(id) {
			return true
		}
	}
	for _, candidate := range []any{item["userid"], item["userId"], item["uid"], item["id"], item["name"], item["label"], item["content"]} {
		if self.matches(stringOrEmpty(candidate)) {
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

// flattenQuoted collects text and image URLs from a quote/reply envelope.
// The sender's own body is not included; callers prepend this as context.
func flattenQuoted(message *struct {
	Header struct {
		FromUserID   string `json:"fromuserid"`
		FromUserName string `json:"fromusername"`
		ToID         any    `json:"toid"`
		ToType       string `json:"totype"`
		MsgType      string `json:"msgtype"`
		MessageID    any    `json:"messageid"`
	} `json:"header"`
	Body  []map[string]any `json:"body"`
	Reply *infoflowQuote   `json:"reply"`
	Quote *infoflowQuote   `json:"quote"`
}) string {
	if message == nil {
		return ""
	}
	var parts []string
	for _, q := range []*infoflowQuote{message.Reply, message.Quote} {
		if q == nil {
			continue
		}
		if s := strings.TrimSpace(q.Preview); s != "" {
			parts = append(parts, s)
		}
		if s := strings.TrimSpace(q.Content); s != "" && s != strings.TrimSpace(q.Preview) {
			parts = append(parts, s)
		}
		if nested := flattenGroupText(q.Body); nested != "" {
			parts = append(parts, nested)
		}
	}
	return strings.TrimSpace(strings.Join(parts, "\n"))
}

func bodyItems(v any) []map[string]any {
	switch t := v.(type) {
	case []map[string]any:
		return t
	case []any:
		out := make([]map[string]any, 0, len(t))
		for _, item := range t {
			if m, ok := item.(map[string]any); ok {
				out = append(out, m)
			}
		}
		return out
	default:
		return nil
	}
}

func groupReplyTo(message *struct {
	Header struct {
		FromUserID   string `json:"fromuserid"`
		FromUserName string `json:"fromusername"`
		ToID         any    `json:"toid"`
		ToType       string `json:"totype"`
		MsgType      string `json:"msgtype"`
		MessageID    any    `json:"messageid"`
	} `json:"header"`
	Body  []map[string]any `json:"body"`
	Reply *infoflowQuote   `json:"reply"`
	Quote *infoflowQuote   `json:"quote"`
}) *channel.ReplyCtx {
	if message == nil {
		return nil
	}
	q := message.Reply
	if q == nil {
		q = message.Quote
	}
	if q == nil {
		return nil
	}
	return &channel.ReplyCtx{MessageID: anyString(q.MessageID)}
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
