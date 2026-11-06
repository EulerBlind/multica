package infoflow

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/multica-ai/multica/server/internal/integrations/channel"
)

// r66LookupLatency mirrors N81's slow gateway (2.8s per lookup). Two
// back-to-back frames used to produce ack1=2.80s / ack2=5.60s because the ACK
// and the read loop waited for the lookup.
const r66LookupLatency = 2800 * time.Millisecond

type quoteRecorder struct {
	mu   sync.Mutex
	msgs []channel.InboundMessage
	at   []time.Time
}

func (r *quoteRecorder) handle(_ context.Context, msg channel.InboundMessage) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.msgs = append(r.msgs, msg)
	r.at = append(r.at, time.Now())
	return nil
}

func (r *quoteRecorder) snapshot() ([]channel.InboundMessage, []time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]channel.InboundMessage(nil), r.msgs...), append([]time.Time(nil), r.at...)
}

func (r *quoteRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.msgs)
}

// streamWSConn feeds frames pushed by the test and timestamps every event_ack
// the connector writes.
type streamWSConn struct {
	frames chan []byte
	ready  chan struct{}
	closed chan struct{}
	once   sync.Once

	mu   sync.Mutex
	acks []time.Time
}

func newStreamWSConn() *streamWSConn {
	return &streamWSConn{frames: make(chan []byte), ready: make(chan struct{}), closed: make(chan struct{})}
}

func (c *streamWSConn) ReadMessage() (int, []byte, error) {
	c.once.Do(func() { close(c.ready) })
	select {
	case frame := <-c.frames:
		return websocket.BinaryMessage, frame, nil
	case <-c.closed:
		return 0, nil, context.Canceled
	}
}

func (c *streamWSConn) WriteMessage(_ int, data []byte) error {
	frame, err := unmarshalFrame(data)
	if err != nil {
		return nil
	}
	if frame.Method == FrameData && headerValue(frame.Headers, "type") == "event_ack" {
		c.mu.Lock()
		c.acks = append(c.acks, time.Now())
		c.mu.Unlock()
	}
	return nil
}

func (c *streamWSConn) SetReadDeadline(time.Time) error  { return nil }
func (c *streamWSConn) SetWriteDeadline(time.Time) error { return nil }
func (c *streamWSConn) Close() error {
	select {
	case <-c.closed:
	default:
		close(c.closed)
	}
	return nil
}

// streamDialer hands the connector the in-memory peer above.
type streamDialer struct{ conn wsConn }

func (d streamDialer) dial(context.Context, string) (wsConn, error) { return d.conn, nil }

func (c *streamWSConn) push(t *testing.T, frame Frame) {
	t.Helper()
	raw, err := marshalFrame(frame)
	if err != nil {
		t.Fatalf("marshal frame: %v", err)
	}
	select {
	case c.frames <- raw:
	case <-time.After(2 * time.Second):
		t.Fatal("connector did not read the frame in time")
	}
}

func (c *streamWSConn) ackTimes() []time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]time.Time(nil), c.acks...)
}

func (c *streamWSConn) waitForAcks(t *testing.T, n int) []time.Time {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if acks := c.ackTimes(); len(acks) >= n {
			return acks
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("only %d acks received", len(c.ackTimes()))
	return nil
}

func waitForMessages(t *testing.T, rec *quoteRecorder, n int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if rec.count() >= n {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("only %d messages reached the handler", rec.count())
}

func r66Payload(msgID, quoteID string) []byte {
	return []byte(`{"groupid":"G-R66","message":{"header":{"fromuserid":"U-S","toid":"G-R66","totype":"GROUP","msgtype":"TEXT","messageid":"` + msgID + `"},"reply":{"messageid":"` + quoteID + `","header":{"fromuserid":"U-Q"},"preview":"截断预览…"},"body":[{"type":"AT","atuserids":["BOT-1"]},{"type":"TEXT","content":"hi ` + msgID + `"}]}}`)
}

// startR66Channel builds the production factory channel and runs a real
// wsConnector against the in-memory peer.
func startR66Channel(t *testing.T, srvURL string, rec *quoteRecorder) (*streamWSConn, func()) {
	t.Helper()
	logger := discardLogger()
	factory := newFactory(ChannelDeps{Client: NewClient(nil, srvURL), Logger: logger})
	cfg := channel.Config{
		Raw:     json.RawMessage(`{"app_id":"APP-R66","app_key":"KEY-1","app_secret_encrypted":"U0VTLTE=","bot_user_id":"BOT-1"}`),
		Handler: rec.handle,
	}
	built, err := factory(cfg)
	if err != nil {
		t.Fatalf("factory: %v", err)
	}
	ch := built.(*infoflowChannel)
	ch.logger = logger
	// Connect owns the worker lifecycle in production; this harness drives the
	// connector directly, so wire the same pool here with its own context.
	workersCtx, workersCancel := context.WithCancel(context.Background())
	ch.workers = newQuoteWorkers(workersCtx, defaultQuoteWorkers, logger)

	conn := newStreamWSConn()
	connector := &wsConnector{
		open:    func(context.Context) (string, error) { return "ws://fake", nil },
		dialer:  streamDialer{conn: conn},
		onFrame: ch.handleFrameQueued,
		logger:  logger,
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = connector.run(ctx)
	}()
	select {
	case <-conn.ready:
	case <-time.After(2 * time.Second):
		cancel()
		t.Fatal("connector never read a frame")
	}
	stop := func() {
		workersCancel()
		cancel()
		_ = conn.Close()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
		}
	}
	return conn, stop
}

// TestQuoteWorkersAckBeforeSlowLookup is the R66 regression: two back-to-back
// frames with a 2.8s lookup each must be acknowledged within 200ms, while both
// messages still reach the handler after their lookup, in order.
func TestQuoteWorkersAckBeforeSlowLookup(t *testing.T) {
	gw := &quoteGateway{message: func(_ int, w http.ResponseWriter, r *http.Request) {
		time.Sleep(r66LookupLatency)
		_, _ = w.Write([]byte(`{"code":"ok","data":{"msgType":"text","content":"全文-` + r.URL.Query().Get("msgId") + `"}}`))
	}}
	srv := gw.start(t)
	rec := &quoteRecorder{}
	conn, stop := startR66Channel(t, srv.URL, rec)
	defer stop()

	sent1 := time.Now()
	conn.push(t, Frame{SeqID: 21, Method: FrameData, Payload: r66Payload("M-A", "Q-A")})
	time.Sleep(100 * time.Millisecond)
	sent2 := time.Now()
	conn.push(t, Frame{SeqID: 22, Method: FrameData, Payload: r66Payload("M-B", "Q-B")})

	acks := conn.waitForAcks(t, 2)
	ack1, ack2 := acks[0].Sub(sent1), acks[1].Sub(sent2)
	t.Logf("lookup=2.8s ack1=%s ack2=%s", ack1, ack2)
	if ack1 > 200*time.Millisecond || ack2 > 200*time.Millisecond {
		t.Fatalf("ACK must not wait for the lookup: ack1=%s ack2=%s", ack1, ack2)
	}
	if acks[0].After(acks[1]) {
		t.Fatalf("ack order reversed: %v", acks)
	}

	waitForMessages(t, rec, 2, 15*time.Second)
	msgs, at := rec.snapshot()
	for _, want := range []string{"全文-Q-A", "hi M-A"} {
		if !strings.Contains(msgs[0].Text, want) {
			t.Fatalf("first message missing %q: %s", want, msgs[0].Text)
		}
	}
	if !strings.Contains(msgs[1].Text, "全文-Q-B") || !strings.Contains(msgs[1].Text, "hi M-B") {
		t.Fatalf("second message not enriched or out of order: %s", msgs[1].Text)
	}
	if !at[0].Before(at[1]) {
		t.Fatalf("same-chat order lost: %v", at)
	}
	t.Logf("handler order: first=%s after %s, second=%s after %s",
		msgs[0].MessageID, at[0].Sub(sent1), msgs[1].MessageID, at[1].Sub(sent1))
}

// TestQuoteWorkersAckBaselineOff keeps the off-switch baseline: with the lookup
// disabled both ACKs stay fast and no getSingleMsg request is made.
func TestQuoteWorkersAckBaselineOff(t *testing.T) {
	t.Setenv(quoteOriginEnv, "off")
	gw := &quoteGateway{message: func(_ int, w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"code":"ok","data":{"content":"should not be used"}}`))
	}}
	srv := gw.start(t)
	rec := &quoteRecorder{}
	conn, stop := startR66Channel(t, srv.URL, rec)
	defer stop()

	sent1 := time.Now()
	conn.push(t, Frame{SeqID: 31, Method: FrameData, Payload: r66Payload("M-C", "Q-C")})
	sent2 := time.Now()
	conn.push(t, Frame{SeqID: 32, Method: FrameData, Payload: r66Payload("M-D", "Q-D")})

	acks := conn.waitForAcks(t, 2)
	t.Logf("off baseline ack1=%s ack2=%s", acks[0].Sub(sent1), acks[1].Sub(sent2))
	if acks[0].Sub(sent1) > 200*time.Millisecond || acks[1].Sub(sent2) > 200*time.Millisecond {
		t.Fatalf("off baseline ACK too slow: %v", acks)
	}
	if _, calls := gw.calls(); calls != 0 {
		t.Fatalf("off must not call getSingleMsg, calls=%d", calls)
	}
	waitForMessages(t, rec, 2, 3*time.Second)
}

// TestQuoteWorkersPreserveChatOrder checks the queue's FIFO promise directly.
func TestQuoteWorkersPreserveChatOrder(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	workers := newQuoteWorkers(ctx, 2, discardLogger())

	var mu sync.Mutex
	var a, b []string
	for i := 0; i < 60; i++ {
		msg := fmt.Sprintf("a-%02d", i)
		workers.enqueue("chat-a", func() {
			mu.Lock()
			a = append(a, msg)
			mu.Unlock()
		})
	}
	for i := 0; i < 20; i++ {
		msg := fmt.Sprintf("b-%02d", i)
		workers.enqueue("chat-b", func() {
			mu.Lock()
			b = append(b, msg)
			mu.Unlock()
		})
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		done := len(a) == 60 && len(b) == 20
		mu.Unlock()
		if done {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(a) != 60 || len(b) != 20 {
		t.Fatalf("missing tasks: a=%d b=%d", len(a), len(b))
	}
	for i, want := range a {
		if want != fmt.Sprintf("a-%02d", i) {
			t.Fatalf("chat-a order broken at %d: %q", i, want)
		}
	}
	for i, want := range b {
		if want != fmt.Sprintf("b-%02d", i) {
			t.Fatalf("chat-b order broken at %d: %q", i, want)
		}
	}
}

// TestChatKeyFromRaw covers the queue key derivation that runs before
// normalization.
func TestChatKeyFromRaw(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"group", `{"groupid":"G-1","message":{"header":{"fromuserid":"U-1"}}}`, "G-1"},
		{"p2p top-level", `{"FromUserId":"U-2","Content":"hi"}`, "U-2"},
		{"p2p nested", `{"message":{"header":{"fromuserid":"U-3"}}}`, "U-3"},
		{"none", `{"Content":"hi"}`, ""},
		{"invalid", `{`, ""},
	}
	for _, tc := range cases {
		if got := chatKeyFromRaw([]byte(tc.raw)); got != tc.want {
			t.Fatalf("%s: chatKeyFromRaw=%q want %q", tc.name, got, tc.want)
		}
	}
}
