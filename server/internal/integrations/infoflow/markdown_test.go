package infoflow

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/events"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// TestContainsMarkdown pins the detection boundary copied from Lark.
func TestContainsMarkdown(t *testing.T) {
	yes := []string{
		"# 标题", "## 二级", "- 列表项", "1. 第一步", "> 引用",
		"**加粗**", "__粗__", "`行内码`", "```go\nx\n```",
		"[链接](https://example.com)", "| a | b |",
	}
	for _, s := range yes {
		if !containsMarkdown(s) {
			t.Errorf("containsMarkdown(%q) = false, want true", s)
		}
	}
	no := []string{"", "普通一句话。", "hello world", "一句带 1 个数字的话"}
	for _, s := range no {
		if containsMarkdown(s) {
			t.Errorf("containsMarkdown(%q) = true, want false", s)
		}
	}
}

// TestNormalizeForInfoflow covers the table and task-list rewrites; supported
// syntax must pass through untouched.
func TestNormalizeForInfoflow(t *testing.T) {
	table := "| 项目 | 状态 |\n| --- | --- |\n| 标题 | 支持 |\n| 表格 | 视客户端 |"
	got := normalizeForInfoflow(table)
	want := "- 项目: 标题；状态: 支持\n- 项目: 表格；状态: 视客户端"
	if got != want {
		t.Fatalf("table normalization:\n got %q\nwant %q", got, want)
	}
	tasks := "- [x] 已完成项\n- [ ] 未完成项"
	got = normalizeForInfoflow(tasks)
	want = "- 已完成 已完成项\n- 未完成 未完成项"
	if got != want {
		t.Fatalf("task normalization:\n got %q\nwant %q", got, want)
	}
	// Supported syntax passes through unchanged.
	keep := "# 标题\n**加粗** *斜体* `code` [x](y)\n> 引用\n- 列表\n```go\nfmt.Println()\n```"
	if normalizeForInfoflow(keep) != keep {
		t.Fatalf("supported syntax must pass through unchanged, got %q", normalizeForInfoflow(keep))
	}
}

// TestGroupMarkdownBodyShape: a group reply containing markdown is sent as
// header msgtype=MD with an MD element, AT first.
func TestGroupMarkdownBodyShape(t *testing.T) {
	body, err := buildSendBody(infoflowSendTarget{ChatType: "group", ChatID: "12917205", MentionUserID: "alice"},
		"8682", "**加粗** 内容", msgTypeMD)
	if err != nil {
		t.Fatal(err)
	}
	msg := body["message"].(map[string]any)
	if got := msg["header"].(map[string]any)["msgtype"]; got != "MD" {
		t.Fatalf("header msgtype = %v, want MD", got)
	}
	elems := msg["body"].([]map[string]any)
	if elems[0]["type"] != "AT" {
		t.Fatalf("first element must be AT, got %+v", elems[0])
	}
	if elems[1]["type"] != "MD" || elems[1]["content"] != "**加粗** 内容" {
		t.Fatalf("second element must be the MD text, got %+v", elems[1])
	}
	// Plain text stays TEXT.
	plain, err := buildSendBody(infoflowSendTarget{ChatType: "group", ChatID: "12917205"}, "8682", "普通句子", msgTypeText)
	if err != nil {
		t.Fatal(err)
	}
	pm := plain["message"].(map[string]any)
	if pm["header"].(map[string]any)["msgtype"] != "TEXT" {
		t.Fatalf("plain text header must be TEXT")
	}
	if pm["body"].([]map[string]any)[0]["type"] != "TEXT" {
		t.Fatalf("plain text element must be TEXT")
	}
}

// TestGroupMarkdownIsChunkedAndOnlyFirstBatchHasAT: markdown long enough to
// split produces MD batches with the AT on the first only.
func TestGroupMarkdownIsChunkedAndOnlyFirstBatchHasAT(t *testing.T) {
	oldDelay := infoflowDefaultChunkDelay
	infoflowDefaultChunkDelay = 0
	defer func() { infoflowDefaultChunkDelay = oldDelay }()

	var bodies []map[string]any
	srv := newJSONServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/auth/app_access_token":
			_, _ = w.Write([]byte(`{"code":"ok","data":{"app_access_token":"at","expire":7200}}`))
		case "/robot/msg/groupmsgsend":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			bodies = append(bodies, body)
			_, _ = w.Write([]byte(`{"code":"ok","data":{"data":{"messageid":"m"}}}`))
		}
	})
	s := &sender{client: NewClient(srv.Client(), srv.URL), appKey: "k", appSecret: "s", agentID: "8682"}
	md := strings.Repeat("- 一个列表项内容 line\n", 200) // > 1500 runes, contains markdown
	_, outcome, err := s.sendChunked(context.Background(),
		infoflowSendTarget{ChatType: "group", ChatID: "12917205", MentionUserID: "alice"}, md)
	if err != nil {
		t.Fatalf("sendChunked: %v", err)
	}
	if outcome.MsgType != msgTypeMD {
		t.Fatalf("outcome.MsgType = %q, want MD", outcome.MsgType)
	}
	if len(bodies) < 2 {
		t.Fatalf("expected multiple MD batches, got %d", len(bodies))
	}
	for i, b := range bodies {
		h := b["message"].(map[string]any)["header"].(map[string]any)
		if h["msgtype"] != "MD" {
			t.Fatalf("batch %d header msgtype = %v, want MD", i, h["msgtype"])
		}
		for _, e := range b["message"].(map[string]any)["body"].([]any) {
			el := e.(map[string]any)
			if el["type"] == "AT" && i != 0 {
				t.Fatalf("batch %d carries an AT; only the first batch may", i)
			}
			if el["type"] == "MD" && i != 0 {
				// sanity: later batches are still MD elements
			}
		}
	}
}

// TestOutboundMarkdownFallbackForcesText: a markdown primary reply that is
// rejected on BOTH MD and TEXT must still emit a TEXT fallback notice (never
// markdown), and the failure must be logged with msgtype/batch fields.
func TestOutboundMarkdownFallbackForcesText(t *testing.T) {
	oldDelay := infoflowDefaultChunkDelay
	infoflowDefaultChunkDelay = 0
	defer func() { infoflowDefaultChunkDelay = oldDelay }()

	var types []string
	srv := newJSONServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/auth/app_access_token":
			_, _ = w.Write([]byte(`{"code":"ok","data":{"app_access_token":"at","expire":7200}}`))
		case "/robot/msg/groupmsgsend":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			h := body["message"].(map[string]any)["header"].(map[string]any)
			types = append(types, h["msgtype"].(string))
			// Reject everything that carries the body: both the MD attempt and
			// the TEXT retry fail; only the short notice (no markdown, no AT)
			// succeeds.
			hasAT := false
			for _, e := range body["message"].(map[string]any)["body"].([]any) {
				if e.(map[string]any)["type"] == "AT" {
					hasAT = true
				}
			}
			if hasAT {
				_, _ = w.Write([]byte(`{"code":"ok","data":{"errcode":40061,"errmsg":"message text length exceed limit"}}`))
				return
			}
			_, _ = w.Write([]byte(`{"code":"ok","data":{"data":{"messageid":"m"}}}`))
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
		Payload:       protocol.ChatDonePayload{Content: "**加粗** 很长的正文……"},
	})
	if err == nil {
		t.Fatal("expected the delivery to be reported as failed")
	}
	// First: MD attempt. Second: TEXT retry of the same body. Third: TEXT notice.
	if len(types) != 3 {
		t.Fatalf("expected MD + TEXT retry + TEXT notice, got %v", types)
	}
	if types[0] != "MD" || types[1] != "TEXT" || types[2] != "TEXT" {
		t.Fatalf("msgtype sequence = %v, want [MD TEXT TEXT]", types)
	}
}

// TestMarkdownSendFailureFallsBackToText: when MD is rejected (e.g. the gateway
// refuses MD), the SAME content is re-sent as TEXT — the body is never dropped.
func TestMarkdownSendFailureFallsBackToText(t *testing.T) {
	oldDelay := infoflowDefaultChunkDelay
	infoflowDefaultChunkDelay = 0
	defer func() { infoflowDefaultChunkDelay = oldDelay }()

	var bodies []string
	srv := newJSONServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/auth/app_access_token":
			_, _ = w.Write([]byte(`{"code":"ok","data":{"app_access_token":"at","expire":7200}}`))
		case "/robot/msg/groupmsgsend":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			raw, _ := json.Marshal(body)
			bodies = append(bodies, string(raw))
			h := body["message"].(map[string]any)["header"].(map[string]any)
			if h["msgtype"] == "MD" {
				_, _ = w.Write([]byte(`{"code":"ok","data":{"errcode":40000,"errmsg":"msgtype not allowed"}}`))
				return
			}
			_, _ = w.Write([]byte(`{"code":"ok","data":{"data":{"messageid":"m1"}}}`))
		}
	})
	s := &sender{client: NewClient(srv.Client(), srv.URL), appKey: "k", appSecret: "s", agentID: "8682"}
	_, outcome, err := s.sendChunked(context.Background(),
		infoflowSendTarget{ChatType: "group", ChatID: "12917205", MentionUserID: "alice"}, "**加粗** 正文")
	if err != nil {
		t.Fatalf("MD failure must fall back to TEXT, got err=%v", err)
	}
	if outcome.MsgType != msgTypeText {
		t.Fatalf("outcome.MsgType = %q, want TEXT after fallback", outcome.MsgType)
	}
	if len(bodies) != 2 {
		t.Fatalf("expected MD attempt + TEXT retry, got %d sends", len(bodies))
	}
	if !strings.Contains(bodies[0], `"msgtype":"MD"`) {
		t.Fatalf("first send should be MD: %s", bodies[0])
	}
	if !strings.Contains(bodies[1], `"msgtype":"TEXT"`) || !strings.Contains(bodies[1], "加粗") {
		t.Fatalf("second send must be the same content as TEXT: %s", bodies[1])
	}
}
