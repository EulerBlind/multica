package infoflow

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/integrations/channel"
	"github.com/multica-ai/multica/server/internal/integrations/channel/engine"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

// originInfoFlowChat is the issue.origin_type label for issues created through
// the InfoFlow /issue command. Keep it aligned with the database CHECK
// constraint, like the existing lark_chat, slack_chat and dingtalk_chat origin
// labels.
const originInfoFlowChat = "infoflow_chat"

// NewResolverSet assembles the InfoFlow ResolverSet over the generated queries
// plus a tx starter. Installation/identity/dedup/session/audit are required;
// Replier delivers the verdict-driven replies (notably the binding prompt for
// an unbound sender); media and typing are omitted until the InfoFlow SDK/API
// surface for those capabilities is settled.
func NewResolverSet(q *db.Queries, tx engine.TxStarter, replier engine.OutboundReplier, logger *slog.Logger) engine.ResolverSet {
	return engine.ResolverSet{
		Installation: &installationResolver{q: q},
		Identity:     &identityResolver{q: q, logger: logger},
		Dedup:        &deduper{q: q},
		Session: &sessionBinder{session: engine.NewChatSession(q, tx, TypeInfoFlow, engine.SessionTitles{
			Group:    "InfoFlow group",
			Direct:   "InfoFlow direct message",
			Fallback: "InfoFlow chat",
		})},
		Audit:      &auditor{q: q, logger: logger},
		Replier:    replier,
		OriginType: originInfoFlowChat,
	}
}

var (
	_ engine.InstallationResolver = (*installationResolver)(nil)
	_ engine.IdentityResolver     = (*identityResolver)(nil)
	_ engine.Deduper              = (*deduper)(nil)
	_ engine.SessionBinder        = (*sessionBinder)(nil)
	_ engine.Auditor              = (*auditor)(nil)
)

// groupRoutingSeparator joins the group id and the sender id into the group
// session binding key. The engine treats BindingKey as an opaque
// session-isolation key (one chat_session per (installation, BindingKey)) and
// stores it in channel_chat_id, so the adapter can decide group isolation
// without touching the shared engine. `:` cannot appear in a numeric group id,
// and the whole string is compared for equality, so a sender id containing a
// colon still yields a unique key.
const groupRoutingSeparator = ":"

// infoflowBindingConfig is the opaque outbound routing persisted on the chat
// binding's config: enough to address a proactive reply back into the
// originating conversation.
//
//   - ChatID is ALWAYS the real addressable destination (the group id for a
//     group, the peer user id for a 1:1). It is never the composite routing key.
//   - SenderID is the 1:1 recipient for a direct chat, and the AT target for a
//     group reply (the InfoFlow user id the bot must @ mention).
//   - MentionUserID is the group AT target; kept separate from SenderID so the
//     1:1 semantics stay intact.
type infoflowBindingConfig struct {
	ChatType      string `json:"chat_type"`
	ChatID        string `json:"chat_id"`
	SenderID      string `json:"sender_id,omitempty"`
	MentionUserID string `json:"mention_user_id,omitempty"`
}

func infoflowSessionRouting(msg channel.InboundMessage) (bindingKey string, config []byte) {
	chatID := msg.Source.ChatID
	cfg := infoflowBindingConfig{ChatType: string(msg.Source.ChatType), ChatID: chatID}
	if msg.Source.ChatType == channel.ChatTypeP2P {
		cfg.SenderID = msg.Source.SenderID
	} else {
		// A group is isolated per (group, sender): A's follow-ups must not land
		// in B's transcript, and a reply must @ the person who asked. SenderID
		// stays the identity/binding key; the AT target rides along separately.
		cfg.SenderID = msg.Source.SenderID
		cfg.MentionUserID = msg.Source.SenderID
		if sender := strings.TrimSpace(msg.Source.SenderID); sender != "" {
			chatID = chatID + groupRoutingSeparator + sender
		}
	}
	raw, _ := json.Marshal(cfg)
	return chatID, raw
}

func outboundTarget(b db.ChannelChatSessionBinding) infoflowSendTarget {
	target := infoflowSendTarget{ChatType: b.ChatType, ChatID: b.ChannelChatID}
	if len(b.Config) > 0 {
		var cfg infoflowBindingConfig
		if err := json.Unmarshal(b.Config, &cfg); err == nil {
			if cfg.ChatType != "" {
				target.ChatType = cfg.ChatType
			}
			if cfg.ChatID != "" {
				target.ChatID = cfg.ChatID
			}
			target.SenderID = cfg.SenderID
			target.MentionUserID = cfg.MentionUserID
		}
	}
	// Compatibility: a legacy group binding stored the plain group id as its
	// key, with no config ChatID/sender. Split the composite key back so a
	// previously-written group binding still addresses the real group and, when
	// the key carries a sender, @ that person.
	target.ChatID, target.MentionUserID = resolveGroupTarget(target.ChatType, target.ChatID, target.MentionUserID)
	return target
}

// resolveGroupTarget normalizes a group destination that may be a composite
// routing key (`<group>:<sender>`) into the real group id plus the AT target.
// Plain ids and non-group destinations pass through unchanged.
func resolveGroupTarget(chatType, chatID, mentionUserID string) (string, string) {
	if chatType == string(channel.ChatTypeP2P) {
		return chatID, mentionUserID
	}
	groupID, sender, ok := splitGroupRoutingKey(chatID)
	if !ok {
		return chatID, mentionUserID
	}
	if mentionUserID == "" {
		mentionUserID = sender
	}
	return groupID, mentionUserID
}

// splitGroupRoutingKey splits `<group>:<sender>` and reports whether the value
// was composite. A value without the separator is a legacy plain group id.
func splitGroupRoutingKey(key string) (groupID, sender string, ok bool) {
	groupID, sender, ok = strings.Cut(key, groupRoutingSeparator)
	if !ok {
		return strings.TrimSpace(key), "", false
	}
	return strings.TrimSpace(groupID), strings.TrimSpace(sender), true
}

func decodeInfoFlowRaw(msg channel.InboundMessage) (infoflowRaw, error) {
	var raw infoflowRaw
	if len(msg.Raw) == 0 {
		return infoflowRaw{}, errors.New("infoflow: inbound message Raw is empty")
	}
	if err := json.Unmarshal(msg.Raw, &raw); err != nil {
		return infoflowRaw{}, err
	}
	return raw, nil
}

func nullText(s string) pgtype.Text {
	if s == "" {
		return pgtype.Text{}
	}
	return pgtype.Text{String: s, Valid: true}
}

type installationResolver struct{ q *db.Queries }

func (r *installationResolver) ResolveInstallation(ctx context.Context, msg channel.InboundMessage) (engine.ResolvedInstallation, error) {
	raw, err := decodeInfoFlowRaw(msg)
	if err != nil {
		return engine.ResolvedInstallation{}, err
	}
	inst, err := r.q.GetChannelInstallationByAppID(ctx, db.GetChannelInstallationByAppIDParams{
		ChannelType: string(TypeInfoFlow),
		AppID:       rawAppIDString(raw.AppID),
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return engine.ResolvedInstallation{}, engine.ErrInstallationNotFound
		}
		return engine.ResolvedInstallation{}, err
	}
	return engine.ResolvedInstallation{
		ID:              inst.ID,
		WorkspaceID:     inst.WorkspaceID,
		AgentID:         inst.AgentID,
		InstallerUserID: inst.InstallerUserID,
		Active:          inst.Status == "active",
		Platform:        inst,
	}, nil
}

// identityQueries is the narrow slice of generated queries ResolveSender needs.
// Keeping it an interface lets the identity ladder be unit-tested without a
// database; *db.Queries satisfies it.
type identityQueries interface {
	GetChannelUserBindingByUserID(ctx context.Context, arg db.GetChannelUserBindingByUserIDParams) (db.ChannelUserBinding, error)
	GetMemberByUserAndWorkspace(ctx context.Context, arg db.GetMemberByUserAndWorkspaceParams) (db.Member, error)
}

type identityResolver struct {
	q      identityQueries
	logger *slog.Logger
}

// ResolveSender maps an InfoFlow sender to the Multica user a run executes as.
//
// InfoFlow deliberately has NO binding gate (QIA-600 N11/N12): the product
// requirement is that single- and group-chat messages reach the Agent without
// a prior account-link step. The identity ladder is:
//
//  1. an explicit channel_user_binding, when one exists (optional enhancement,
//     still re-checked for workspace membership);
//  2. otherwise the installation's installer — the workspace member who
//     connected the bot and therefore owns what it may act on.
//
// Only an installation whose installer is missing falls back to
// ErrSenderUnbound; that is a data defect, not a product path. A binding that
// points at a non-member stays ErrSenderNotMember, so a removed member cannot
// keep driving the Agent through a stale binding.
//
// The fallback is logged with the installation/sender/chat coordinates
// (D-N11-A option 1) so "who really sent this" stays answerable in ops logs
// without widening the message tables.
func (r *identityResolver) ResolveSender(ctx context.Context, inst engine.ResolvedInstallation, msg channel.InboundMessage) (engine.ResolvedIdentity, error) {
	binding, err := r.q.GetChannelUserBindingByUserID(ctx, db.GetChannelUserBindingByUserIDParams{
		InstallationID: inst.ID,
		ChannelUserID:  msg.Source.SenderID,
	})
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			return engine.ResolvedIdentity{}, err
		}
		if !inst.InstallerUserID.Valid {
			return engine.ResolvedIdentity{}, engine.ErrSenderUnbound
		}
		r.loggerFor().InfoContext(ctx, "infoflow: unbound sender resolved to the installation installer",
			"installation_id", uuidOrEmpty(inst.ID),
			"sender_id", msg.Source.SenderID,
			"chat_type", string(msg.Source.ChatType),
			"chat_id", msg.Source.ChatID,
			"identity_source", "installer_fallback",
		)
		return engine.ResolvedIdentity{UserID: inst.InstallerUserID}, nil
	}
	if _, err := r.q.GetMemberByUserAndWorkspace(ctx, db.GetMemberByUserAndWorkspaceParams{
		UserID:      binding.MulticaUserID,
		WorkspaceID: inst.WorkspaceID,
	}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return engine.ResolvedIdentity{}, engine.ErrSenderNotMember
		}
		return engine.ResolvedIdentity{}, err
	}
	return engine.ResolvedIdentity{UserID: binding.MulticaUserID}, nil
}

func (r *identityResolver) loggerFor() *slog.Logger {
	if r.logger != nil {
		return r.logger
	}
	return slog.Default()
}

func uuidOrEmpty(id pgtype.UUID) string {
	if !id.Valid {
		return ""
	}
	return util.UUIDToString(id)
}

func (r *auditor) loggerFor() *slog.Logger {
	if r.logger != nil {
		return r.logger
	}
	return slog.Default()
}

// configuredBotIdentity reads the installation's stored bot_user_id and
// bot_mention_names for the diagnostic log. The second return value reports
// whether the read itself succeeded, so the log can distinguish a genuinely
// empty configuration from a failed lookup (the previous version dropped
// channel_type from the query and so ALWAYS failed with ErrNoRows, printing an
// empty value even for a configured installation — the N40 misdiagnosis).
func (r *auditor) configuredBotIdentity(ctx context.Context, instID pgtype.UUID) (botUserID string, mentionNames []string, read bool) {
	if !instID.Valid {
		return "", nil, false
	}
	row, err := r.q.GetChannelInstallation(ctx, db.GetChannelInstallationParams{
		ID:          instID,
		ChannelType: string(TypeInfoFlow),
	})
	if err != nil {
		return "", nil, false
	}
	cfg, err := decodeCredentials(row.Config, nil)
	if err != nil {
		return "", nil, false
	}
	return cfg.BotUserID, cfg.BotMentionNames, true
}

// mentionTargets flattens every AT element's candidate target ids for logging.
func mentionTargets(msg channel.InboundMessage) []string {
	raw, err := decodeInfoFlowRaw(msg)
	if err != nil || raw.Message == nil {
		return nil
	}
	var out []string
	for _, item := range raw.Message.Body {
		if !strings.EqualFold(stringOrEmpty(item["type"]), "AT") {
			continue
		}
		for _, id := range stringListOrEmpty(item["atuserids"]) {
			out = append(out, id)
		}
		for _, candidate := range []any{item["userid"], item["userId"], item["uid"], item["id"], item["name"], item["label"]} {
			if s := stringOrEmpty(candidate); s != "" {
				out = append(out, s)
			}
		}
	}
	return out
}

// mentionAtElements serializes each AT body element verbatim so a log can show
// the exact mention shape the server sent — this is what exposed the
// "display name only" payload behind N40.
func mentionAtElements(msg channel.InboundMessage) []string {
	raw, err := decodeInfoFlowRaw(msg)
	if err != nil || raw.Message == nil {
		return nil
	}
	var out []string
	for _, item := range raw.Message.Body {
		if !strings.EqualFold(stringOrEmpty(item["type"]), "AT") {
			continue
		}
		encoded, err := json.Marshal(item)
		if err != nil {
			continue
		}
		if s := string(encoded); s != "" {
			out = append(out, s)
		}
	}
	return out
}

type deduper struct{ q *db.Queries }

func (r *deduper) Claim(ctx context.Context, installationID pgtype.UUID, messageID string) (pgtype.UUID, error) {
	claim, err := r.q.ClaimChannelInboundDedup(ctx, db.ClaimChannelInboundDedupParams{
		InstallationID: installationID,
		MessageID:      messageID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return pgtype.UUID{}, engine.ErrDuplicate
		}
		return pgtype.UUID{}, err
	}
	return claim.ClaimToken, nil
}

func (r *deduper) Mark(ctx context.Context, installationID pgtype.UUID, messageID string, claimToken pgtype.UUID) error {
	_, err := r.q.MarkChannelInboundDedupProcessed(ctx, db.MarkChannelInboundDedupProcessedParams{
		InstallationID: installationID,
		MessageID:      messageID,
		ClaimToken:     claimToken,
	})
	return err
}

func (r *deduper) Release(ctx context.Context, installationID pgtype.UUID, messageID string, claimToken pgtype.UUID) error {
	_, err := r.q.ReleaseChannelInboundDedup(ctx, db.ReleaseChannelInboundDedupParams{
		InstallationID: installationID,
		MessageID:      messageID,
		ClaimToken:     claimToken,
	})
	return err
}

type chatSession interface {
	EnsureSession(ctx context.Context, in engine.EnsureSessionInput) (pgtype.UUID, error)
	StartSession(ctx context.Context, in engine.StartSessionInput) (engine.StartSessionResult, error)
	MarkPendingFresh(ctx context.Context, sessionID pgtype.UUID, messageID string) error
	AppendUserMessage(ctx context.Context, in engine.AppendInput) (engine.AppendResult, error)
	BindMediaRefs(ctx context.Context, in engine.BindMediaInput) error
}

type sessionBinder struct {
	session chatSession
}

func (r *sessionBinder) EnsureSession(ctx context.Context, p engine.EnsureSessionParams) (pgtype.UUID, error) {
	bindingKey, config := infoflowSessionRouting(p.Message)
	return r.session.EnsureSession(ctx, engine.EnsureSessionInput{
		WorkspaceID:    p.Installation.WorkspaceID,
		AgentID:        p.Installation.AgentID,
		InstallationID: p.Installation.ID,
		Sender:         p.Sender,
		BindingKey:     bindingKey,
		BindingConfig:  config,
		ChatType:       p.Message.Source.ChatType,
	})
}

func (r *sessionBinder) StartSession(ctx context.Context, p engine.StartSessionParams) (engine.StartSessionResult, error) {
	bindingKey, config := infoflowSessionRouting(p.Message)
	result, err := r.session.StartSession(ctx, engine.StartSessionInput{
		EnsureSessionInput: engine.EnsureSessionInput{
			WorkspaceID:    p.Installation.WorkspaceID,
			AgentID:        p.Installation.AgentID,
			InstallationID: p.Installation.ID,
			Sender:         p.Creator,
			BindingKey:     bindingKey,
			BindingConfig:  config,
			ChatType:       p.Message.Source.ChatType,
		},
		Initiator:              p.Sender,
		Body:                   p.Message.Text,
		CommandText:            p.Message.CommandText,
		MessageID:              p.Message.MessageID,
		ThreadID:               p.Message.Source.ThreadID,
		ClaimToken:             p.ClaimToken,
		MediaPendingSeconds:    p.MediaPendingSeconds,
		PersistMessage:         p.PersistMessage,
		HistoryBoundaryPending: p.HistoryBoundaryPending,
		BeforeCommit:           p.BeforeCommit,
	})
	if err != nil {
		return engine.StartSessionResult{}, err
	}
	return engine.StartSessionResult{
		SessionID:     result.SessionID,
		BindingID:     result.BindingID,
		RouteRevision: result.RouteRevision,
		Append:        result.Append,
	}, nil
}

func (r *sessionBinder) MarkPendingFresh(ctx context.Context, sessionID pgtype.UUID, messageID string) error {
	return r.session.MarkPendingFresh(ctx, sessionID, messageID)
}

func (r *sessionBinder) AppendMessage(ctx context.Context, p engine.AppendParams) (engine.AppendResult, error) {
	commandText := p.Message.CommandText
	if commandText == "" {
		commandText = p.Message.Text
	}
	return r.session.AppendUserMessage(ctx, engine.AppendInput{
		SessionID:           p.SessionID,
		Sender:              p.Sender,
		InstallationID:      p.InstallationID,
		Body:                p.Message.Text,
		CommandText:         commandText,
		MessageID:           p.Message.MessageID,
		ThreadID:            p.Message.Source.ThreadID,
		ClaimToken:          p.ClaimToken,
		MediaPendingSeconds: p.MediaPendingSeconds,
		ForceFresh:          p.Message.ForceFresh,
	})
}

func (r *sessionBinder) BindMedia(ctx context.Context, p engine.BindMediaParams) (engine.BindMediaResult, error) {
	return engine.BindMediaResult{}, r.session.BindMediaRefs(ctx, engine.BindMediaInput{
		MessageID:            p.MessageID,
		SessionID:            p.SessionID,
		WorkspaceID:          p.WorkspaceID,
		Sender:               p.Sender,
		IssueID:              p.IssueID,
		IssueDescriptionBase: p.IssueDescriptionBase,
		IssueCommandText:     p.IssueCommandText,
		Body:                 p.Body,
		MediaRefs:            p.MediaRefs,
	})
}

type auditor struct {
	q      *db.Queries
	logger *slog.Logger
}

func (r *auditor) RecordDrop(ctx context.Context, instID pgtype.UUID, msg channel.InboundMessage, reason engine.DropReason) error {
	if reason == engine.DropReasonNotAddressedInGroup {
		// A group message that we received but decided was not addressed to the
		// bot. Log the mention targets alongside the configured bot id so a
		// misconfigured installation (e.g. bot_user_id holding a group id) is
		// diagnosable from logs alone: if mention_targets never contains
		// bot_user_id, the message really was not for us OR the id is wrong.
		botUserID, mentionNames, read := r.configuredBotIdentity(ctx, instID)
		r.loggerFor().InfoContext(ctx, "infoflow: group message not addressed to the bot",
			"installation_id", uuidOrEmpty(instID),
			"chat_id", msg.Source.ChatID,
			"event_id", msg.EventID,
			"bot_user_id_configured", botUserID,
			"bot_mention_names", strings.Join(mentionNames, ","),
			"config_read_ok", read,
			"mention_targets", strings.Join(mentionTargets(msg), ","),
			"mention_at_raw", strings.Join(mentionAtElements(msg), " "),
			"hint", "bot_user_id must be the robot's own InfoFlow user id; InfoFlow @ commonly carries only the robot DISPLAY NAME, so set bot_mention_names or rely on the group member lookup",
		)
	}
	return r.q.RecordChannelInboundDrop(ctx, db.RecordChannelInboundDropParams{
		ID:               dbid.NewV7(),
		ChannelType:      string(TypeInfoFlow),
		EventType:        "message",
		DropReason:       string(reason),
		InstallationID:   instID,
		ChannelChatID:    nullText(msg.Source.ChatID),
		ChannelEventID:   nullText(msg.EventID),
		ChannelMessageID: nullText(msg.MessageID),
	})
}
