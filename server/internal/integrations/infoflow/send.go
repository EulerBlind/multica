package infoflow

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// InfoFlow robot send paths. Group messages go through the robot endpoint;
// single-chat messages go through the application message endpoint, which is
// currently gated by an application permission (errcode 40000).
const (
	pathSendGroup  = "/robot/msg/groupmsgsend"
	pathSendSingle = "/app/message/send"
)

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
func (s *sender) sendText(ctx context.Context, target infoflowSendTarget, text string) (string, error) {
	if text == "" {
		return "", nil
	}
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
