package infoflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/multica-ai/multica/server/internal/integrations/channel"
)

// infoflowChannel is ONE installation's InfoFlow long connection. Every
// installation carries its own AppKey/AppSecret in the installation config and
// gets its own connection, exactly like the per-installation Feishu, Slack and
// DingTalk adapters. engine.Supervisor builds one infoflowChannel per active
// installation and owns lease/reconnect lifecycle; Connect blocks until the run
// context is cancelled.
type infoflowChannel struct {
	appID     string
	appKey    string
	appSecret string
	botUserID string
	client    *Client
	handler   channel.InboundHandler
	logger    *slog.Logger
}

func (c *infoflowChannel) Type() channel.Type { return TypeInfoFlow }

func (c *infoflowChannel) Capabilities() channel.Capability {
	return channel.CapText
}

func (c *infoflowChannel) Disconnect(context.Context) error { return nil }

// ErrSendRequiresTarget is returned by Channel.Send when it cannot tell which
// conversation kind the caller means. The channel.OutboundMessage envelope
// carries only a ChatID, and an InfoFlow group id and a single-chat user id are
// not distinguishable from the string alone — sending a 1:1 reply down the
// group endpoint (or the reverse) would silently address the wrong API. Callers
// that know the conversation kind must use SendToTarget; the generic Send path
// only serves group destinations.
var ErrSendRequiresTarget = errors.New("infoflow: channel.Send needs a conversation kind; use SendToTarget")

func (c *infoflowChannel) Send(ctx context.Context, out channel.OutboundMessage) (channel.SendResult, error) {
	if strings.TrimSpace(out.ChatID) == "" {
		return channel.SendResult{}, ErrSendRequiresTarget
	}
	// Generic Send serves group destinations only: the envelope cannot express
	// the conversation kind, and a p2p reply must go through the application
	// send API instead. Prefer SendToTarget when the kind is known.
	return c.SendToTarget(ctx, infoflowSendTarget{ChatType: string(channel.ChatTypeGroup), ChatID: out.ChatID}, out.Text)
}

// SendToTarget delivers text to an explicit InfoFlow destination (group or
// 1:1). The outbound subscriber and the replier use it because they recover the
// conversation kind from the binding config.
func (c *infoflowChannel) SendToTarget(ctx context.Context, target infoflowSendTarget, text string) (channel.SendResult, error) {
	msgID, err := (&sender{client: c.client, appKey: c.appKey, appSecret: c.appSecret, agentID: c.appID}).sendText(ctx, target, text)
	if err != nil {
		return channel.SendResult{}, err
	}
	return channel.SendResult{MessageID: msgID}, nil
}

// Connect starts the InfoFlow two-phase WS connection and blocks until ctx is
// cancelled or the socket ends.
func (c *infoflowChannel) Connect(ctx context.Context) error {
	if c.handler == nil {
		return errors.New("infoflow: inbound handler not configured")
	}
	if c.appKey == "" || c.appSecret == "" {
		return errors.New("infoflow: app key and secret not configured")
	}
	conn := &wsConnector{
		httpClient: c.client.httpClient,
		appKey:     c.appKey,
		appSecret:  c.appSecret,
		logger:     c.logger,
		onFrame: func(ctx context.Context, frame Frame) error {
			raw, err := parseFramePayload(frame)
			if err != nil {
				c.logger.InfoContext(ctx, "infoflow: dropped non-payload frame", "error", err)
				return nil
			}
			msg, dropReason, ok := inboundFromPayloadReason(raw, c.appID, c.botUserID)
			if !ok {
				// The payload could not even be turned into a message (missing
				// sender/content/group id, or malformed). Name the reason so a
				// silent "the bot ignores me" report is traceable. Messages that
				// DO normalize — including group messages not addressed to the
				// bot — are routed on purpose: the engine owns that drop, so it
				// records the audit row and runs the dedup gate.
				c.logger.InfoContext(ctx, "infoflow: frame could not be normalized",
					"drop_reason", dropReason,
					"installation_id", c.appID,
					"bot_user_id_configured", c.botUserID != "",
					"hint", "check bot_user_id and the robot's message-receive configuration",
				)
				return nil
			}
			return c.handler(ctx, msg)
		},
	}
	return conn.run(ctx)
}

// ChannelDeps are the shared dependencies the InfoFlow Factory closes over.
type ChannelDeps struct {
	Decrypt Decrypter
	Client  *Client
	Logger  *slog.Logger
}

// Register builds/registers the per-installation InfoFlow factory so
// engine.Supervisor builds one infoflowChannel per active installation.
func Register(reg *channel.Registry, deps ChannelDeps) {
	reg.Register(TypeInfoFlow, newFactory(deps))
}

func newFactory(deps ChannelDeps) channel.Factory {
	logger := deps.Logger
	if logger == nil {
		logger = slog.Default()
	}
	client := deps.Client
	if client == nil {
		client = NewClient(nil, "")
	}
	return func(cfg channel.Config) (channel.Channel, error) {
		creds, err := decodeCredentials(cfg.Raw, deps.Decrypt)
		if err != nil {
			return nil, err
		}
		if creds.AppKey == "" || creds.AppSecret == "" {
			return nil, errors.New("infoflow: installation has empty app key or app secret")
		}
		return &infoflowChannel{
			appID:     creds.AppID,
			appKey:    creds.AppKey,
			appSecret: creds.AppSecret,
			botUserID: creds.BotUserID,
			client:    client,
			handler:   cfg.Handler,
			logger:    logger,
		}, nil
	}
}

var _ channel.Channel = (*infoflowChannel)(nil)

// UnmarshalInstallationConfig is exported for the install service and tests
// to normalize a raw JSON blob into the opaque installConfig without exposing
// decrypted secrets.
func UnmarshalInstallationConfig(raw json.RawMessage) (installConfig, error) {
	var cfg installConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return installConfig{}, fmt.Errorf("infoflow: decode installation config: %w", err)
	}
	return cfg, nil
}
