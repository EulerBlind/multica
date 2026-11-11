// Package infoflow is the Baidu InfoFlow (如流) integration for the
// channel-agnostic engine. It follows the bring-your-own-app (BYO) model used
// by the DingTalk adapter: an agent owner or workspace admin registers a robot
// in the InfoFlow open platform and pastes its application id, AppKey and
// AppSecret into Multica. Each channel_installation carries its OWN credential
// set and gets its OWN long-connection, supervised per-installation by the
// engine like Feishu, Slack and DingTalk.
//
// InfoFlow specifics verified in QIA-600 N2:
//   - HTTP auth: POST /api/v1/auth/app_access_token with
//     {app_key, app_secret: md5hex(app_secret)} → {app_access_token, expire}.
//   - Auth header: Authorization: Bearer-<token> (note the "Bearer-" prefix).
//   - Long connection: POST https://infoflow-open-gateway.baidu.com/open/ws/endpoint
//     with {app_key, app_secret} → one-shot ws url + client_config, then a
//     protobuf-framed WebSocket session (frame.proto) with ping/pong and
//     event_ack.
//   - Outbound: POST /robot/msg/groupmsgsend (group) and POST /app/message/send
//     (single chat). The single-chat API is currently gated by an application
//     permission (errcode 40000 "Api not allowed"); the adapter classifies that
//     as a permission error rather than a retryable transport failure.
package infoflow

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/multica-ai/multica/server/internal/integrations/channel"
)

// TypeInfoFlow is the channel discriminator for the InfoFlow adapter. It lives
// with the adapter (not in the channel core) so registering a new platform
// never requires editing the core, mirroring slack.TypeSlack and
// dingtalk.TypeDingTalk.
const TypeInfoFlow channel.Type = "infoflow"

// installConfig is the JSON shape stored in channel_installation.config for an
// InfoFlow installation. The cross-platform columns stay flat; everything
// InfoFlow-specific lives in this opaque blob.
//
// app_id holds the InfoFlow application id and doubles as the per-installation
// routing key: the generic GetChannelInstallationByAppID query
// (config->>'app_id') plus the (channel_type, app_id) unique index map an
// inbound event's stamped application id to its installation, so several robots
// (several agents) in one InfoFlow organization stay distinct.
//
// app_key is the open-platform AppKey used for the two-phase WebSocket auth and
// the token mint; app_secret_encrypted is base64 secretbox ciphertext, never
// plaintext. bot_user_id is the robot's own InfoFlow user id, used to suppress
// the bot's own outbound messages on the inbound path.
type installConfig struct {
	AppID              string `json:"app_id"`
	AppKey             string `json:"app_key"`
	AppSecretEncrypted string `json:"app_secret_encrypted"`
	BotUserID          string `json:"bot_user_id,omitempty"`
	BotMentionNames    string `json:"bot_mention_names,omitempty"`
	AgentID            string `json:"agent_id,omitempty"`
}

// credentials is the decoded, decrypted form the outbound sender and the
// WebSocket connector run on.
type credentials struct {
	AppID     string
	AppKey    string
	AppSecret string
	BotUserID string
	// BotMentionNames are the display names the bot may be @-mentioned by.
	// InfoFlow's group AT element commonly carries only the robot's display
	// name (not its imId), and the display name changes when the robot is
	// renamed, so this is a configurable fallback next to the authoritative
	// BotUserID. Comma-separated in the stored JSON.
	BotMentionNames []string
	AgentID         string
}

// Decrypter turns stored ciphertext into plaintext. The wiring injects a
// secretbox-backed implementation; tests inject an identity decrypter (or nil,
// which treats the stored bytes as plaintext).
type Decrypter func(ciphertext []byte) (plaintext []byte, err error)

// decodeCredentials parses the per-installation config blob and decrypts the
// stored AppSecret. It is the single place the InfoFlow config JSON is
// interpreted for the outbound/token paths.
func decodeCredentials(raw json.RawMessage, decrypt Decrypter) (credentials, error) {
	if len(raw) == 0 {
		return credentials{}, errors.New("infoflow: empty installation config")
	}
	var cfg installConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return credentials{}, fmt.Errorf("decode infoflow installation config: %w", err)
	}
	appSecret, err := decryptToken(cfg.AppSecretEncrypted, decrypt)
	if err != nil {
		return credentials{}, fmt.Errorf("decrypt app secret: %w", err)
	}
	return credentials{
		AppID:           cfg.AppID,
		AppKey:          cfg.AppKey,
		AppSecret:       appSecret,
		BotUserID:       cfg.BotUserID,
		BotMentionNames: splitMentionNames(cfg.BotMentionNames),
		AgentID:         cfg.AgentID,
	}, nil
}

// splitMentionNames parses the comma-separated bot_mention_names config into a
// trimmed, non-empty list.
func splitMentionNames(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if name := strings.TrimSpace(part); name != "" {
			out = append(out, name)
		}
	}
	return out
}

// normalizeMentionNames canonicalizes a comma-separated bot_mention_names
// value for storage (trimmed, de-duplicated, comma-joined).
func normalizeMentionNames(raw string) string {
	return strings.Join(splitMentionNames(raw), ",")
}

// decryptToken base64-decodes the stored ciphertext (tolerating the MIME
// newline wrapping PostgreSQL's encode(...,'base64') emits) and runs it through
// the injected Decrypter. An empty stored value decodes to an empty secret; a
// nil Decrypter treats the decoded bytes as plaintext (test convenience).
func decryptToken(enc string, decrypt Decrypter) (string, error) {
	if enc == "" {
		return "", nil
	}
	ciphertext, err := base64.StdEncoding.DecodeString(stripWhitespace(enc))
	if err != nil {
		return "", fmt.Errorf("base64 decode: %w", err)
	}
	if decrypt == nil {
		return string(ciphertext), nil
	}
	plaintext, err := decrypt(ciphertext)
	if err != nil {
		return "", err
	}
	return string(plaintext), nil
}

// stripWhitespace removes ASCII whitespace so a MIME-wrapped base64 string
// (newlines every 64 chars) and an unwrapped one decode identically.
func stripWhitespace(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch r {
		case ' ', '\t', '\n', '\r':
			continue
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
