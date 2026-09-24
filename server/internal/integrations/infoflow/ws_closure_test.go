package infoflow

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/multica-ai/multica/server/internal/integrations/channel"
)

// fakeWSConn is an in-memory peer used to drive the connector without a real
// InfoFlow gateway. The server-side frame is written by the test; the
// connector's replies (event_ack / pong) are captured for assertions.
type fakeWSConn struct {
	mu      sync.Mutex
	reads   [][]byte
	written [][]byte
}

func (f *fakeWSConn) ReadMessage() (int, []byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.reads) == 0 {
		return 0, nil, context.Canceled
	}
	next := f.reads[0]
	f.reads = f.reads[1:]
	return websocket.BinaryMessage, next, nil
}

func (f *fakeWSConn) WriteMessage(_ int, data []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.written = append(f.written, append([]byte(nil), data...))
	return nil
}

func (f *fakeWSConn) SetReadDeadline(time.Time) error  { return nil }
func (f *fakeWSConn) SetWriteDeadline(time.Time) error { return nil }
func (f *fakeWSConn) Close() error                     { return nil }

type fakeDialer struct{ conn *fakeWSConn }

func (d fakeDialer) dial(context.Context, string) (wsConn, error) { return d.conn, nil }

// TestWSProtocolClosureFakePeer drives one inbound frame through the connector
// exactly as a real gateway would, and asserts the adapter (a) normalizes the
// payload and (b) ACKs the event with an event_ack frame. This is the protocol
// layer closure the N3 v2 plan requires when D1 blocks a real InfoFlow peer.
func TestWSProtocolClosureFakePeer(t *testing.T) {
	payload := json.RawMessage(`{"FromUserId":"u1","FromUserName":"alice","Content":"hello","MsgId":"m1","MsgType":"text"}`)
	frameBytes, err := marshalFrame(Frame{SeqID: 9, LogID: "log-9", Method: FrameData, Payload: payload})
	if err != nil {
		t.Fatal(err)
	}
	conn := &fakeWSConn{reads: [][]byte{frameBytes}}
	var got channel.InboundMessage
	connector := &wsConnector{
		open:   func(context.Context) (string, error) { return "ws://fake", nil },
		dialer: fakeDialer{conn: conn},
		onFrame: func(_ context.Context, frame Frame) error {
			msg, ok := inboundFromPayload(frame.Payload, "14534", "bot")
			if !ok {
				t.Fatal("expected normalized inbound message")
			}
			got = msg
			return nil
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = connector.run(ctx)
	if got.Source.SenderID != "u1" || strings.TrimSpace(got.Text) != "hello" {
		t.Fatalf("unexpected inbound: %+v", got)
	}
	conn.mu.Lock()
	defer conn.mu.Unlock()
	if len(conn.written) == 0 {
		t.Fatal("expected an event_ack write")
	}
	ack, err := unmarshalFrame(conn.written[len(conn.written)-1])
	if err != nil {
		t.Fatal(err)
	}
	if ack.Method != FrameData || headerValue(ack.Headers, "type") != "event_ack" {
		t.Fatalf("expected event_ack frame, got %+v", ack)
	}
}

// TestWSEndpointFetchAndPermissionClassification covers the two-phase endpoint
// fetch against a fake HTTP gateway and the 40000 classification for the
// single-chat send API.
func TestWSEndpointFetchAndPermissionClassification(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/open/ws/endpoint", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"msg":"success","data":{"url":"ws://example/ws?connection_id=x","client_config":{"ping_interval":60}}}`))
	})
	mux.HandleFunc("/app/message/send", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"code":"ok","data":{"errcode":40000,"errmsg":"Api not allowed"}}`))
	})
	mux.HandleFunc("/robot/msg/groupmsgsend", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"code":"ok","data":{"errcode":40001,"errmsg":"group not follow"}}`))
	})
	mux.HandleFunc("/auth/app_access_token", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"code":"ok","data":{"app_access_token":"at_test","expire":7200}}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	url, err := fetchWSEndpoint(context.Background(), srv.Client(), srv.URL, "key", "secret")
	if err != nil || !strings.Contains(url, "connection_id=x") {
		t.Fatalf("endpoint fetch: url=%q err=%v", url, err)
	}
	client := NewClient(srv.Client(), srv.URL)
	_, p2pErr := (&sender{client: client, appKey: "key", appSecret: "secret", agentID: "14534"}).
		sendText(context.Background(), infoflowSendTarget{ChatType: "p2p", ChatID: "u1", SenderID: "u1"}, "hi")
	if !IsPermissionNotGranted(p2pErr) {
		t.Fatalf("expected permission classification, got %v", p2pErr)
	}
	_, groupErr := (&sender{client: client, appKey: "key", appSecret: "secret", agentID: "14534"}).
		sendText(context.Background(), infoflowSendTarget{ChatType: "group", ChatID: "123"}, "hi")
	if groupErr == nil || IsPermissionNotGranted(groupErr) {
		t.Fatalf("expected group business error, got %v", groupErr)
	}
	if !strings.Contains(groupErr.Error(), "group not follow") {
		t.Fatalf("expected group error message, got %v", groupErr)
	}
}
