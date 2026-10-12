package infoflow

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/integrations/channel/engine"
	"github.com/multica-ai/multica/server/internal/util/secretbox"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

var (
	ErrInstallationNotFound       = errors.New("infoflow installation not found")
	ErrAppOwnedByAnotherWorkspace = errors.New("infoflow: application is already connected to a different Multica workspace")
	ErrAppOwnedBySameWorkspace    = errors.New("infoflow: application is already connected to another agent in this workspace")
	ErrAppOwnedByArchivedAgent    = errors.New("infoflow: application is connected to an archived agent in this workspace")
	ErrInvalidApplicationID       = errors.New("infoflow: application id is required")
	ErrInvalidBotUserID           = errors.New("infoflow: bot_user_id must not equal the application id")
	ErrInvalidAppKey              = errors.New("infoflow: AppKey is required")
	ErrInvalidAppSecret           = errors.New("infoflow: AppSecret is required")
	ErrCredentialValidation       = errors.New("infoflow: could not validate credentials")
)

// pgUniqueViolation is Postgres' unique_violation SQLSTATE. A live owner of the
// (channel_type, app_id) routing slot trips it after the dead-owner reclaim has
// run, and the handler must see an accurate conflict instead of a raw 500.
const pgUniqueViolation = "23505"

type RegisterBYOParams struct {
	WorkspaceID   pgtype.UUID
	AgentID       pgtype.UUID
	InitiatorID   pgtype.UUID
	ApplicationID string
	AppKey        string
	AppSecret     string
	BotUserID     string
	// BotMentionNames are optional display names the robot may be @-mentioned
	// by. InfoFlow's group AT element usually carries only the display name, so
	// these let an installation match mentions without a member-list lookup.
	BotMentionNames string
}

type InstallService struct {
	box               *secretbox.Box
	q                 *db.Queries
	tx                engine.TxStarter
	http              *http.Client
	logger            *slog.Logger
	validationTimeout time.Duration
	apiBase           string
}

func NewInstallService(q *db.Queries, tx engine.TxStarter, box *secretbox.Box, logger *slog.Logger) (*InstallService, error) {
	if q == nil {
		return nil, errors.New("infoflow: InstallService requires queries")
	}
	if tx == nil {
		return nil, errors.New("infoflow: InstallService requires a tx starter")
	}
	if box == nil {
		return nil, errors.New("infoflow: InstallService requires a non-nil secretbox.Box")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &InstallService{box: box, q: q, tx: tx, http: http.DefaultClient, logger: logger, validationTimeout: 15 * time.Second}, nil
}

// RegisterBYO installs a user-supplied InfoFlow robot for an agent.
//
// The whole sequence — lock the application's routing slot, read its current
// owner, refuse a live foreign owner, validate the pasted credentials, reclaim a
// dead owner, write the installation — runs inside ONE transaction, mirroring
// the wecom/slack/telegram install paths:
//
//   - idx_channel_installation_type_appid is UNIQUE on
//     (channel_type, config->>'app_id') with no workspace in it, so an
//     application id's routing slot is global across the deployment.
//     ReclaimDeadChannelInstallationByAppID hard-deletes a DEAD owner of that
//     slot (a revoked row held by another agent, or an orphan whose
//     workspace/agent is gone) along with its dependent rows.
//
//   - The slot read must be serialized with the write, so it happens after
//     LockChannelInstallationAppIDSlot (a transaction-level advisory lock) and
//     before any side effect. A live foreign owner is refused here, having
//     touched nothing.
//
//   - Revoke only flips status to 'revoked'; it does not free the row. Without
//     the reclaim step, an application revoked from agent A could never be
//     connected to agent B: the upsert would miss its ON CONFLICT and trip the
//     unique index, and the admin would be told to disconnect an already
//     disconnected bot with no UI control to free it. The reclaim is therefore
//     the self-service release path for a revoked installation.
func (s *InstallService) RegisterBYO(ctx context.Context, p RegisterBYOParams) (db.ChannelInstallation, error) {
	applicationID := strings.TrimSpace(p.ApplicationID)
	appKey := strings.TrimSpace(p.AppKey)
	appSecret := strings.TrimSpace(p.AppSecret)
	if applicationID == "" {
		return db.ChannelInstallation{}, ErrInvalidApplicationID
	}
	if appKey == "" {
		return db.ChannelInstallation{}, ErrInvalidAppKey
	}
	if appSecret == "" {
		return db.ChannelInstallation{}, ErrInvalidAppSecret
	}
	if strings.TrimSpace(p.BotUserID) == applicationID {
		return db.ChannelInstallation{}, ErrInvalidBotUserID
	}
	if s.tx == nil {
		return db.ChannelInstallation{}, errors.New("infoflow: InstallService requires a transaction starter")
	}

	tx, err := s.tx.Begin(ctx)
	if err != nil {
		return db.ChannelInstallation{}, fmt.Errorf("infoflow: begin install tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	qtx := s.q.WithTx(tx)

	// Serialization boundary: held until commit/rollback, so the owner read
	// below cannot go stale under a concurrent install or reconnect.
	if err := qtx.LockChannelInstallationAppIDSlot(ctx, db.LockChannelInstallationAppIDSlotParams{
		ChannelType: string(TypeInfoFlow),
		AppID:       applicationID,
	}); err != nil {
		return db.ChannelInstallation{}, fmt.Errorf("infoflow: lock application routing slot: %w", err)
	}

	// Refuse a LIVE foreign owner before validating credentials or reclaiming
	// anything, so a request that cannot succeed has no side effect.
	if err := s.slotConflictErr(ctx, qtx, p, applicationID); err != nil {
		return db.ChannelInstallation{}, err
	}

	// Prove the pasted credentials are real before the reclaim acts on the slot.
	validationCtx, cancel := context.WithTimeout(ctx, s.validationTimeout)
	defer cancel()
	if _, err := NewClient(s.http, s.apiBase).AccessToken(validationCtx, appKey, appSecret); err != nil {
		return db.ChannelInstallation{}, fmt.Errorf("%w: %v", ErrCredentialValidation, err)
	}

	sealed, err := s.box.Seal([]byte(appSecret))
	if err != nil {
		return db.ChannelInstallation{}, fmt.Errorf("encrypt infoflow app secret: %w", err)
	}
	cfgJSON, err := json.Marshal(installConfig{
		AppID:              applicationID,
		AppKey:             appKey,
		AppSecretEncrypted: base64.StdEncoding.EncodeToString(sealed),
		BotUserID:          strings.TrimSpace(p.BotUserID),
		BotMentionNames:    normalizeMentionNames(p.BotMentionNames),
		AgentID:            applicationID,
	})
	if err != nil {
		return db.ChannelInstallation{}, fmt.Errorf("encode infoflow installation config: %w", err)
	}

	// Free a dead owner (revoked row of another agent, or an orphan). A no-op
	// (pgx.ErrNoRows) is success: nothing was dead.
	if _, err := qtx.ReclaimDeadChannelInstallationByAppID(ctx, db.ReclaimDeadChannelInstallationByAppIDParams{
		ChannelType: string(TypeInfoFlow),
		AppID:       applicationID,
		WorkspaceID: p.WorkspaceID,
		AgentID:     p.AgentID,
	}); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return db.ChannelInstallation{}, fmt.Errorf("infoflow: reclaim dead installation: %w", err)
	}

	if strings.TrimSpace(p.BotUserID) == "" {
		// Not a hard requirement: with no bot_user_id a group @ mention can only
		// be recognized via an explicit @all, so named mentions will be dropped
		// as unaddressed. Log it at install time so the misconfiguration is
		// discoverable instead of showing up later as "the bot ignores me".
		s.loggerFor().WarnContext(ctx, "infoflow: installation has no bot_user_id; group @ mentions will not be recognized",
			"installation_app_id", applicationID,
			"workspace_id", uuidOrEmpty(p.WorkspaceID),
			"agent_id", uuidOrEmpty(p.AgentID),
			"action", "set bot_user_id to the robot's own InfoFlow user id (e.g. 4105001274) so group @ mentions can be matched",
		)
	}

	row, err := qtx.UpsertChannelInstallationByAppID(ctx, db.UpsertChannelInstallationByAppIDParams{
		WorkspaceID:     p.WorkspaceID,
		AgentID:         p.AgentID,
		ChannelType:     string(TypeInfoFlow),
		Config:          cfgJSON,
		InstallerUserID: p.InitiatorID,
	})
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == pgUniqueViolation {
			// A live owner still holds the slot; name it precisely. The tx is
			// aborted now, so read the owner on the caller's handle.
			return db.ChannelInstallation{}, s.ownerConflictErr(ctx, p.WorkspaceID, applicationID)
		}
		return db.ChannelInstallation{}, fmt.Errorf("infoflow: upsert installation: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return db.ChannelInstallation{}, fmt.Errorf("infoflow: commit install tx: %w", err)
	}
	return row, nil
}

// slotConflictErr answers one question inside the slot lock: may this install
// act on the (infoflow, app_id) routing slot at all? It mirrors the reclaim's
// definition of "dead" so the two cannot disagree about which rows are
// takeable.
//
//	no row            → the slot is free.
//	orphan            → workspace or agent row gone; the reclaim frees it.
//	revoked           → the owner is done with the bot; the reclaim takes it
//	                    (or, if it is this caller's own row, the upsert
//	                    reactivates it in place).
//	active, ours      → re-install / secret rotation; not a conflict.
//	active, somebody  → refused with the precise owner conflict.
func (s *InstallService) slotConflictErr(ctx context.Context, qtx *db.Queries, p RegisterBYOParams, applicationID string) error {
	owner, err := qtx.GetChannelInstallationSlotOwnerByAppID(ctx, db.GetChannelInstallationSlotOwnerByAppIDParams{
		ChannelType: string(TypeInfoFlow),
		AppID:       applicationID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return fmt.Errorf("infoflow: read application routing slot owner: %w", err)
	}
	switch {
	case !owner.WorkspaceExists || !owner.AgentExists:
		return nil
	case owner.Status == "revoked":
		return nil
	case owner.WorkspaceID == p.WorkspaceID && owner.AgentID == p.AgentID:
		return nil
	case owner.WorkspaceID != p.WorkspaceID:
		return ErrAppOwnedByAnotherWorkspace
	case owner.AgentArchivedAt.Valid:
		return ErrAppOwnedByArchivedAgent
	default:
		return ErrAppOwnedBySameWorkspace
	}
}

// ownerConflictErr names who holds the slot after a unique violation, so the
// handler can point the admin at the right control instead of a raw 500. A
// failed lookup falls back to the cross-workspace message: it is the only one
// that is never wrong about where to look, and a retry succeeds anyway.
func (s *InstallService) ownerConflictErr(ctx context.Context, requestingWorkspaceID pgtype.UUID, applicationID string) error {
	owner, err := s.q.GetChannelInstallationOwnerByAppID(ctx, db.GetChannelInstallationOwnerByAppIDParams{
		ChannelType: string(TypeInfoFlow),
		AppID:       applicationID,
	})
	if err != nil {
		return ErrAppOwnedByAnotherWorkspace
	}
	switch {
	case owner.WorkspaceID != requestingWorkspaceID:
		return ErrAppOwnedByAnotherWorkspace
	case owner.AgentArchivedAt.Valid:
		return ErrAppOwnedByArchivedAgent
	default:
		return ErrAppOwnedBySameWorkspace
	}
}

func (s *InstallService) loggerFor() *slog.Logger {
	if s.logger != nil {
		return s.logger
	}
	return slog.Default()
}

func (s *InstallService) ListByWorkspace(ctx context.Context, workspaceID pgtype.UUID) ([]db.ChannelInstallation, error) {
	return s.q.ListChannelInstallationsByWorkspace(ctx, db.ListChannelInstallationsByWorkspaceParams{
		WorkspaceID: workspaceID, ChannelType: string(TypeInfoFlow),
	})
}

func (s *InstallService) GetInWorkspace(ctx context.Context, id, workspaceID pgtype.UUID) (db.ChannelInstallation, error) {
	row, err := s.q.GetChannelInstallationInWorkspace(ctx, db.GetChannelInstallationInWorkspaceParams{
		ID: id, WorkspaceID: workspaceID, ChannelType: string(TypeInfoFlow),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return db.ChannelInstallation{}, ErrInstallationNotFound
	}
	return row, err
}

func (s *InstallService) Revoke(ctx context.Context, id pgtype.UUID) error {
	return s.q.SetChannelInstallationStatus(ctx, db.SetChannelInstallationStatusParams{ID: id, Status: "revoked"})
}
