package infoflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
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
	appID           string
	appKey          string
	appSecret       string
	botUserID       string
	botMentionNames []string
	client          *Client
	groupIdentity   *groupIdentityResolver
	quoteEnricher   *quoteEnricher
	// workers delivers normalized messages out of the read loop (DEC7). It is
	// created in Connect and nil in unit tests that drive handleFrame directly,
	// which keeps those tests synchronous.
	workers *quoteWorkers
	handler channel.InboundHandler
	logger  *slog.Logger
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
	c.workers = newQuoteWorkers(ctx, defaultQuoteWorkers, c.logger)
	defer func() {
		c.workers.stop()
		c.workers = nil
	}()
	conn := &wsConnector{
		httpClient: c.client.httpClient,
		appKey:     c.appKey,
		appSecret:  c.appSecret,
		logger:     c.logger,
		onFrame:    c.handleFrameQueued,
	}
	return conn.run(ctx)
}

// handleFrame is the synchronous frame entry used by tests and by channels
// without workers: normalize, enrich, then handler.
func (c *infoflowChannel) handleFrame(ctx context.Context, frame Frame) error {
	raw, err := parseFramePayload(frame)
	if err != nil {
		c.logger.InfoContext(ctx, "infoflow: dropped non-payload frame", "error", err)
		return nil
	}
	return c.processFrame(ctx, raw)
}

// handleFrameQueued is the WebSocket read-loop entry. It only parses the payload
// and derives the conversation key, then returns; normalization (which may call
// the group member list) and the quoted-message lookup run on the bounded
// workers, so the read loop never waits for either (DEC7).
func (c *infoflowChannel) handleFrameQueued(ctx context.Context, frame Frame) error {
	raw, err := parseFramePayload(frame)
	if err != nil {
		c.logger.InfoContext(ctx, "infoflow: dropped non-payload frame", "error", err)
		return nil
	}
	if c.workers == nil {
		return c.processFrame(ctx, raw)
	}
	c.workers.enqueue(chatKeyFromRaw(raw), func() {
		if err := c.processFrame(ctx, raw); err != nil {
			c.logger.WarnContext(ctx, "infoflow: inbound delivery failed", "error", err)
		}
	})
	return nil
}

// processFrame normalizes one payload, resolves a selected quote and invokes the
// handler. It never returns an error for a payload it cannot use.
func (c *infoflowChannel) processFrame(ctx context.Context, raw json.RawMessage) error {
	c.logInboundShape(ctx, raw)
	// Enrich the self-identity set with the group-scoped identity before
	// normalization: InfoFlow's group AT element usually carries only
	// the robot's DISPLAY NAME, which the configured id cannot match.
	// Extra tokens are harmless for p2p (mention checking is skipped).
	self := newIdentitySet(c.botUserID, c.botMentionNames)
	if groupID := groupIDFromRaw(raw); groupID != "" && c.groupIdentity != nil {
		if tokens, _ := c.groupIdentity.lookup(ctx, groupID); len(tokens) > 0 {
			self = self.withResolved(tokens)
		}
	}
	msg, dropReason, ok := inboundFromPayloadReasonWithIdentity(raw, c.appID, self)
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
	return c.deliver(ctx, msg, raw, self)
}

// logInboundShape records one redacted shape line per inbound frame so the live
// payload can be identified without another blind round-trip (N88/A29). Only
// type names, key names and presence flags are logged: never content, URLs,
// user ids or full message objects.
func (c *infoflowChannel) logInboundShape(ctx context.Context, raw json.RawMessage) {
	if c.logger == nil {
		return
	}
	var payload infoflowRaw
	_ = json.Unmarshal(raw, &payload)
	chatType := "p2p"
	headerID := anyString(payload.MsgID)
	replyPresent, quotePresent := false, false
	var body []map[string]any
	if payload.Message != nil {
		chatType = "p2p"
		if payload.GroupID != nil || payload.Message.Header.ToID != nil {
			chatType = "group"
		}
		headerID = anyString(payload.Message.Header.MessageID)
		body = payload.Message.Body
		replyPresent = payload.Message.Reply != nil
		quotePresent = payload.Message.Quote != nil
	}

	bodyTypes := make([]string, 0, len(body))
	keysByType := map[string][]string{}
	seenType := map[string]bool{}
	for _, item := range body {
		if item == nil {
			continue
		}
		typeName := strings.ToUpper(stringOrEmpty(item["type"]))
		if typeName == "" {
			typeName = "UNKNOWN"
		}
		if !seenType[typeName] {
			seenType[typeName] = true
			bodyTypes = append(bodyTypes, typeName)
		}
		for key := range item {
			if !containsString(keysByType[typeName], key) {
				keysByType[typeName] = append(keysByType[typeName], key)
			}
		}
	}
	sort.Strings(bodyTypes)
	for typeName := range keysByType {
		sort.Strings(keysByType[typeName])
	}

	quoteFields := []string{}
	if element := quoteElementFromBody(body); element != nil {
		quoteFields = shapeFields(element, "")
		sort.Strings(quoteFields)
	}
	fromIDSource := ""
	if payload.Message != nil && strings.TrimSpace(payload.Message.Header.FromUserID) != "" {
		fromIDSource = "current_sender"
	} else if strings.TrimSpace(payload.FromUserID) != "" {
		fromIDSource = "current_sender"
	}

	c.logger.InfoContext(ctx, "infoflow: inbound frame shape",
		"chat_type", chatType,
		"header_messageid", headerID,
		"reply_present", replyPresent,
		"quote_present", quotePresent,
		"body_types", bodyTypes,
		"body_keys", keysByType,
		"quote_element_fields", quoteFields,
		"quote_from_id_source", fromIDSource,
	)
}

// shapeFields lists key names, prefixing one level of nested objects with
// "parent.". It never reads a value, so no content/URL/user id can leak.
func shapeFields(m map[string]any, prefix string) []string {
	fields := make([]string, 0, len(m))
	for key, value := range m {
		fields = append(fields, prefix+key)
		nested, ok := value.(map[string]any)
		if !ok || prefix != "" {
			continue
		}
		for nestedKey := range nested {
			fields = append(fields, key+"."+nestedKey)
		}
	}
	return fields
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

// chatKeyFromRaw derives the conversation id before normalization so the queue
// can keep per-chat order without waiting for the group identity lookup.
func chatKeyFromRaw(raw json.RawMessage) string {
	var payload struct {
		GroupID    string `json:"groupid"`
		FromUserID string `json:"FromUserId"`
		Message    *struct {
			Header *struct {
				FromUserID string `json:"fromuserid"`
			} `json:"header"`
		} `json:"message"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return ""
	}
	if key := strings.TrimSpace(payload.GroupID); key != "" {
		return key
	}
	if key := strings.TrimSpace(payload.FromUserID); key != "" {
		return key
	}
	if payload.Message != nil && payload.Message.Header != nil {
		return strings.TrimSpace(payload.Message.Header.FromUserID)
	}
	return ""
}

// deliver resolves the quoted message (when selected) and invokes the handler.
// It runs on a worker goroutine in production, preserving per-chat order.
func (c *infoflowChannel) deliver(ctx context.Context, msg channel.InboundMessage, raw json.RawMessage, self identitySet) error {
	if msg.HasSelectedContext && c.quoteEnricher != nil {
		msg = c.enrichQuoteOrigin(ctx, msg, raw, self)
	}
	return c.handler(ctx, msg)
}

// enrichQuoteOrigin looks the quoted message up and re-renders the inbound text
// with its origin. The lookup runs on a detached, bounded context so neither a
// cancelled WebSocket nor a slow platform call can take the main message down;
// whatever the outcome, the handler still receives a message.
func (c *infoflowChannel) enrichQuoteOrigin(ctx context.Context, msg channel.InboundMessage, raw json.RawMessage, self identitySet) channel.InboundMessage {
	var payload infoflowRaw
	if err := json.Unmarshal(raw, &payload); err != nil {
		return msg
	}
	if anyString(payload.AppID) == "" {
		payload.AppID = c.appID
	}
	quote := quoteForPayload(payload)
	if quote == nil {
		return msg
	}
	origin := &quoteOrigin{State: quoteStateSkipped}
	if query, ok := quoteQueryFor(msg, payload, quote); ok {
		enrichCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), quoteEnrichBudget)
		origin = c.quoteEnricher.Enrich(enrichCtx, quoteEnrichRequest{
			AppID:     c.appID,
			AppKey:    c.appKey,
			AppSecret: c.appSecret,
			Query:     query,
		})
		cancel()
	}
	enriched, ok := attachQuoteOrigin(msg, origin, c.appID, self)
	if !ok {
		return msg
	}
	c.logQuoteOrigin(ctx, origin)
	return enriched
}

// logQuoteOrigin records the outcome without logging quoted content.
func (c *infoflowChannel) logQuoteOrigin(ctx context.Context, origin *quoteOrigin) {
	if origin == nil || c.logger == nil {
		return
	}
	c.logger.InfoContext(ctx, "infoflow: quote origin resolved",
		"quote_origin", string(origin.State),
		"source", origin.Source,
		"elapsed_ms", origin.ElapsedMS,
		"msg_type", origin.MsgType,
		"installation_id", c.appID,
	)
}

// ChannelDeps are the shared dependencies the InfoFlow Factory closes over.
type ChannelDeps struct {
	Decrypt Decrypter
	Client  *Client
	Logger  *slog.Logger
	// QuoteEnrich injects the quoted-message full-text resolver. A nil value
	// builds the default resolver unless MULTICA_INFOFLOW_QUOTE_ORIGIN=off, so
	// the feature works in every environment without editing shared wiring.
	QuoteEnrich *quoteEnricher
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
	enricher := deps.QuoteEnrich
	if enricher == nil && quoteOriginEnabled() {
		// One resolver per process: the concurrency cap and the cache are
		// shared across installations.
		enricher = newQuoteEnricher(client, logger)
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
			appID:           creds.AppID,
			appKey:          creds.AppKey,
			appSecret:       creds.AppSecret,
			botUserID:       creds.BotUserID,
			botMentionNames: creds.BotMentionNames,
			client:          client,
			groupIdentity:   newGroupIdentityResolver(client, creds.AppKey, creds.AppSecret, creds.AppID),
			quoteEnricher:   enricher,
			handler:         cfg.Handler,
			logger:          logger,
		}, nil
	}
}

// groupIDFromRaw extracts the group id from a raw frame payload without
// committing to a full normalization (the group id may arrive top-level or in
// the message header). Empty means p2p or not a group frame.
func groupIDFromRaw(raw json.RawMessage) string {
	var payload infoflowRaw
	if err := json.Unmarshal(raw, &payload); err != nil {
		return ""
	}
	if !isGroupPayload(payload) {
		return ""
	}
	if groupID := anyString(payload.GroupID); groupID != "" {
		return groupID
	}
	return anyString(payload.Message.Header.ToID)
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
