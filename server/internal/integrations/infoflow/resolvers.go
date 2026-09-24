package infoflow

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/integrations/channel"
	"github.com/multica-ai/multica/server/internal/integrations/channel/engine"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

// originInfoFlowChat is the issue.origin_type label for issues created through
// the InfoFlow /issue command. Keep it aligned with the database CHECK
// constraint, like the existing lark_chat, slack_chat and dingtalk_chat origin
// labels.
const originInfoFlowChat = "infoflow_chat"

// NewResolverSet assembles the InfoFlow ResolverSet over the generated queries
// plus a tx starter. This adapter starts with the minimum channel pipeline:
// installation/identity/dedup/session/audit are required; media and typing are
// omitted until the InfoFlow SDK/API surface for those capabilities is settled.
func NewResolverSet(q *db.Queries, tx engine.TxStarter) engine.ResolverSet {
	return engine.ResolverSet{
		Installation: &installationResolver{q: q},
		Identity:     &identityResolver{q: q},
		Dedup:        &deduper{q: q},
		Session: &sessionBinder{session: engine.NewChatSession(q, tx, TypeInfoFlow, engine.SessionTitles{
			Group:    "InfoFlow group",
			Direct:   "InfoFlow direct message",
			Fallback: "InfoFlow chat",
		})},
		Audit:      &auditor{q: q},
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

// infoflowBindingConfig is the opaque outbound routing persisted on the chat
// binding's config: enough to address a proactive reply back into the
// originating conversation. SenderID is the lone recipient of a 1:1 chat; for a
// group it is empty and the group is addressed by ChatID.
type infoflowBindingConfig struct {
	ChatType string `json:"chat_type"`
	ChatID   string `json:"chat_id"`
	SenderID string `json:"sender_id,omitempty"`
}

func infoflowSessionRouting(msg channel.InboundMessage) (bindingKey string, config []byte) {
	chatID := msg.Source.ChatID
	cfg := infoflowBindingConfig{ChatType: string(msg.Source.ChatType), ChatID: chatID}
	if msg.Source.ChatType == channel.ChatTypeP2P {
		cfg.SenderID = msg.Source.SenderID
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
		}
	}
	return target
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

type identityResolver struct{ q *db.Queries }

func (r *identityResolver) ResolveSender(ctx context.Context, inst engine.ResolvedInstallation, msg channel.InboundMessage) (engine.ResolvedIdentity, error) {
	binding, err := r.q.GetChannelUserBindingByUserID(ctx, db.GetChannelUserBindingByUserIDParams{
		InstallationID: inst.ID,
		ChannelUserID:  msg.Source.SenderID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return engine.ResolvedIdentity{}, engine.ErrSenderUnbound
		}
		return engine.ResolvedIdentity{}, err
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
		Initiator:                p.Sender,
		Body:                     p.Message.Text,
		CommandText:              p.Message.CommandText,
		MessageID:                p.Message.MessageID,
		ThreadID:                 p.Message.Source.ThreadID,
		ClaimToken:               p.ClaimToken,
		MediaPendingSeconds:      p.MediaPendingSeconds,
		PersistMessage:           p.PersistMessage,
		HistoryBoundaryPending:   p.HistoryBoundaryPending,
		BeforeCommit:             p.BeforeCommit,
	})
	if err != nil {
		return engine.StartSessionResult{}, err
	}
	return engine.StartSessionResult{
		SessionID: result.SessionID,
		BindingID: result.BindingID,
		RouteRevision: result.RouteRevision,
		Append: result.Append,
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

type auditor struct{ q *db.Queries }

func (r *auditor) RecordDrop(ctx context.Context, instID pgtype.UUID, msg channel.InboundMessage, reason engine.DropReason) error {
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
