package infoflow

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/integrations/channel"
	"github.com/multica-ai/multica/server/internal/integrations/channel/engine"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// This file is the InfoFlow OutboundReplier — the engine seam that delivers a
// verdict-driven reply back to the sender. It mirrors the Slack / Telegram
// repliers and closes the binding loop the router depends on:
//
//   - NeedsBinding: mint a single-use binding token and reply with the link to
//     the in-product redeem page (/infoflow/bind). Without this the binding
//     page and POST /api/infoflow/binding/redeem are unreachable dead paths.
//   - AgentOffline / AgentArchived: a status notice so the user is not left
//     wondering why nothing happened.
//   - FreshPending / ChatStarted / IssueUsage: command confirmation or
//     corrective guidance.
//   - Ingested with an /issue result: creation or duplicate confirmation.
//   - Dropped addressed /issue commands: an authorization/status refusal.
const (
	agentOfflineText     = "⚠️ The agent is offline, so this message won't be processed automatically."
	agentArchivedText    = "⚠️ This agent has been archived and can't respond. Please contact your workspace admin."
	freshPendingText     = "✅ Fresh start ready. Your next chat message will run without previous context."
	chatStartedText      = "✅ Started a new Multica chat. Your next message will enter it."
	issueUsageText       = "Please include an issue title. Use:\n\n`/issue <title>`\n\n`[description]` (optional)"
	issueNotMemberText   = "You're not a member of this Multica workspace, so I can't file an issue for you. Ask a workspace admin to invite you, then send the command again."
	issueDisabledText    = "This InfoFlow robot isn't connected to Multica (or was disconnected). Ask the agent owner or a workspace owner/admin to reconnect it."
	bindingGroupHintText = "👋 To link your InfoFlow account to Multica, start a private chat with me and send any message — I'll reply with a personal link there."
)

// bindingMinter is the binding-token surface the replier needs.
// *BindingTokenService satisfies it.
type bindingMinter interface {
	Mint(ctx context.Context, workspaceID, installationID pgtype.UUID, infoflowUserID string) (BindingToken, error)
}

// OutboundReplier implements engine.OutboundReplier for InfoFlow.
type OutboundReplier struct {
	binding     bindingMinter
	decrypt     Decrypter
	client      *Client
	appURL      string
	bindingPath string
	logger      *slog.Logger
}

// OutboundReplierConfig configures the replier. Binding + AppURL are required
// for the NeedsBinding prompt to work; without them the prompt is skipped (the
// status and command notices still fire).
type OutboundReplierConfig struct {
	Binding bindingMinter
	Decrypt Decrypter
	Client  *Client
	// AppURL is the Multica web app host the user clicks into to redeem the
	// binding token (e.g. https://multica.example). The bind page
	// (/infoflow/bind) is served by the web app, so the link must point at the
	// app host, not the API host.
	AppURL      string
	BindingPath string // default "/infoflow/bind"
	Logger      *slog.Logger
}

var _ engine.OutboundReplier = (*OutboundReplier)(nil)

// NewOutboundReplier builds the replier.
func NewOutboundReplier(cfg OutboundReplierConfig) *OutboundReplier {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	client := cfg.Client
	if client == nil {
		client = NewClient(nil, "")
	}
	bindingPath := cfg.BindingPath
	if bindingPath == "" {
		bindingPath = "/infoflow/bind"
	}
	if !strings.HasPrefix(bindingPath, "/") {
		bindingPath = "/" + bindingPath
	}
	return &OutboundReplier{
		binding:     cfg.Binding,
		decrypt:     cfg.Decrypt,
		client:      client,
		appURL:      strings.TrimRight(cfg.AppURL, "/"),
		bindingPath: bindingPath,
		logger:      logger,
	}
}

// Reply routes each outcome to its user-visible message. Errors are logged, not
// propagated: the replier runs detached from the inbound ACK path.
func (r *OutboundReplier) Reply(ctx context.Context, inst engine.ResolvedInstallation, msg channel.InboundMessage, res engine.Result) {
	switch res.Outcome {
	case engine.OutcomeNeedsBinding:
		if err := r.sendBindingPrompt(ctx, inst, msg, res); err != nil {
			r.logger.WarnContext(ctx, "infoflow replier: binding prompt failed",
				"installation_id", util.UUIDToString(inst.ID), "error", err)
		}
	case engine.OutcomeAgentOffline:
		if err := r.post(ctx, inst, msg, agentOfflineText); err != nil {
			r.logger.WarnContext(ctx, "infoflow replier: offline notice failed",
				"installation_id", util.UUIDToString(inst.ID), "error", err)
		}
	case engine.OutcomeAgentArchived:
		if err := r.post(ctx, inst, msg, agentArchivedText); err != nil {
			r.logger.WarnContext(ctx, "infoflow replier: archived notice failed",
				"installation_id", util.UUIDToString(inst.ID), "error", err)
		}
	case engine.OutcomeFreshPending:
		if err := r.post(ctx, inst, msg, freshPendingText); err != nil {
			r.logger.WarnContext(ctx, "infoflow replier: fresh-start confirmation failed",
				"installation_id", util.UUIDToString(inst.ID), "error", err)
		}
	case engine.OutcomeChatStarted:
		if err := r.post(ctx, inst, msg, chatStartedText); err != nil {
			r.logger.WarnContext(ctx, "infoflow replier: new-chat confirmation failed",
				"installation_id", util.UUIDToString(inst.ID), "error", err)
		}
	case engine.OutcomeIssueUsage:
		if err := r.post(ctx, inst, msg, issueUsageText); err != nil {
			r.logger.WarnContext(ctx, "infoflow replier: issue usage reply failed",
				"installation_id", util.UUIDToString(inst.ID), "error", err)
		}
	case engine.OutcomeIngested:
		if res.IssueID.Valid {
			text := issueCreatedText(res)
			if res.IssueDuplicate {
				text = issueDuplicateText(res)
			}
			if err := r.post(ctx, inst, msg, text); err != nil {
				r.logger.WarnContext(ctx, "infoflow replier: issue outcome reply failed",
					"installation_id", util.UUIDToString(inst.ID), "error", err)
			}
		}
	case engine.OutcomeDropped:
		if text := droppedReplyText(res, msg); text != "" {
			if err := r.post(ctx, inst, msg, text); err != nil {
				r.logger.WarnContext(ctx, "infoflow replier: drop refusal failed",
					"installation_id", util.UUIDToString(inst.ID), "error", err)
			}
		}
	}
}

func (r *OutboundReplier) sendBindingPrompt(ctx context.Context, inst engine.ResolvedInstallation, msg channel.InboundMessage, res engine.Result) error {
	// A group-visible bearer link could be redeemed by another group member and
	// would bind the original sender's InfoFlow identity to the wrong Multica
	// user. Group prompts only point the user at a private chat.
	if msg.Source.ChatType == channel.ChatTypeGroup {
		return r.post(ctx, inst, msg, bindingGroupHintText)
	}
	sender := res.Sender
	if sender == "" {
		sender = msg.Source.SenderID
	}
	if sender == "" {
		return errors.New("missing sender id")
	}
	if r.binding == nil {
		return errors.New("binding service not configured")
	}
	if r.appURL == "" {
		return errors.New("app url not configured")
	}
	token, err := r.binding.Mint(ctx, inst.WorkspaceID, inst.ID, sender)
	if err != nil {
		return fmt.Errorf("mint binding token: %w", err)
	}
	bindURL := r.appURL + r.bindingPath + "?token=" + url.QueryEscape(token.Raw)
	text := "👋 To start chatting with me, link your InfoFlow account to Multica:\n" + bindURL + "\n(This link expires in 15 minutes.)"
	// Deliver the single-use link privately (1:1) to the sender, never into the
	// group where any member could redeem it.
	target := infoflowSendTarget{ChatType: string(channel.ChatTypeP2P), ChatID: sender, SenderID: sender}
	return r.sendToTarget(ctx, inst, target, text)
}

// post resolves the installation's credentials from the carried platform row
// and sends text back into the originating conversation.
func (r *OutboundReplier) post(ctx context.Context, inst engine.ResolvedInstallation, msg channel.InboundMessage, text string) error {
	return r.sendToTarget(ctx, inst, targetFromMessage(msg), text)
}

func (r *OutboundReplier) sendToTarget(ctx context.Context, inst engine.ResolvedInstallation, target infoflowSendTarget, text string) error {
	row, ok := inst.Platform.(db.ChannelInstallation)
	if !ok {
		return errors.New("installation platform row unavailable")
	}
	creds, err := decodeCredentials(row.Config, r.decrypt)
	if err != nil {
		return fmt.Errorf("decode credentials: %w", err)
	}
	s := &sender{client: r.client, appKey: creds.AppKey, appSecret: creds.AppSecret, agentID: creds.AgentID}
	if _, err := s.sendText(ctx, target, text); err != nil {
		return fmt.Errorf("post infoflow reply: %w", err)
	}
	return nil
}

// targetFromMessage builds the reply target from the inbound message's own
// routing identity (used for immediate binding/status replies, before any chat
// binding exists).
func targetFromMessage(msg channel.InboundMessage) infoflowSendTarget {
	target := infoflowSendTarget{ChatType: string(channel.ChatTypeGroup), ChatID: msg.Source.ChatID}
	if msg.Source.ChatType == channel.ChatTypeP2P {
		target.ChatType = string(channel.ChatTypeP2P)
		target.SenderID = msg.Source.SenderID
	}
	return target
}

// isAddressedIssueCommand reports whether msg is an /issue command explicitly
// addressed to the bot — only such messages warrant an error/refusal reply.
func isAddressedIssueCommand(msg channel.InboundMessage) bool {
	if !msg.AddressedToBot {
		return false
	}
	source := msg.CommandText
	if source == "" {
		source = msg.Text
	}
	_, ok := engine.ParseIssueCommand(source)
	return ok
}

func droppedReplyText(res engine.Result, msg channel.InboundMessage) string {
	if !isAddressedIssueCommand(msg) {
		return ""
	}
	switch res.DropReason {
	case engine.DropReasonNonWorkspaceMember:
		return issueNotMemberText
	case engine.DropReasonRevokedInstallation:
		return issueDisabledText
	default:
		return ""
	}
}

func issueCreatedText(res engine.Result) string {
	id := issueResultIdentifier(res)
	title := strings.TrimSpace(res.IssueTitle)
	if title == "" {
		return "✅ Created " + id
	}
	return "✅ Created " + id + " — " + title
}

func issueDuplicateText(res engine.Result) string {
	id := issueResultIdentifier(res)
	title := strings.TrimSpace(res.IssueTitle)
	if title == "" {
		return "⚠️ Not created — active issue " + id + " already exists."
	}
	return "⚠️ Not created — active issue " + id + " already exists: " + title
}

func issueResultIdentifier(res engine.Result) string {
	if res.IssueIdentifier != "" {
		return res.IssueIdentifier
	}
	if res.IssueNumber > 0 {
		return fmt.Sprintf("#%d", res.IssueNumber)
	}
	return util.UUIDToString(res.IssueID)
}
