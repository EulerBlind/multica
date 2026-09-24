package infoflow

import (
	"encoding/json"
	"testing"
)

func TestFrameRoundTrip(t *testing.T) {
	payload, err := json.Marshal(map[string]any{"hello": "世界"})
	if err != nil {
		t.Fatal(err)
	}
	in := Frame{
		SeqID:   42,
		LogID:   "log-1",
		Service: 1,
		Method:  FrameData,
		Headers: []Header{{Key: "type", Value: "event_ack"}},
		Payload: payload,
	}
	buf, err := marshalFrame(in)
	if err != nil {
		t.Fatal(err)
	}
	out, err := unmarshalFrame(buf)
	if err != nil {
		t.Fatal(err)
	}
	if out.SeqID != in.SeqID || out.LogID != in.LogID || out.Service != in.Service || out.Method != in.Method {
		t.Fatalf("round trip mismatch: %+v", out)
	}
	if len(out.Headers) != 1 || out.Headers[0].Key != "type" || out.Headers[0].Value != "event_ack" {
		t.Fatalf("headers mismatch: %+v", out.Headers)
	}
	var got map[string]any
	if err := json.Unmarshal(out.Payload, &got); err != nil {
		t.Fatal(err)
	}
	if got["hello"] != "世界" {
		t.Fatalf("payload mismatch: %v", got)
	}
}

func TestPingAndPongFrames(t *testing.T) {
	ping := pingFrame(7)
	buf, err := marshalFrame(ping)
	if err != nil {
		t.Fatal(err)
	}
	got, err := unmarshalFrame(buf)
	if err != nil {
		t.Fatal(err)
	}
	if got.Method != FrameControl || headerValue(got.Headers, "type") != "ping" {
		t.Fatalf("unexpected ping frame: %+v", got)
	}
	pong := pongFrame(7)
	if pong.Method != FrameControl || headerValue(pong.Headers, "type") != "pong" {
		t.Fatalf("unexpected pong frame: %+v", pong)
	}
}
