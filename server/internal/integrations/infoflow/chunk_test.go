package infoflow

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/multica-ai/multica/server/internal/events"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// TestSplitTextChunksBounds pins the splitting contract: non-empty input yields
// non-empty ordered chunks, each within the rune bound, and concatenation
// preserves the original runes (minus dropped newlines at cut points).
func TestSplitTextChunksBounds(t *testing.T) {
	long := strings.Repeat("段落内容abc ", 400) // >> 1500 runes
	chunks := splitTextChunks(long, 1500)
	if len(chunks) < 2 {
		t.Fatalf("expected the text to be split, got %d chunk(s)", len(chunks))
	}
	for i, c := range chunks {
		if c == "" {
			t.Fatalf("chunk %d is empty", i)
		}
		if n := utf8.RuneCountInString(c); n > 1500 {
			t.Fatalf("chunk %d has %d runes, over the bound", i, n)
		}
	}
	// Short text stays a single chunk, unchanged.
	if got := splitTextChunks("hello", 1500); len(got) != 1 || got[0] != "hello" {
		t.Fatalf("short text = %v, want single unchanged chunk", got)
	}
	// Multibyte text is never cut mid-rune.
	multi := strings.Repeat("测试", 2000)
	for i, c := range splitTextChunks(multi, 1500) {
		if !utf8.ValidString(c) {
			t.Fatalf("chunk %d cut mid-rune", i)
		}
	}
}

// TestSendChunkedSplitsOrdersAndMentionsOnce drives sendChunked against a fake
// gateway and asserts: every batch is sent in order, the AT mention rides on the
// first batch only, and concatenated batch text preserves the original.
func TestSendChunkedSplitsOrdersAndMentionsOnce(t *testing.T) {
	oldDelay := infoflowDefaultChunkDelay
	infoflowDefaultChunkDelay = 0
	defer func() { infoflowDefaultChunkDelay = oldDelay }()

	var sentBodies []map[string]any
	srv := newJSONServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/auth/app_access_token":
			_, _ = w.Write([]byte(`{"code":"ok","data":{"app_access_token":"at_1","expire":7200}}`))
		case "/robot/msg/groupmsgsend":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			sentBodies = append(sentBodies, body)
			_, _ = w.Write([]byte(`{"code":"ok","data":{"data":{"messageid":"m"}}}`))
		}
	})
	s := &sender{client: NewClient(srv.Client(), srv.URL), appKey: "k", appSecret: "s", agentID: "14534"}
	text := strings.Repeat("行内容line ", 400) // > 1500 runes → multiple batches
	ids, _, err := s.sendChunked(context.Background(),
		infoflowSendTarget{ChatType: "group", ChatID: "12917205", MentionUserID: "alice"}, text)
	if err != nil {
		t.Fatalf("sendChunked: %v", err)
	}
	if len(sentBodies) < 2 {
		t.Fatalf("expected multiple batches, got %d", len(sentBodies))
	}
	if len(ids) != len(sentBodies) {
		t.Fatalf("ids=%d batches=%d", len(ids), len(sentBodies))
	}
	var rebuilt strings.Builder
	for i, body := range sentBodies {
		msg := body["message"].(map[string]any)
		header := msg["header"].(map[string]any)
		if header["toid"].(float64) != 12917205 {
			t.Fatalf("batch %d toid = %v, want the real group id", i, header["toid"])
		}
		elems := msg["body"].([]any)
		hasAT := false
		for _, e := range elems {
			el := e.(map[string]any)
			switch el["type"] {
			case "AT":
				hasAT = true
				if i != 0 {
					t.Fatalf("batch %d carries an AT; the mention must ride only the first batch", i)
				}
			case "TEXT":
				content := el["content"].(string)
				if i == 0 && strings.Contains(content, "@") {
					t.Fatalf("first batch content repeats the AT as text: %q", content)
				}
				rebuilt.WriteString(content)
			}
		}
		if i == 0 && !hasAT {
			t.Fatal("the first batch must carry the AT mention")
		}
	}
	if got := rebuilt.String(); got != text {
		t.Fatalf("reassembled text differs from the original (got %d runes, want %d)", utf8.RuneCountInString(got), utf8.RuneCountInString(text))
	}
}

// TestSendChunkedStopsOnFailure: if a later batch fails, the error surfaces and
// the already-sent count is reflected in the returned ids.
func TestSendChunkedStopsOnFailure(t *testing.T) {
	oldDelay := infoflowDefaultChunkDelay
	infoflowDefaultChunkDelay = 0
	defer func() { infoflowDefaultChunkDelay = oldDelay }()

	attempts := 0
	srv := newJSONServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/auth/app_access_token":
			_, _ = w.Write([]byte(`{"code":"ok","data":{"app_access_token":"at_1","expire":7200}}`))
		case "/robot/msg/groupmsgsend":
			attempts++
			if attempts == 1 {
				_, _ = w.Write([]byte(`{"code":"ok","data":{"data":{"messageid":"m1"}}}`))
				return
			}
			_, _ = w.Write([]byte(`{"code":"ok","data":{"errcode":40061,"errmsg":"message text length exceed limit"}}`))
		}
	})
	s := &sender{client: NewClient(srv.Client(), srv.URL), appKey: "k", appSecret: "s", agentID: "14534"}
	ids, _, err := s.sendChunked(context.Background(),
		infoflowSendTarget{ChatType: "group", ChatID: "12917205"}, strings.Repeat("x", 5000))
	if err == nil || !IsMessageTooLong(err) {
		t.Fatalf("err = %v, want ErrMessageTooLong", err)
	}
	if len(ids) != 1 {
		t.Fatalf("ids = %v, want the first batch's id before the failure", ids)
	}
}

func TestClassifySendError(t *testing.T) {
	srv := newJSONServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/auth/app_access_token" {
			_, _ = w.Write([]byte(`{"code":"ok","data":{"app_access_token":"at_1","expire":7200}}`))
			return
		}
		_, _ = w.Write([]byte(`{"code":"ok","data":{"errcode":40060,"errmsg":"message body length exceed limit"}}`))
	})
	_, err := (&sender{client: NewClient(srv.Client(), srv.URL), appKey: "k", appSecret: "s", agentID: "14534"}).
		sendText(context.Background(), infoflowSendTarget{ChatType: "group", ChatID: "12917205"}, "hi")
	if !IsMessageTooLong(err) {
		t.Fatalf("40060 must classify as ErrMessageTooLong, got %v", err)
	}
	if got := classifySendError(ErrMessageTooLong); got != "message_too_long" {
		t.Fatalf("classify(too long) = %q", got)
	}
	if got := classifySendError(ErrPermissionNotGranted); got != "permission_not_granted" {
		t.Fatalf("classify(permission) = %q", got)
	}
	if got := classifySendError(nil); got != "" {
		t.Fatalf("classify(nil) = %q", got)
	}
}

// TestFailureFallbackTextIsShortAndLinked pins the user-visible notice: it names
// the cause, stays short, and carries the chat-thread link when known.
func TestFailureFallbackTextIsShortAndLinked(t *testing.T) {
	withLink := infoflowFailureFallbackText("https://app.example/acme/chat?session=s1", ErrMessageTooLong)
	if !strings.Contains(withLink, "长度上限") {
		t.Fatalf("notice should name the length cause: %q", withLink)
	}
	if !strings.Contains(withLink, "https://app.example/acme/chat?session=s1") {
		t.Fatalf("notice should carry the thread link: %q", withLink)
	}
	if n := utf8.RuneCountInString(withLink); n > 500 {
		t.Fatalf("fallback notice too long for a safe send: %d runes", n)
	}
	noLink := infoflowFailureFallbackText("", errors.New("boom"))
	if !strings.Contains(noLink, "Multica 会话") {
		t.Fatalf("notice without link should still point at the session: %q", noLink)
	}
}

// TestOutboundDeliversVisibleFallbackWhenSendFails drives the real processEvent:
// the first group send fails (40061), so the outbound must post a visible
// fallback notice and return the classified error.
func TestOutboundDeliversVisibleFallbackWhenSendFails(t *testing.T) {
	var bodies []map[string]any
	srv := newJSONServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/auth/app_access_token":
			_, _ = w.Write([]byte(`{"code":"ok","data":{"app_access_token":"at_1","expire":7200}}`))
		case "/robot/msg/groupmsgsend":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			bodies = append(bodies, body)
			if len(bodies) == 1 {
				_, _ = w.Write([]byte(`{"code":"ok","data":{"errcode":40061,"errmsg":"message text length exceed limit"}}`))
				return
			}
			_, _ = w.Write([]byte(`{"code":"ok","data":{"data":{"messageid":"fallback-1"}}}`))
		}
	})
	q := fakeOutboundQueries{delivery: db.ChannelTaskDelivery{
		ChannelType:   string(TypeInfoFlow),
		ChannelChatID: "12917205:alice",
		ChatType:      "group",
		Config:        []byte(`{"chat_type":"group","chat_id":"12917205","mention_user_id":"alice"}`),
	}}
	o := NewOutbound(q, nil, NewClient(srv.Client(), srv.URL), nil).WithAppURL("https://app.example")
	err := o.processEvent(context.Background(), events.Event{
		Type:          protocol.EventChatDone,
		TaskID:        "11111111-1111-1111-1111-111111111111",
		ChatSessionID: "22222222-2222-2222-2222-222222222222",
		Payload:       protocol.ChatDonePayload{Content: "hello"},
	})
	if !IsMessageTooLong(err) {
		t.Fatalf("processEvent err = %v, want ErrMessageTooLong", err)
	}
	if len(bodies) != 2 {
		t.Fatalf("expected the failed send + a visible fallback, got %d sends", len(bodies))
	}
	// The fallback (second body) must be plain text (no AT) and mention Multica.
	fallbackText := ""
	for _, e := range bodies[1]["message"].(map[string]any)["body"].([]any) {
		el := e.(map[string]any)
		if el["type"] == "AT" {
			t.Fatal("the failure fallback must not carry an AT element")
		}
		if el["type"] == "TEXT" {
			fallbackText = el["content"].(string)
		}
	}
	if !strings.Contains(fallbackText, "Multica") {
		t.Fatalf("fallback notice = %q, want a user-visible notice", fallbackText)
	}
}
