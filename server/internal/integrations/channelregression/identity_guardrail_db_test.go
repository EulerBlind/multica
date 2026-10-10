package channelregression

// identity_guardrail_db_test.go — the cross-channel red line from QIA-600
// N11/N12/N15, pinned as a table-driven test.
//
// InfoFlow deliberately resolves an UNBOUND sender to the installation's
// installer (N11 decision A: messages reach the Agent without an account-link
// step). Every other channel keeps the original gate: an unbound sender is
// refused with engine.ErrSenderUnbound, which the engine maps to
// outcome=needs_binding / drop_reason=unbound_user.
//
// The two behaviours are produced by each channel's own identity resolver —
// there is no shared code path — so this test builds the REAL resolver sets
// (real *db.Queries over a real connection pool) and calls ResolveSender for a
// sender that has no binding. It therefore fails if anyone flips the InfoFlow
// branch back, or extends the fallback to another channel.
//
// It skips when no migrated database is reachable, matching the other
// integration DB tests in this tree.

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/multica-ai/multica/server/internal/integrations/channel"
	"github.com/multica-ai/multica/server/internal/integrations/channel/engine"
	"github.com/multica-ai/multica/server/internal/integrations/dingtalk"
	"github.com/multica-ai/multica/server/internal/integrations/infoflow"
	"github.com/multica-ai/multica/server/internal/integrations/lark"
	"github.com/multica-ai/multica/server/internal/integrations/slack"
	"github.com/multica-ai/multica/server/internal/integrations/telegram"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func guardrailDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://multica:multica@localhost:5432/multica?sslmode=disable"
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Skipf("no database: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Skipf("database not reachable: %v", err)
	}
	var migrated bool
	if err := pool.QueryRow(ctx,
		`SELECT to_regclass('public.channel_user_binding') IS NOT NULL`).Scan(&migrated); err != nil || !migrated {
		pool.Close()
		t.Skip("channel tables not present (database not migrated)")
	}
	t.Cleanup(pool.Close)
	return pool
}

// unboundInstallation is a well-formed installation whose id matches no
// channel_user_binding row, so each resolver takes its unbound path.
func unboundInstallation() engine.ResolvedInstallation {
	return engine.ResolvedInstallation{
		ID:              util.MustParseUUID("aaaaaaaa-0000-4000-8000-00000000a001"),
		WorkspaceID:     util.MustParseUUID("aaaaaaaa-0000-4000-8000-00000000a002"),
		AgentID:         util.MustParseUUID("aaaaaaaa-0000-4000-8000-00000000a003"),
		InstallerUserID: util.MustParseUUID("aaaaaaaa-0000-4000-8000-00000000a004"),
		Active:          true,
	}
}

func unboundMessage(channelType channel.Type) channel.InboundMessage {
	return channel.InboundMessage{
		EventID:   "guardrail-event",
		MessageID: "guardrail-message",
		Type:      channel.MsgTypeText,
		Text:      "hi",
		Source: channel.Source{
			ChannelType: channelType,
			ChatID:      "guardrail-chat",
			ChatType:    channel.ChatTypeP2P,
			SenderID:    "guardrail-unbound-sender",
		},
		AddressedToBot: true,
	}
}

// TestUnboundSenderIdentityIsChannelSpecific pins the cross-channel red line:
// InfoFlow falls back to the installer; the other four keep ErrSenderUnbound.
func TestUnboundSenderIdentityIsChannelSpecific(t *testing.T) {
	pool := guardrailDB(t)
	ctx := context.Background()
	q := db.New(pool)

	// Each entry builds the channel's REAL identity resolver through its real
	// resolver-set constructor.
	cases := []struct {
		name        string
		channelType channel.Type
		set         engine.ResolverSet
		wantErr     error
		wantUser    string
	}{
		{
			name:        "infoflow falls back to the installer",
			channelType: infoflow.TypeInfoFlow,
			set:         infoflow.NewResolverSet(q, pool, nil, nil),
			wantErr:     nil,
			wantUser:    "aaaaaaaa-0000-4000-8000-00000000a004",
		},
		{
			name:        "lark still requires a binding",
			channelType: channel.TypeFeishu,
			set: lark.NewFeishuResolverSet(
				lark.NewChannelStore(q),
				engine.NewChatSession(q, pool, channel.TypeFeishu, engine.SessionTitles{}),
				nil, nil, nil, nil,
			),
			wantErr: engine.ErrSenderUnbound,
		},
		{
			name:        "slack still requires a binding",
			channelType: channel.Type("slack"),
			set:         slack.NewSlackResolverSet(q, pool, nil, nil, nil),
			wantErr:     engine.ErrSenderUnbound,
		},
		{
			name:        "telegram still requires a binding",
			channelType: channel.Type("telegram"),
			set:         telegram.NewTelegramResolverSet(q, pool, nil, nil),
			wantErr:     engine.ErrSenderUnbound,
		},
		{
			name:        "dingtalk still requires a binding",
			channelType: dingtalk.TypeDingTalk,
			set:         dingtalk.NewDingTalkResolverSet(q, pool, nil, nil, nil, nil),
			wantErr:     engine.ErrSenderUnbound,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.set.Identity == nil {
				t.Fatal("resolver set has no Identity resolver")
			}
			got, err := tc.set.Identity.ResolveSender(ctx, unboundInstallation(), unboundMessage(tc.channelType))
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("ResolveSender err = %v, want %v (unbound sender must NOT be routed for this channel)", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ResolveSender err = %v, want installer fallback", err)
			}
			if util.UUIDToString(got.UserID) != tc.wantUser {
				t.Fatalf("identity = %s, want installer %s", util.UUIDToString(got.UserID), tc.wantUser)
			}
		})
	}
}
