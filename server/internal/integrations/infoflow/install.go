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
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/integrations/channel/engine"
	"github.com/multica-ai/multica/server/internal/util/secretbox"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

var (
	ErrInstallationNotFound          = errors.New("infoflow installation not found")
	ErrAppOwnedByAnotherWorkspace    = errors.New("infoflow: application is already connected to another workspace")
	ErrInvalidApplicationID          = errors.New("infoflow: application id is required")
	ErrInvalidAppKey                 = errors.New("infoflow: AppKey is required")
	ErrInvalidAppSecret              = errors.New("infoflow: AppSecret is required")
	ErrCredentialValidation          = errors.New("infoflow: could not validate credentials")
)

type RegisterBYOParams struct {
	WorkspaceID   pgtype.UUID
	AgentID       pgtype.UUID
	InitiatorID   pgtype.UUID
	ApplicationID string
	AppKey        string
	AppSecret     string
	BotUserID     string
}

type InstallService struct {
	box  *secretbox.Box
	q    *db.Queries
	tx   engine.TxStarter
	http *http.Client
	logger *slog.Logger
	validationTimeout time.Duration
	apiBase string
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
	client := NewClient(s.http, s.apiBase)
	validationCtx, cancel := context.WithTimeout(ctx, s.validationTimeout)
	defer cancel()
	if _, err := client.AccessToken(validationCtx, appKey, appSecret); err != nil {
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
		AgentID:            applicationID,
	})
	if err != nil {
		return db.ChannelInstallation{}, fmt.Errorf("encode infoflow installation config: %w", err)
	}
	row, err := s.q.UpsertChannelInstallationByAppID(ctx, db.UpsertChannelInstallationByAppIDParams{
		WorkspaceID: p.WorkspaceID, AgentID: p.AgentID, ChannelType: string(TypeInfoFlow),
		Config: cfgJSON, InstallerUserID: p.InitiatorID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return db.ChannelInstallation{}, ErrAppOwnedByAnotherWorkspace
	}
	if err != nil {
		return db.ChannelInstallation{}, err
	}
	return row, nil
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
