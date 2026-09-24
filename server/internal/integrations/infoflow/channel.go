package infoflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

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

func (c *infoflowChannel) Send(ctx context.Context, out channel.OutboundMessage) (channel.SendResult, error) {
	// The Channel contract carries only ChatID and text; InfoFlow's minimal Send
	// path sends to the chat as a group message. Richer P2P routing is handled by
	// the outbound subscriber, which reads the binding config.
	target := infoflowSendTarget{ChatType: string(channel.ChatTypeGroup), ChatID: out.ChatID}
	msgID, err := (&sender{client: c.client, appKey: c.appKey, appSecret: c.appSecret, agentID: c.appID}).sendText(ctx, target, out.Text)
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
			msg, ok := inboundFromPayload(raw, c.appID, c.botUserID)
			if !ok {
				c.logger.InfoContext(ctx, "infoflow: dropped unsupported inbound message")
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
