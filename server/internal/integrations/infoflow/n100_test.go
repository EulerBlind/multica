package infoflow

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/integrations/channel"
)

func n100Channel(enricher *quoteEnricher, logger *slog.Logger, handler channel.InboundHandler) *infoflowChannel {
	return &infoflowChannel{
		appID:         "14534",
		appKey:        "app-key",
		appSecret:     "app-secret",
		botUserID:     "4100104388",
		quoteEnricher: enricher,
		logger:        logger,
		handler:       handler,
	}
}

// TestQuoteOffMatchesLegacyBridge is the DEC35 safety-switch proof: off must
// remove quote parsing, quote-addressing and the lookup, leaving the same text
// the legacy bridge would have produced. It also proves no getSingleMsg call.
func TestQuoteOffMatchesLegacyBridge(t *testing.T) {
	t.Setenv(quoteOriginEnv, "off")
	if quoteOriginEnabled() {
		t.Fatal("quoteOriginEnabled must be false for off")
	}

	gw := &quoteGateway{message: func(_ int, w http.ResponseWriter, _ *http.Request) {
		t.Error("off must never call getSingleMsg")
	}}
	srv := gw.start(t)

	rawQuote := json.RawMessage(`{"groupid":"G1","message":{"header":{"fromuserid":"U1","toid":"G1","messageid":"M9"},"reply":{"messageid":"M8","preview":"SECRET-PREVIEW"},"body":[{"type":"AT","atuserids":["4100104388"]},{"type":"TEXT","content":"own body"}]}}`)
	rawLegacy := json.RawMessage(`{"groupid":"G1","message":{"header":{"fromuserid":"U1","toid":"G1","messageid":"M9"},"body":[{"type":"AT","atuserids":["4100104388"]},{"type":"TEXT","content":"own body"}]}}`)

	var quoteMsg, legacyMsg channel.InboundMessage
	quoteCh := n100Channel(newQuoteEnricher(NewClient(srv.Client(), srv.URL), discardLogger()), discardLogger(), func(_ context.Context, msg channel.InboundMessage) error {
		quoteMsg = msg
		return nil
	})
	quoteCh.quoteDisabled = true
	legacyCh := n100Channel(nil, discardLogger(), func(_ context.Context, msg channel.InboundMessage) error {
		legacyMsg = msg
		return nil
	})
	legacyCh.quoteDisabled = true

	if err := quoteCh.handleFrame(context.Background(), Frame{Method: FrameData, Payload: rawQuote}); err != nil {
		t.Fatalf("quote frame: %v", err)
	}
	if err := legacyCh.handleFrame(context.Background(), Frame{Method: FrameData, Payload: rawLegacy}); err != nil {
		t.Fatalf("legacy frame: %v", err)
	}
	if token, message := gw.calls(); token != 0 || message != 0 {
		t.Fatalf("off must make zero gateway calls, got token=%d message=%d", token, message)
	}
	if quoteMsg.Text != "own body" || quoteMsg.CommandText != "own body" {
		t.Fatalf("off text = %q command=%q, want legacy body", quoteMsg.Text, quoteMsg.CommandText)
	}
	if quoteMsg.HasSelectedContext || quoteMsg.ReplyTo != nil {
		t.Fatalf("off must not expose quoted metadata: %+v", quoteMsg)
	}
	if quoteMsg.Text != legacyMsg.Text || quoteMsg.CommandText != legacyMsg.CommandText ||
		quoteMsg.HasSelectedContext != legacyMsg.HasSelectedContext ||
		(quoteMsg.ReplyTo == nil) != (legacyMsg.ReplyTo == nil) ||
		quoteMsg.AddressedToBot != legacyMsg.AddressedToBot {
		t.Fatalf("off differs from legacy: quote=%+v legacy=%+v", quoteMsg, legacyMsg)
	}
}

// TestFactoryQuoteOffDropsInjectedEnricher pins the env-to-channel wiring:
// MULTICA_INFOFLOW_QUOTE_ORIGIN=off must disable an explicitly injected
// enricher too, not only the default one built inside the factory.
func TestFactoryQuoteOffDropsInjectedEnricher(t *testing.T) {
	t.Setenv(quoteOriginEnv, "off")
	factory := newFactory(ChannelDeps{
		Logger:      discardLogger(),
		QuoteEnrich: &quoteEnricher{},
	})
	secret := base64.StdEncoding.EncodeToString([]byte("secret"))
	cfg := channel.Config{Raw: json.RawMessage(`{"app_id":"a","app_key":"k","app_secret_encrypted":"` + secret + `"}`)}
	built, err := factory(cfg)
	if err != nil {
		t.Fatalf("build channel: %v", err)
	}
	ch, ok := built.(*infoflowChannel)
	if !ok {
		t.Fatalf("unexpected channel type %T", built)
	}
	if !ch.quoteDisabled || ch.quoteEnricher != nil {
		t.Fatalf("off must disable quotes and the injected enricher: %+v", ch)
	}
}

// TestQuoteFailuresStillDeliver covers the A31 three failure classes and the
// missing-id sample: every one must reach the handler with the own body intact.
func TestQuoteFailuresStillDeliver(t *testing.T) {
	cases := []struct {
		name       string
		raw        string
		message    func(int, http.ResponseWriter, *http.Request)
		timeout    time.Duration
		wantQuote  bool
		wantMarker string
	}{
		{
			name: "items null",
			raw:  `{"groupid":"G1","message":{"header":{"fromuserid":"U1","toid":"G1","messageid":"M-NF"},"reply":{"messageid":"M8","preview":"PREVIEW-NF"},"body":[{"type":"AT","atuserids":["4100104388"]},{"type":"TEXT","content":"OWN-NF"}]}}`,
			message: func(_ int, w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(`{"code":"ok","data":{"items":null}}`))
			},
			wantQuote:  true,
			wantMarker: quoteTagTruncated,
		},
		{
			name: "timeout",
			raw:  `{"groupid":"G1","message":{"header":{"fromuserid":"U1","toid":"G1","messageid":"M-TO"},"reply":{"messageid":"M8","preview":"PREVIEW-TO"},"body":[{"type":"AT","atuserids":["4100104388"]},{"type":"TEXT","content":"OWN-TO"}]}}`,
			message: func(_ int, w http.ResponseWriter, _ *http.Request) {
				time.Sleep(80 * time.Millisecond)
				_, _ = w.Write([]byte(`{"code":"ok","data":{"content":"too late"}}`))
			},
			timeout:    5 * time.Millisecond,
			wantQuote:  true,
			wantMarker: quoteTagPreview,
		},
		{
			name:       "malformed quote object",
			raw:        `{"groupid":"G1","message":{"header":{"fromuserid":"U1","toid":"G1","messageid":"M-BAD"},"reply":{"messageid":{"bad":1},"preview":{"bad":true},"body":"not-an-array"},"body":[{"type":"AT","atuserids":["4100104388"]},{"type":"TEXT","content":"OWN-BAD"}]}}`,
			wantQuote:  false,
			wantMarker: "OWN-BAD",
		},
		{
			name: "id without preview resolves",
			raw:  `{"groupid":"G1","message":{"header":{"fromuserid":"U1","toid":"G1","messageid":"M-FULL"},"reply":{"messageid":"M8"},"body":[{"type":"AT","atuserids":["4100104388"]},{"type":"TEXT","content":"OWN-FULL"}]}}`,
			message: func(_ int, w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(`{"code":"ok","data":{"content":"FULL-TEXT"}}`))
			},
			wantQuote:  true,
			wantMarker: quoteTagFull,
		},
		{
			name:       "missing quote id",
			raw:        `{"groupid":"G1","message":{"header":{"fromuserid":"U1","toid":"G1","messageid":"M-ID"},"reply":{"preview":"PREVIEW-ID"},"body":[{"type":"AT","atuserids":["4100104388"]},{"type":"TEXT","content":"OWN-ID"}]}}`,
			wantQuote:  true,
			wantMarker: quoteTagPreview,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var enricher *quoteEnricher
			if tc.message != nil {
				gw := &quoteGateway{message: tc.message}
				srv := gw.start(t)
				enricher = newQuoteEnricher(NewClient(srv.Client(), srv.URL), discardLogger())
				if tc.timeout > 0 {
					enricher.timeout = tc.timeout
				}
			}
			var got channel.InboundMessage
			ch := n100Channel(enricher, discardLogger(), func(_ context.Context, msg channel.InboundMessage) error {
				got = msg
				return nil
			})
			if err := ch.handleFrame(context.Background(), Frame{Method: FrameData, Payload: json.RawMessage(tc.raw)}); err != nil {
				t.Fatalf("handleFrame: %v", err)
			}
			if !strings.Contains(got.Text, tc.wantMarker) {
				t.Fatalf("delivered text missing %q: %q", tc.wantMarker, got.Text)
			}
			if got.HasSelectedContext != tc.wantQuote {
				t.Fatalf("HasSelectedContext=%v, want %v (text=%q)", got.HasSelectedContext, tc.wantQuote, got.Text)
			}
			if got.Source.ChatID != "G1" || !got.AddressedToBot {
				t.Fatalf("message must remain routable: %+v", got)
			}
		})
	}
}

// TestMissingSenderStillDeliveredAndLogged is the DEC34 reversal of DEC22.
func TestMissingSenderStillDeliveredAndLogged(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	var got channel.InboundMessage
	ch := n100Channel(nil, logger, func(_ context.Context, msg channel.InboundMessage) error {
		got = msg
		return nil
	})
	raw := json.RawMessage(`{"groupid":"G1","message":{"header":{"totype":"GROUP","messageid":"M-MISS"},"body":[{"type":"AT","atuserids":["4100104388"]},{"type":"TEXT","content":"OWN-MISS"}]}}`)
	if err := ch.handleFrame(context.Background(), Frame{Method: FrameData, Payload: raw}); err != nil {
		t.Fatalf("handleFrame: %v", err)
	}
	if got.Text != "OWN-MISS" || got.Source.SenderID != "" || got.Source.ChatID != "G1" {
		t.Fatalf("missing sender must degrade, got %+v", got)
	}
	if !strings.Contains(buf.String(), `"msg":"infoflow: inbound degraded"`) ||
		!strings.Contains(buf.String(), `"reason":"missing_sender"`) ||
		!strings.Contains(buf.String(), `"attempts":0`) {
		t.Fatalf("missing degradation log fields: %s", buf.String())
	}
}

// TestQuoteDegradationLogFields pins A34: reason enum, sha256_8 and attempts,
// with no content/preview/id value in the line.
func TestQuoteDegradationLogFields(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	gw := &quoteGateway{message: func(_ int, w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"code":"ok","data":{"items":null}}`))
	}}
	srv := gw.start(t)
	ch := n100Channel(newQuoteEnricher(NewClient(srv.Client(), srv.URL), discardLogger()), logger, func(context.Context, channel.InboundMessage) error {
		return nil
	})
	raw := json.RawMessage(`{"groupid":"G1","message":{"header":{"fromuserid":"U1","toid":"G1","messageid":"M-LOG"},"reply":{"messageid":"M8","preview":"SECRET-PREVIEW"},"body":[{"type":"AT","atuserids":["4100104388"]},{"type":"TEXT","content":"OWN-LOG"}]}}`)
	if err := ch.handleFrame(context.Background(), Frame{Method: FrameData, Payload: raw}); err != nil {
		t.Fatalf("handleFrame: %v", err)
	}
	var found bool
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		var entry map[string]any
		if json.Unmarshal([]byte(line), &entry) != nil {
			continue
		}
		if entry["msg"] != "infoflow: quote origin resolved" {
			continue
		}
		found = true
		if entry["degraded"] != true || entry["reason"] != string(quoteStateNotFound) {
			t.Fatalf("degradation fields = %v", entry)
		}
		if entry["attempts"] != float64(1) || entry["sha256_8"] != quoteIDFingerprint("M8") {
			t.Fatalf("hash/attempts = %v", entry)
		}
		if strings.Contains(line, "SECRET-PREVIEW") || strings.Contains(line, "M8") {
			t.Fatalf("log leaked quote content/id: %s", line)
		}
	}
	if !found {
		t.Fatalf("no quote degradation log: %s", buf.String())
	}
}

// TestMalformedQuoteLogsUnavailable covers the degraded path where a quote is
// present but yields no selected context: it must be logged as unavailable,
// not silently omitted.
func TestMalformedQuoteLogsUnavailable(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	var got channel.InboundMessage
	ch := n100Channel(nil, logger, func(_ context.Context, msg channel.InboundMessage) error {
		got = msg
		return nil
	})
	raw := json.RawMessage(`{"groupid":"G1","message":{"header":{"fromuserid":"U1","toid":"G1","messageid":"M-MAL"},"reply":{"messageid":{"bad":1},"preview":{"bad":true},"body":"not-an-array"},"body":[{"type":"AT","atuserids":["4100104388"]},{"type":"TEXT","content":"OWN-MAL"}]}}`)
	if err := ch.handleFrame(context.Background(), Frame{Method: FrameData, Payload: raw}); err != nil {
		t.Fatalf("handleFrame: %v", err)
	}
	if got.Text != "OWN-MAL" {
		t.Fatalf("body lost: %+v", got)
	}
	if !strings.Contains(buf.String(), `"msg":"infoflow: quote degraded"`) ||
		!strings.Contains(buf.String(), `"reason":"quote_unavailable"`) ||
		!strings.Contains(buf.String(), `"attempts":0`) {
		t.Fatalf("unavailable log fields missing: %s", buf.String())
	}
}

// TestHandleFrameQueuedAcksBeforeDelivery proves the read loop does not wait
// for the handler (and therefore not for quote enrich): ACK latency stays in
// the baseline order even when delivery is slow.
func TestHandleFrameQueuedAcksBeforeDelivery(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	done := make(chan struct{})
	ch := n100Channel(nil, discardLogger(), func(context.Context, channel.InboundMessage) error {
		started <- struct{}{}
		<-release
		close(done)
		return nil
	})
	ch.workers = newQuoteWorkers(ctx, 1, discardLogger())
	defer ch.workers.stop()

	raw := json.RawMessage(`{"groupid":"G1","message":{"header":{"fromuserid":"U1","toid":"G1","messageid":"M-ACK"},"reply":{"messageid":"M8","preview":"PREVIEW"},"body":[{"type":"AT","atuserids":["4100104388"]},{"type":"TEXT","content":"OWN-ACK"}]}}`)
	start := time.Now()
	if err := ch.handleFrameQueued(ctx, Frame{Method: FrameData, Payload: raw}); err != nil {
		t.Fatalf("handleFrameQueued: %v", err)
	}
	elapsed := time.Since(start)
	t.Logf("handleFrameQueued ACK latency: %s", elapsed)
	if elapsed > 100*time.Millisecond {
		t.Fatalf("ACK waited for delivery: %s", elapsed)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("worker never started")
	}
	close(release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("worker did not finish")
	}
}
