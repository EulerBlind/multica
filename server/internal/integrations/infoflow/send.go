package infoflow

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// InfoFlow robot send paths. Group messages go through the robot endpoint;
// single-chat messages go through the application message endpoint, which is
// currently gated by an application permission (errcode 40000).
const (
	pathSendGroup  = "/robot/msg/groupmsgsend"
	pathSendSingle = "/app/message/send"
)

// infoflowMaxTextRunes bounds ONE outbound text element. The live group-send
// API rejects an oversized payload with errcode 40061 "message text length
// exceed limit" (and 40060 for the whole body); probing the gateway bracketed
// the rejection between 2048 and 2500 characters, so this stays comfortably
// below it. Message text is split on rune boundaries at or under this size.
const infoflowMaxTextRunes = 1500

// infoflowDefaultChunkDelay spaces consecutive batches so the group shows them
// in order; it is a var so tests can zero it out.
var infoflowDefaultChunkDelay = 120 * time.Millisecond

// infoflowSendTarget is the resolved destination for a reply.
type infoflowSendTarget struct {
	ChatType string
	ChatID   string
	SenderID string
	// MentionUserID is the InfoFlow user id to @ mention ahead of a group reply
	// (the person who asked). Empty means a plain group message with no AT.
	MentionUserID string
}

// sender posts replies for one installation.
type sender struct {
	client    *Client
	appKey    string
	appSecret string
	agentID   string
}

// sendText delivers one bounded plain-text message, refreshing the access token
// once on a 401. Permission failures are returned wrapped in
// ErrPermissionNotGranted so the caller can classify them.
//
// Text longer than infoflowMaxTextRunes is split into ordered batches and sent
// sequentially; every batch must succeed or the whole send fails. The @ mention
// rides on the FIRST batch only, so the recipient is notified once (see
// sendChunked).
func (s *sender) sendText(ctx context.Context, target infoflowSendTarget, text string) (string, error) {
	ids, err := s.sendChunked(ctx, target, text)
	if err != nil {
		return "", err
	}
	if len(ids) == 0 {
		return "", nil
	}
	return ids[0], nil
}

// sendChunked delivers text as one or more ordered batches and returns each
// batch's message id. It is the single place length limits are handled: an
// oversized single request would otherwise be rejected outright (errcode 40061)
// and the reply would silently vanish. The AT mention (when present) is attached
// to the first batch only so the group is not re-notified per batch.
func (s *sender) sendChunked(ctx context.Context, target infoflowSendTarget, text string) ([]string, error) {
	if text == "" {
		return nil, nil
	}
	batches := splitTextChunks(text, infoflowMaxTextRunes)
	ids := make([]string, 0, len(batches))
	for i, batch := range batches {
		batchTarget := target
		if i > 0 {
			// Only the first batch @ mentions the asker.
			batchTarget.MentionUserID = ""
		}
		id, err := s.sendOnce(ctx, batchTarget, batch)
		if err != nil {
			return ids, err
		}
		ids = append(ids, id)
		if i < len(batches)-1 && infoflowDefaultChunkDelay > 0 {
			select {
			case <-ctx.Done():
				return ids, ctx.Err()
			case <-time.After(infoflowDefaultChunkDelay):
			}
		}
	}
	return ids, nil
}

// sendOnce posts exactly one message (no chunking) and returns its id.
func (s *sender) sendOnce(ctx context.Context, target infoflowSendTarget, text string) (string, error) {
	body, err := buildSendBody(target, s.agentID, text)
	if err != nil {
		return "", err
	}
	path := pathSendGroup
	if target.ChatType == "p2p" {
		path = pathSendSingle
	}
	var resp struct {
		Data map[string]any `json:"data"`
	}
	for attempt := 0; attempt < 2; attempt++ {
		token, err := s.client.AccessToken(ctx, s.appKey, s.appSecret)
		if err != nil {
			return "", fmt.Errorf("access token: %w", err)
		}
		err = s.client.postJSON(ctx, path, token, body, &resp)
		if err == nil {
			return messageIDFromSendResponse(resp.Data), nil
		}
		if errors.Is(err, ErrUnauthorized) && attempt == 0 {
			s.client.Invalidate(s.appKey)
			continue
		}
		return "", err
	}
	return "", ErrUnauthorized
}

// splitTextChunks splits text into ordered chunks of at most maxRunes runes,
// preferring a line break and then a space so a chunk rarely cuts mid-word. It
// never returns an empty chunk for non-empty input.
func splitTextChunks(text string, maxRunes int) []string {
	if maxRunes <= 0 {
		maxRunes = infoflowMaxTextRunes
	}
	if utf8.RuneCountInString(text) <= maxRunes {
		return []string{text}
	}
	var chunks []string
	remaining := text
	for utf8.RuneCountInString(remaining) > maxRunes {
		cut := runeCutIndex(remaining, maxRunes)
		chunk := remaining[:cut]
		if trimmed := strings.TrimRight(chunk, "\n"); trimmed != "" {
			chunk = trimmed
		}
		chunks = append(chunks, chunk)
		remaining = strings.TrimLeft(remaining[cut:], "\n")
	}
	if remaining != "" {
		chunks = append(chunks, remaining)
	}
	return chunks
}

// runeCutIndex returns the byte index at which to cut s so the head holds at
// most maxRunes runes, backing off to the last line break (preferred) or space
// within the window when one is reasonably close to the end.
func runeCutIndex(s string, maxRunes int) int {
	limit := len(s)
	count := 0
	for i := range s {
		if count == maxRunes {
			limit = i
			break
		}
		count++
	}
	window := s[:limit]
	if idx := strings.LastIndex(window, "\n"); idx > limit/2 {
		return idx + 1
	}
	if idx := strings.LastIndex(window, " "); idx > limit/2 {
		return idx + 1
	}
	return limit
}

func buildSendBody(target infoflowSendTarget, agentID, text string) (map[string]any, error) {
	if target.ChatType == "p2p" {
		if target.SenderID == "" {
			return nil, errors.New("infoflow: 1:1 send missing recipient")
		}
		agent := agentID
		if agent == "" {
			agent = target.ChatID
		}
		return map[string]any{
			"touser":  target.SenderID,
			"msgtype": "text",
			"agentid": agent,
			"text":    map[string]any{"content": text},
		}, nil
	}
	if target.ChatID == "" {
		return nil, errors.New("infoflow: group send missing group id")
	}
	toID, err := strconv.ParseInt(target.ChatID, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("infoflow: group id %q is not numeric: %w", target.ChatID, err)
	}
	agentIDNum, err := strconv.ParseInt(agentID, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("infoflow: agent id %q is not numeric: %w", agentID, err)
	}
	// Address the reply to the person who asked: an AT element ahead of the text
	// (a plain TEXT-only body when there is no mention target, so a legacy
	// binding never regresses into a broken send).
	body := []map[string]any{}
	if mention := strings.TrimSpace(target.MentionUserID); mention != "" {
		body = append(body, map[string]any{"type": "AT", "atall": false, "atuserids": []string{mention}})
	}
	body = append(body, map[string]any{"type": "TEXT", "content": text})
	return map[string]any{
		"agentid": agentIDNum,
		"message": map[string]any{
			"header": map[string]any{
				"toid":        toID,
				"totype":      "GROUP",
				"msgtype":     "TEXT",
				"clientmsgid": time.Now().UnixMilli(),
				"role":        "robot",
			},
			"body": body,
		},
	}, nil
}

func messageIDFromSendResponse(data map[string]any) string {
	if data == nil {
		return ""
	}
	if id, ok := data["messageid"]; ok {
		return fmt.Sprint(id)
	}
	if id, ok := data["msgkey"]; ok {
		return fmt.Sprint(id)
	}
	// The group endpoint nests the identifiers one level deeper
	// (data.data.messageid); accept both shapes.
	if nested, ok := data["data"].(map[string]any); ok {
		if id, ok := nested["messageid"]; ok {
			return fmt.Sprint(id)
		}
		if id, ok := nested["msgkey"]; ok {
			return fmt.Sprint(id)
		}
	}
	return ""
}
