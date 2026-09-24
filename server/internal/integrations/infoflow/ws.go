package infoflow

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const (
	infoflowPingInterval = 55 * time.Second
	infoflowReadDeadline = 90 * time.Second
	infoflowWriteTimeout = 10 * time.Second
)

// endpointResponse is the success shape of the two-phase long-connection
// endpoint fetch.
type endpointResponse struct {
	Code int `json:"code"`
	Msg  string `json:"msg"`
	Data struct {
		URL          string `json:"url"`
		ClientConfig struct {
			ReconnectCount   int `json:"reconnect_count"`
			ReconnectInterval int `json:"reconnect_interval"`
			ReconnectNonce   int `json:"reconnect_nonce"`
			PingInterval     int `json:"ping_interval"`
		} `json:"client_config"`
	} `json:"data"`
}

// wsConn is the slice of *websocket.Conn the connector uses.
type wsConn interface {
	ReadMessage() (messageType int, p []byte, err error)
	WriteMessage(messageType int, data []byte) error
	SetReadDeadline(t time.Time) error
	SetWriteDeadline(t time.Time) error
	Close() error
}

type wsDialer interface {
	dial(ctx context.Context, dialURL string) (wsConn, error)
}

type gorillaWSDialer struct{}

func (gorillaWSDialer) dial(ctx context.Context, dialURL string) (wsConn, error) {
	conn, _, err := websocket.DefaultDialer.DialContext(ctx, dialURL, nil)
	if err != nil {
		return nil, err
	}
	return conn, nil
}

// wsConnector opens one InfoFlow socket session. Reconnect/backoff/lease live
// in the shared engine.Supervisor, so run() returns nil on graceful close and a
// non-nil error on a broken socket.
type wsConnector struct {
	httpClient *http.Client
	gateway    string
	appKey     string
	appSecret  string

	dialer    wsDialer
	open      func(ctx context.Context) (string, error)
	onFrame   func(ctx context.Context, frame Frame) error
	logger    *slog.Logger

	pingInterval time.Duration
	readDeadline time.Duration
	writeTimeout time.Duration
}

func (c *wsConnector) withDefaults() {
	if c.httpClient == nil {
		c.httpClient = http.DefaultClient
	}
	if c.gateway == "" {
		c.gateway = defaultWSGateway
	}
	if c.dialer == nil {
		c.dialer = gorillaWSDialer{}
	}
	if c.open == nil {
		c.open = func(ctx context.Context) (string, error) {
			return fetchWSEndpoint(ctx, c.httpClient, appendDefaultScheme("https://"+c.gateway), c.appKey, c.appSecret)
		}
	}
	if c.logger == nil {
		c.logger = slog.Default()
	}
	if c.pingInterval == 0 {
		c.pingInterval = infoflowPingInterval
	}
	if c.readDeadline == 0 {
		c.readDeadline = infoflowReadDeadline
	}
	if c.writeTimeout == 0 {
		c.writeTimeout = infoflowWriteTimeout
	}
}

func appendDefaultScheme(s string) string {
	if s == "" {
		return s
	}
	if len(s) >= 7 && (s[:7] == "http://" || s[:8] == "https://") {
		return s
	}
	return "https://" + s
}

// fetchWSEndpoint performs Phase 1 of the InfoFlow connection: POST the gateway
// endpoint with app_key/app_secret and return the one-shot websocket URL.
func fetchWSEndpoint(ctx context.Context, httpClient *http.Client, gateway, appKey, appSecret string) (string, error) {
	payload, err := json.Marshal(map[string]string{"app_key": appKey, "app_secret": appSecret})
	if err != nil {
		return "", fmt.Errorf("infoflow: marshal ws endpoint request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, gateway+"/open/ws/endpoint", bytes.NewReader(payload))
	if err != nil {
		return "", fmt.Errorf("infoflow: build ws endpoint request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("infoflow: ws endpoint request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("infoflow: read ws endpoint response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("infoflow: ws endpoint http %d", resp.StatusCode)
	}
	var parsed endpointResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return "", fmt.Errorf("infoflow: decode ws endpoint response: %w", err)
	}
	if parsed.Code != 0 || parsed.Data.URL == "" {
		return "", fmt.Errorf("infoflow: ws endpoint missing url (code=%d msg=%q)", parsed.Code, parsed.Msg)
	}
	return parsed.Data.URL, nil
}

func (c *wsConnector) run(ctx context.Context) error {
	c.withDefaults()
	url, err := c.open(ctx)
	if err != nil {
		return err
	}
	conn, err := c.dialer.dial(ctx, url)
	if err != nil {
		// Do not wrap the one-shot URL into error text; it may contain the
		// single-use connection_id.
		return fmt.Errorf("infoflow: websocket dial failed (%T)", err)
	}

	runCtx, cancel := context.WithCancel(ctx)
	var closeOnce sync.Once
	closeConn := func() { closeOnce.Do(func() { _ = conn.Close() }) }
	done := make(chan struct{})
	go func() {
		select {
		case <-runCtx.Done():
			closeConn()
		case <-done:
		}
	}()

	var writeMu sync.Mutex
	write := func(frame Frame) error {
		b, err := marshalFrame(frame)
		if err != nil {
			return err
		}
		writeMu.Lock()
		defer writeMu.Unlock()
		_ = conn.SetWriteDeadline(time.Now().Add(c.writeTimeout))
		return conn.WriteMessage(websocket.BinaryMessage, b)
	}

	pingDone := make(chan struct{})
	go func() {
		defer close(pingDone)
		ticker := time.NewTicker(c.pingInterval)
		defer ticker.Stop()
		for {
			select {
			case <-runCtx.Done():
				return
			case <-ticker.C:
				if err := write(pingFrame(1)); err != nil {
					closeConn()
					return
				}
			}
		}
	}()

	defer func() {
		cancel()
		closeConn()
		close(done)
		<-pingDone
	}()

	for {
		if err := conn.SetReadDeadline(time.Now().Add(c.readDeadline)); err != nil {
			return fmt.Errorf("infoflow: set read deadline: %w", err)
		}
		msgType, data, err := conn.ReadMessage()
		if err != nil {
			if runCtx.Err() != nil {
				return nil
			}
			return fmt.Errorf("infoflow: websocket read: %w", err)
		}
		if msgType != websocket.BinaryMessage {
			continue
		}
		frame, err := unmarshalFrame(data)
		if err != nil {
			c.logger.WarnContext(runCtx, "infoflow: malformed proto frame", "error", err)
			continue
		}
		if frame.Method == FrameControl && headerValue(frame.Headers, "type") == "ping" {
			if err := write(pongFrame(frame.SeqID)); err != nil {
				if runCtx.Err() != nil {
					return nil
				}
				return fmt.Errorf("infoflow: write pong: %w", err)
			}
		}
		if frame.Method == FrameData {
			if err := c.dispatchData(runCtx, frame, write); err != nil && runCtx.Err() == nil {
				c.logger.WarnContext(runCtx, "infoflow: frame handler error", "error", err)
			}
		}
	}
}

func (c *wsConnector) dispatchData(ctx context.Context, frame Frame, write func(Frame) error) error {
	if c.onFrame != nil {
		if err := c.onFrame(ctx, frame); err != nil {
			return err
		}
	}
	return write(eventACKFrame(frame))
}

func pingFrame(seq uint64) Frame {
	return Frame{SeqID: seq, Method: FrameControl, Headers: []Header{{Key: "type", Value: "ping"}}, Payload: []byte("{}")}
}

func pongFrame(seq uint64) Frame {
	return Frame{SeqID: seq, Method: FrameControl, Headers: []Header{{Key: "type", Value: "pong"}}, Payload: []byte("{}")}
}

func eventACKFrame(original Frame) Frame {
	payload, _ := json.Marshal(map[string]any{"ackSeqId": original.SeqID, "code": 0, "message": "ok"})
	return Frame{
		SeqID:   1,
		LogID:   original.LogID,
		Method:  FrameData,
		Headers: []Header{{Key: "type", Value: "event_ack"}},
		Payload: payload,
	}
}

// parseFramePayload decodes a JSON payload. It returns an error only for an
// empty payload; malformed JSON is handled by the inbound normalizer as an
// unsupported message.
func parseFramePayload(frame Frame) (json.RawMessage, error) {
	if len(frame.Payload) == 0 {
		return nil, errors.New("infoflow: frame has no payload")
	}
	copyPayload := append(json.RawMessage(nil), frame.Payload...)
	var probe any
	if err := json.Unmarshal(copyPayload, &probe); err != nil {
		return nil, err
	}
	return copyPayload, nil
}
