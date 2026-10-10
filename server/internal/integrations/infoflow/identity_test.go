package infoflow

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/multica-ai/multica/server/internal/integrations/channel"
	"github.com/multica-ai/multica/server/internal/integrations/channel/engine"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// fakeIdentityQueries lets the identity ladder be tested without a database:
// the fallback rule is pure decision logic over two rows.
type fakeIdentityQueries struct {
	binding    db.ChannelUserBinding
	bindingErr error
	memberErr  error
	memberCall int
}

func (f *fakeIdentityQueries) GetChannelUserBindingByUserID(context.Context, db.GetChannelUserBindingByUserIDParams) (db.ChannelUserBinding, error) {
	return f.binding, f.bindingErr
}

func (f *fakeIdentityQueries) GetMemberByUserAndWorkspace(context.Context, db.GetMemberByUserAndWorkspaceParams) (db.Member, error) {
	f.memberCall++
	if f.memberErr != nil {
		return db.Member{}, f.memberErr
	}
	return db.Member{}, nil
}

func testInstallation(installer string) engine.ResolvedInstallation {
	inst := engine.ResolvedInstallation{
		WorkspaceID: util.MustParseUUID("11111111-1111-1111-1111-111111111111"),
		ID:          util.MustParseUUID("22222222-2222-2222-2222-222222222222"),
	}
	if installer != "" {
		inst.InstallerUserID = util.MustParseUUID(installer)
	}
	return inst
}

func infoflowMessage(chatType channel.ChatType, chatID, senderID string) channel.InboundMessage {
	return channel.InboundMessage{
		Source: channel.Source{
			ChannelType: TypeInfoFlow,
			ChatType:    chatType,
			ChatID:      chatID,
			SenderID:    senderID,
		},
	}
}

// The N12 contract: an unbound sender executes as the installation's installer
// instead of being dropped as needs_binding.
func TestResolveSenderFallsBackToInstallerWhenUnbound(t *testing.T) {
	const installer = "33333333-3333-3333-3333-333333333333"
	r := &identityResolver{q: &fakeIdentityQueries{bindingErr: pgx.ErrNoRows}}
	got, err := r.ResolveSender(context.Background(), testInstallation(installer), infoflowMessage(channel.ChatTypeP2P, "u1", "u1"))
	if err != nil {
		t.Fatalf("unbound sender must not error, got %v", err)
	}
	if util.UUIDToString(got.UserID) != installer {
		t.Fatalf("identity = %s, want installer %s", util.UUIDToString(got.UserID), installer)
	}
}

// A group mention takes the same fallback — the product decision (D-N11-B) is
// that group and single chat both reach the Agent without a binding.
func TestResolveSenderFallsBackToInstallerInGroup(t *testing.T) {
	const installer = "33333333-3333-3333-3333-333333333333"
	r := &identityResolver{q: &fakeIdentityQueries{bindingErr: pgx.ErrNoRows}}
	got, err := r.ResolveSender(context.Background(), testInstallation(installer), infoflowMessage(channel.ChatTypeGroup, "12572761", "u2"))
	if err != nil {
		t.Fatalf("group fallback must not error, got %v", err)
	}
	if util.UUIDToString(got.UserID) != installer {
		t.Fatalf("identity = %s, want installer %s", util.UUIDToString(got.UserID), installer)
	}
}

// A data defect: an installation with no installer cannot fall back.
func TestResolveSenderUnboundWithoutInstallerStillFails(t *testing.T) {
	r := &identityResolver{q: &fakeIdentityQueries{bindingErr: pgx.ErrNoRows}}
	_, err := r.ResolveSender(context.Background(), testInstallation(""), infoflowMessage(channel.ChatTypeP2P, "u1", "u1"))
	if !errors.Is(err, engine.ErrSenderUnbound) {
		t.Fatalf("err = %v, want ErrSenderUnbound", err)
	}
}

// An explicit binding still wins over the fallback.
func TestResolveSenderUsesBindingWhenPresent(t *testing.T) {
	const bound = "44444444-4444-4444-4444-444444444444"
	r := &identityResolver{q: &fakeIdentityQueries{
		binding: db.ChannelUserBinding{MulticaUserID: util.MustParseUUID(bound)},
	}}
	got, err := r.ResolveSender(context.Background(), testInstallation("33333333-3333-3333-3333-333333333333"), infoflowMessage(channel.ChatTypeP2P, "u1", "u1"))
	if err != nil {
		t.Fatal(err)
	}
	if util.UUIDToString(got.UserID) != bound {
		t.Fatalf("identity = %s, want bound user %s", util.UUIDToString(got.UserID), bound)
	}
}

// A binding to a removed member must not keep driving the Agent.
func TestResolveSenderBindingToNonMemberFails(t *testing.T) {
	r := &identityResolver{q: &fakeIdentityQueries{
		binding:   db.ChannelUserBinding{MulticaUserID: util.MustParseUUID("44444444-4444-4444-4444-444444444444")},
		memberErr: pgx.ErrNoRows,
	}}
	_, err := r.ResolveSender(context.Background(), testInstallation("33333333-3333-3333-3333-333333333333"), infoflowMessage(channel.ChatTypeP2P, "u1", "u1"))
	if !errors.Is(err, engine.ErrSenderNotMember) {
		t.Fatalf("err = %v, want ErrSenderNotMember", err)
	}
}

// Single-chat sessions are isolated per user: two senders in the same bot must
// not share a Multica chat. Group chat stays keyed by the group id.
func TestSessionRoutingIsolatesSingleChatPerSender(t *testing.T) {
	keyA, cfgA := infoflowSessionRouting(infoflowMessage(channel.ChatTypeP2P, "u1", "u1"))
	keyB, cfgB := infoflowSessionRouting(infoflowMessage(channel.ChatTypeP2P, "u2", "u2"))
	if keyA == keyB {
		t.Fatalf("two single-chat senders share binding key %q", keyA)
	}
	for _, raw := range [][]byte{cfgA, cfgB} {
		var cfg infoflowBindingConfig
		if err := json.Unmarshal(raw, &cfg); err != nil {
			t.Fatal(err)
		}
		if cfg.ChatType != string(channel.ChatTypeP2P) {
			t.Fatalf("single-chat routing config chat_type = %q", cfg.ChatType)
		}
	}

	groupKey, groupCfgRaw := infoflowSessionRouting(infoflowMessage(channel.ChatTypeGroup, "12572761", "u1"))
	if groupKey != "12572761" {
		t.Fatalf("group binding key = %q, want the group id", groupKey)
	}
	var groupCfg infoflowBindingConfig
	if err := json.Unmarshal(groupCfgRaw, &groupCfg); err != nil {
		t.Fatal(err)
	}
	if groupCfg.ChatType != string(channel.ChatTypeGroup) || groupCfg.ChatID != "12572761" {
		t.Fatalf("group routing config = %+v", groupCfg)
	}
}
