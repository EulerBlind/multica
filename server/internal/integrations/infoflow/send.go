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
	ids, _, err := s.sendChunked(ctx, target, text)
	if err != nil {
		return "", err
	}
	if len(ids) == 0 {
		return "", nil
	}
	return ids[0], nil
}

// sendChunked delivers text as one or more ordered batches and returns each
// batch's id plus an outcome (msgtype used, batches delivered). It is the single
// place length limits are handled: an oversized single request would otherwise
// be rejected outright (errcode 40061) and the reply would silently vanish.
//
// For a GROUP target the body is first normalized for InfoFlow (tables/task
// lists) and, when it still contains markdown, sent as header msgtype=MD with an
// MD element. Markdown send failures fall back to re-sending the SAME content as
// plain TEXT so a rendering mismatch never drops the reply body. The AT mention
// (when present) rides the first batch only.
//
// Delivery accounting (guards the N53 duplicate-send defect):
//   - a batch is DELIVERED only when the gateway returns a success response we
//     can read a message id from; those ids are what we report;
//   - an explicit rejection (40061 length, 40000 permission/param, ...) means
//     NOT delivered ⇒ eligible for re-send;
//   - an UNKNOWN outcome (transport timeout/reset) is also treated as NOT
//     delivered ⇒ re-sent. That is deliberately at-least-once: a duplicate is
//     preferable to a silently missing part of the reply.
//   - the markdown→TEXT fallback re-sends only the un-acked batches
//     (batches[firstFailedIndex:]); already-acked batches are never re-sent.
func (s *sender) sendChunked(ctx context.Context, target infoflowSendTarget, text string) ([]string, infoflowSendOutcome, error) {
	if text == "" {
		return nil, infoflowSendOutcome{}, nil
	}
	// Group replies are normalized once; p2p keeps its raw text (unchanged this
	// round — the single-chat send path is still gated by D1).
	body := text
	useMD := false
	if target.ChatType == "group" {
		body = normalizeForInfoflow(text)
		useMD = containsMarkdown(body)
	}
	msgType := msgTypeText
	if useMD {
		msgType = msgTypeMD
	}
	// The AT element is the only @. Do not also write "@name" into the body:
	// the client renders the AT as the display name, and a text prefix of the
	// account id shows up beside it.
	batches := splitTextChunks(body, infoflowMaxTextRunes)
	ids := make([]string, 0, len(batches))
	for i, batch := range batches {
		batchTarget := target
		if i > 0 {
			batchTarget.MentionUserID = ""
		}
		id, err := s.sendOnce(ctx, batchTarget, batch, msgType)
		if err != nil {
			// Markdown was rejected (bad msgtype, unsupported element, or the
			// gateway refusing MD): re-send THE SAME content as plain TEXT so the
			// reply body is not lost. Only if the TEXT retry also fails does the
			// caller fall back to a visible notice.
			//
			// batches[0:i] were already acked, so the retry starts at the FIRST
			// FAILED batch (batches[i:]) and never re-sends an accepted one — the
			// N53 defect was re-sending from index 0 and duplicating the first
			// batch. The AT is re-attached only when the first batch itself failed
			// (i == 0).
			if msgType == msgTypeMD {
				retryIDs, retryErr := s.sendBatchesAsText(ctx, target, batches[i:], i == 0)
				delivered := append(append([]string(nil), ids...), retryIDs...)
				outcome := infoflowSendOutcome{MsgType: msgTypeText, DeliveredBatches: len(delivered)}
				if retryErr != nil {
					return delivered, outcome, fmt.Errorf("post infoflow reply as markup failed then text retry failed: %w", retryErr)
				}
				return delivered, outcome, nil
			}
			return ids, infoflowSendOutcome{MsgType: msgType, DeliveredBatches: len(ids)}, err
		}
		ids = append(ids, id)
		if i < len(batches)-1 && infoflowDefaultChunkDelay > 0 {
			select {
			case <-ctx.Done():
				return ids, infoflowSendOutcome{MsgType: msgType, DeliveredBatches: len(ids)}, ctx.Err()
			case <-time.After(infoflowDefaultChunkDelay):
			}
		}
	}
	return ids, infoflowSendOutcome{MsgType: msgType, DeliveredBatches: len(ids)}, nil
}

// sendBatchesAsText re-sends a batch list as plain TEXT (the markdown fallback).
// The caller passes ONLY the batches that were not yet acked. attachMention
// decides whether the first element of this sequence keeps the AT: it is true
// only when the sequence starts at the reply's very first batch (the original
// first batch was never delivered); otherwise the AT already landed on an
// earlier batch and must not repeat.
func (s *sender) sendBatchesAsText(ctx context.Context, target infoflowSendTarget, batches []string, attachMention bool) ([]string, error) {
	ids := make([]string, 0, len(batches))
	for i, batch := range batches {
		batchTarget := target
		if i > 0 || !attachMention {
			batchTarget.MentionUserID = ""
		}
		id, err := s.sendOnce(ctx, batchTarget, batch, msgTypeText)
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

// sendTextOnly posts text as plain TEXT (no markdown detection, no
// normalization, no chunking). The failure fallback uses it so a markdown
// rejection cannot repeat: the notice always goes out as TEXT.
func (s *sender) sendTextOnly(ctx context.Context, target infoflowSendTarget, text string) (string, error) {
	if text == "" {
		return "", nil
	}
	return s.sendOnce(ctx, target, text, msgTypeText)
}

// sendOnce posts exactly one message (no chunking) and returns its id.
func (s *sender) sendOnce(ctx context.Context, target infoflowSendTarget, text string, msgType infoflowMsgType) (string, error) {
	body, err := buildSendBody(target, s.agentID, text, msgType)
	if err != nil {
		return "", err
	}
	return s.postPrepared(ctx, target, body)
}

func (s *sender) postPrepared(ctx context.Context, target infoflowSendTarget, body map[string]any) (string, error) {
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

func buildSendBody(target infoflowSendTarget, agentID, text string, msgType infoflowMsgType) (map[string]any, error) {
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
	if msgType == "" {
		msgType = msgTypeText
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
		body = append(body, atElement(mention))
	}
	// The element type follows the header msgtype: a body containing a markdown
	// element requires msgtype=MD (the gateway rejects a mismatch).
	body = append(body, map[string]any{"type": string(msgType), "content": text})
	return groupEnvelope(agentIDNum, toID, msgType, body), nil
}

func atElement(mention string) map[string]any {
	return map[string]any{"type": "AT", "atall": false, "atuserids": atUserIDsForMention(mention, nil)}
}

func groupEnvelope(agentID, toID int64, msgType infoflowMsgType, body []map[string]any) map[string]any {
	return map[string]any{
		"agentid": agentID,
		"message": map[string]any{
			"header": map[string]any{
				"toid":        toID,
				"totype":      "GROUP",
				"msgtype":     string(msgType),
				"clientmsgid": time.Now().UnixMilli(),
				"role":        "robot",
			},
			"body": body,
		},
	}
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

// infoflowMsgType is the element/header type a batch is sent as. The InfoFlow
// group send distinguishes plain text (TEXT) from markdown (MD); the header
// msgtype MUST match the body element type (see the SDK's sendToGroupWithOptions
// doc: a body containing MD requires header msgtype=MD).
type infoflowMsgType string

const (
	msgTypeText infoflowMsgType = "TEXT"
	msgTypeMD   infoflowMsgType = "MD"
)

// infoflowSendOutcome reports how many batches were delivered and whether the
// body went out as markdown or plain text. It exists so the outbound layer can
// log partial delivery (R30) and the msgtype actually used.
type infoflowSendOutcome struct {
	MsgType          infoflowMsgType
	DeliveredBatches int
}
