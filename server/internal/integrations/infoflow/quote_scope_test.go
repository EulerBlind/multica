package infoflow

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/integrations/channel"
)

// rawGroupPayload builds one group event. reply is optional and, when present,
// carries the quoted message's own sender so the lookup is allowed.
func rawGroupPayload(msgID, chatID, text, quoteID, quotePreview string) []byte {
	message := map[string]any{
		"header": map[string]any{
			"fromuserid": "U-" + msgID, "toid": chatID, "totype": "group",
			"msgtype": "text", "messageid": msgID,
		},
		"body": []map[string]any{
			{"type": "AT", "atuserids": []string{"BOT-RAW"}},
			{"type": "TEXT", "content": text},
		},
	}
	if quoteID != "" {
		message["reply"] = map[string]any{
			"messageid": quoteID,
			"preview":   quotePreview,
			"header":    map[string]any{"fromuserid": "U-QUOTED"},
			"body":      []map[string]any{{"type": "IMAGE", "downloadurl": "https://img.example.com/" + quoteID + ".png"}},
		}
	}
	payload := map[string]any{"agentid": "APP-RAW", "groupid": chatID, "message": message}
	raw, _ := json.Marshal(payload)
	return raw
}

func newRawScopeChannel(t *testing.T, srvURL string, rec channel.InboundHandler, workers int) *infoflowChannel {
	t.Helper()
	logger := discardLogger()
	factory := newFactory(ChannelDeps{Client: NewClient(nil, srvURL), Logger: logger})
	cfg := channel.Config{
		Raw:     json.RawMessage(`{"app_id":"APP-RAW","app_key":"KEY-1","app_secret_encrypted":"U0VTLTE=","bot_user_id":"BOT-RAW"}`),
		Handler: rec,
	}
	built, err := factory(cfg)
	if err != nil {
		t.Fatalf("factory: %v", err)
	}
	ch := built.(*infoflowChannel)
	ch.logger = logger
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	ch.workers = newQuoteWorkers(ctx, workers, logger)
	return ch
}

func rawScopeGateway(t *testing.T) string {
	t.Helper()
	gw := &quoteGateway{message: func(_ int, w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":"ok","data":{"msgType":"text","content":"全文-` + r.URL.Query().Get("msgId") + `"}}`))
	}}
	return gw.start(t).URL
}

func queueFrames(t *testing.T, ch *infoflowChannel, frames ...[]byte) {
	t.Helper()
	for i, raw := range frames {
		if err := ch.handleFrameQueued(context.Background(), Frame{SeqID: uint64(i + 1), Method: FrameData, Payload: raw}); err != nil {
			t.Fatalf("frame %d: %v", i+1, err)
		}
	}
}

type rawScopeMessage struct {
	messageID string
	chatID    string
	hasQuote  bool
	raw       map[string]any
}

func (m rawScopeMessage) inner() map[string]any {
	inner, _ := m.raw["message"].(map[string]any)
	return inner
}

func (m rawScopeMessage) origin() (map[string]any, bool) {
	origin, ok := m.raw["multica_quote_origin"].(map[string]any)
	return origin, ok
}

func decodeRawScope(t *testing.T, msgs []channel.InboundMessage) map[string]rawScopeMessage {
	t.Helper()
	out := make(map[string]rawScopeMessage, len(msgs))
	for _, msg := range msgs {
		var raw map[string]any
		if err := json.Unmarshal(msg.Raw, &raw); err != nil {
			t.Fatalf("unmarshal %s raw: %v", msg.MessageID, err)
		}
		out[msg.MessageID] = rawScopeMessage{
			messageID: msg.MessageID,
			chatID:    msg.Source.ChatID,
			hasQuote:  msg.HasSelectedContext,
			raw:       raw,
		}
	}
	return out
}

// assertPlainFrame checks that a registered frame's Raw still owns exactly its
// own payload: no reply/quote from another message and no origin.
func assertPlainFrame(t *testing.T, view rawScopeMessage, ownMessageID string) {
	t.Helper()
	inner := view.inner()
	if got := inner["reply"]; got != nil {
		t.Fatalf("%s Raw.message.reply leaked: %#v", ownMessageID, got)
	}
	if got := inner["quote"]; got != nil {
		t.Fatalf("%s Raw.message.quote leaked: %#v", ownMessageID, got)
	}
	if got := view.raw["quote"]; got != nil {
		t.Fatalf("%s Raw.quote leaked: %#v", ownMessageID, got)
	}
	if _, ok := view.origin(); ok {
		t.Fatalf("%s Raw.multica_quote_origin leaked: %#v", ownMessageID, view.raw["multica_quote_origin"])
	}
	header, _ := inner["header"].(map[string]any)
	if got := header["messageid"]; got != ownMessageID {
		t.Fatalf("%s Raw.message.header.messageid = %#v", ownMessageID, got)
	}
	if view.hasQuote {
		t.Fatalf("%s claims a selected quote", ownMessageID)
	}
}

func assertQuoteFrame(t *testing.T, view rawScopeMessage, quoteID string) {
	t.Helper()
	inner := view.inner()
	reply, _ := inner["reply"].(map[string]any)
	if got := reply["messageid"]; got != quoteID {
		t.Fatalf("%s Raw.message.reply.messageid = %#v, want %s", view.messageID, got, quoteID)
	}
	origin, ok := view.origin()
	if !ok {
		t.Fatalf("%s Raw.multica_quote_origin missing", view.messageID)
	}
	if got := origin["state"]; got != string(quoteStateOK) {
		t.Fatalf("%s origin.state = %#v", view.messageID, got)
	}
	if got := origin["query_id"]; got != quoteID {
		t.Fatalf("%s origin.query_id = %#v, want %s", view.messageID, got, quoteID)
	}
	if !view.hasQuote {
		t.Fatalf("%s should carry a selected quote", view.messageID)
	}
}

// TestRawOriginOnlyOnOwningFrame is the N86 DEC11 acceptance case: one quoted
// frame followed by four plain frames in the SAME chat. Each of frames 2-5 must
// keep its own Raw: no reply/quote from frame 1, no origin.
func TestRawOriginOnlyOnOwningFrame(t *testing.T) {
	srvURL := rawScopeGateway(t)
	rec := &quoteRecorder{}
	ch := newRawScopeChannel(t, srvURL, rec.handle, 4)

	frames := [][]byte{
		rawGroupPayload("M-1", "G-RAW", "请总结 M-1", "Q-1", "被引用的预览…"),
		rawGroupPayload("M-2", "G-RAW", "普通消息 2", "", ""),
		rawGroupPayload("M-3", "G-RAW", "普通消息 3", "", ""),
		rawGroupPayload("M-4", "G-RAW", "普通消息 4", "", ""),
		rawGroupPayload("M-5", "G-RAW", "普通消息 5", "", ""),
	}
	queueFrames(t, ch, frames...)
	waitForMessages(t, rec, 5, 10*time.Second)

	msgs, _ := rec.snapshot()
	views := decodeRawScope(t, msgs)
	assertQuoteFrame(t, views["M-1"], "Q-1")
	for _, id := range []string{"M-2", "M-3", "M-4", "M-5"} {
		assertPlainFrame(t, views[id], id)
		logRawFrame(t, views[id])
	}
	logRawFrame(t, views["M-1"])
}

// TestRawOriginIsolationCrossChat repeats the same five frames across different
// conversations so the lookup of M-1 overlaps frames 2-5.
func TestRawOriginIsolationCrossChat(t *testing.T) {
	srvURL := rawScopeGateway(t)
	rec := &quoteRecorder{}
	ch := newRawScopeChannel(t, srvURL, rec.handle, 4)

	queueFrames(t, ch,
		rawGroupPayload("C-1", "G-C1", "请总结 C-1", "Q-C1", "预览 C-1…"),
		rawGroupPayload("C-2", "G-C2", "普通消息 C-2", "", ""),
		rawGroupPayload("C-3", "G-C3", "普通消息 C-3", "", ""),
		rawGroupPayload("C-4", "G-C4", "普通消息 C-4", "", ""),
		rawGroupPayload("C-5", "G-C5", "普通消息 C-5", "", ""),
	)
	waitForMessages(t, rec, 5, 10*time.Second)

	msgs, _ := rec.snapshot()
	views := decodeRawScope(t, msgs)
	assertQuoteFrame(t, views["C-1"], "Q-C1")
	for _, id := range []string{"C-2", "C-3", "C-4", "C-5"} {
		assertPlainFrame(t, views[id], id)
		logRawFrame(t, views[id])
	}
	logRawFrame(t, views["C-1"])
}

// TestRawOriginIsolationConcurrentChats interleaves three conversations; every
// quoted frame keeps its own origin and every plain frame stays clean.
func TestRawOriginIsolationConcurrentChats(t *testing.T) {
	srvURL := rawScopeGateway(t)
	rec := &quoteRecorder{}
	ch := newRawScopeChannel(t, srvURL, rec.handle, 4)

	var frames [][]byte
	for i := 1; i <= 3; i++ {
		chat := "G-P" + string(rune('0'+i))
		msgID := "P" + string(rune('0'+i)) + "-Q"
		quoteID := "Q-P" + string(rune('0'+i))
		frames = append(frames, rawGroupPayload(msgID, chat, "请总结 "+msgID, quoteID, "预览 "+quoteID+"…"))
		frames = append(frames, rawGroupPayload(msgID+"-plain", chat, "普通消息 "+msgID, "", ""))
	}
	queueFrames(t, ch, frames...)
	waitForMessages(t, rec, 6, 10*time.Second)

	msgs, _ := rec.snapshot()
	views := decodeRawScope(t, msgs)
	for i := 1; i <= 3; i++ {
		msgID := "P" + string(rune('0'+i)) + "-Q"
		quoteID := "Q-P" + string(rune('0'+i))
		assertQuoteFrame(t, views[msgID], quoteID)
		assertPlainFrame(t, views[msgID+"-plain"], msgID+"-plain")
		logRawFrame(t, views[msgID])
		logRawFrame(t, views[msgID+"-plain"])
	}
}

// TestPlainRawMatchesStandaloneNormalization proves the queued frame's Raw is
// the same bytes as normalizing that payload alone: no other frame's data is
// merged in.
func TestPlainRawMatchesStandaloneNormalization(t *testing.T) {
	srvURL := rawScopeGateway(t)
	rec := &quoteRecorder{}
	ch := newRawScopeChannel(t, srvURL, rec.handle, 4)

	plain := rawGroupPayload("S-2", "G-S", "普通消息 S-2", "", "")
	queueFrames(t, ch,
		rawGroupPayload("S-1", "G-S", "请总结 S-1", "Q-S1", "预览…"),
		plain,
	)
	waitForMessages(t, rec, 2, 10*time.Second)

	msgs, _ := rec.snapshot()
	var got []byte
	for _, msg := range msgs {
		if msg.MessageID == "S-2" {
			got = msg.Raw
		}
	}
	standalone, _, ok := inboundFromPayloadReasonWithIdentity(plain, "APP-RAW", newIdentitySet("BOT-RAW", nil))
	if !ok {
		t.Fatal("standalone normalization failed")
	}
	if !bytes.Equal(got, standalone.Raw) {
		t.Fatalf("queued Raw differs from standalone normalization:\n queued: %s\n standalone: %s", got, standalone.Raw)
	}
}

// --- DEC10: dedupe rule ----------------------------------------------------

func TestQuoteLineDuplicatedRules(t *testing.T) {
	full := "同意上线，周五发版"
	quote := &infoflowQuote{Preview: "同意上线…", Content: "同意上线，周五发版"}
	cases := []struct {
		name string
		line string
		want bool
	}{
		{"equal full text", full, true},
		{"equal after whitespace normalization", "  同意上线，   周五发版 ", true},
		{"ellipsis prefix", "同意上线…", true},
		{"ascii ellipsis prefix", "同意上线...", true},
		{"ellipsis with trailing space", "同意上线...   ", true},
		{"independent short line kept", "同意上线", false},
		{"punctuation-only difference kept", "同意上线，周五发版！", false},
		{"suffix line kept", "周五发版", false},
		{"unrelated line kept", "下周再评审", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := quoteLineDuplicated(tc.line, quote, full); got != tc.want {
				t.Fatalf("quoteLineDuplicated(%q) = %v, want %v", tc.line, got, tc.want)
			}
		})
	}
}

// TestFlattenQuotedKeepsIndependentPrefixLine reproduces the N85 minimal case:
// the body line is a prefix of the full text but is not a truncated preview, so
// it must survive next to the full text.
func TestFlattenQuotedKeepsIndependentPrefixLine(t *testing.T) {
	full := "同意上线，周五发版"
	quote := &infoflowQuote{
		MessageID: "Q-1",
		Preview:   "同意上线…",
		Body:      []map[string]any{{"type": "TEXT", "content": "同意上线"}},
	}
	got := flattenQuoted(&infoflowEventMessage{Reply: quote}, &quoteOrigin{QueryID: "Q-1", State: quoteStateOK, Source: "api", Text: full})
	want := quoteTagFull + "\n" + full + "\n同意上线"
	t.Logf("full=%q bodyLine=%q out=%q", full, "同意上线", got)
	if got != want {
		t.Fatalf("independent line lost:\n got: %q\nwant: %q", got, want)
	}
}

// TestFlattenQuotedDropsEllipsisPreviewRows keeps the DEC9 main path: rows that
// are truncated previews ("…" / "...") of the full text are dropped, media rows
// stay.
func TestFlattenQuotedDropsEllipsisPreviewRows(t *testing.T) {
	full := "同意上线，周五发版"
	quote := &infoflowQuote{
		MessageID: "Q-2",
		Preview:   "同意上线…",
		Body: []map[string]any{
			{"type": "TEXT", "content": "同意上线…"},
			{"type": "TEXT", "content": "同意上线..."},
			{"type": "IMAGE", "downloadurl": "https://example.com/q.png"},
		},
	}
	got := flattenQuoted(&infoflowEventMessage{Reply: quote}, &quoteOrigin{QueryID: "Q-2", State: quoteStateOK, Source: "api", Text: full})
	if n := countSubstring(got, "同意上线"); n != 1 {
		t.Fatalf("want one 同意上线 (inside full text), got %d:\n%s", n, got)
	}
	if n := countSubstring(got, "[IMAGE] https://example.com/q.png"); n != 1 {
		t.Fatalf("image row missing, count=%d", n)
	}
	if n := countSubstring(got, quoteTagFull); n != 1 {
		t.Fatalf("full label count=%d", n)
	}
}

// logRawFrame prints the three fields N86 acceptance is about, so the evidence
// run shows each frame's own Raw instead of only a PASS line.
func logRawFrame(t *testing.T, view rawScopeMessage) {
	t.Helper()
	inner := view.inner()
	origin, hasOrigin := view.raw["multica_quote_origin"]
	reply := inner["reply"]
	if m, ok := reply.(map[string]any); ok {
		reply = m["messageid"]
	}
	t.Logf("frame=%s chat=%s hasSelected=%v Raw.message.reply=%v Raw.message.quote=%v Raw.quote=%v origin=%v state=%v",
		view.messageID, view.chatID, view.hasQuote, reply, inner["quote"], view.raw["quote"], hasOrigin, originState(origin))
}

func originState(origin any) any {
	if m, ok := origin.(map[string]any); ok {
		return m["state"]
	}
	return nil
}

func countSubstring(s, sub string) int {
	n := 0
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			n++
		}
	}
	return n
}
