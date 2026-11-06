package infoflow

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestClientDoesNotClassifyLackParamAsPermission(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":"ok","data":{"errcode":40000,"errmsg":"lack param"}}`))
	}))
	defer srv.Close()
	c := NewClient(srv.Client(), srv.URL)
	err := c.postJSON(context.Background(), "/robot/msg/groupmsgsend", "token", map[string]any{}, nil)
	if err == nil {
		t.Fatal("expected error")
	}
	if IsPermissionNotGranted(err) {
		t.Fatalf("40000/lack param must NOT be classified as permission, got %v", err)
	}
}

func TestClientClassifiesAPINotAllowed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":"ok","data":{"errcode":40000,"errmsg":"Api not allowed"}}`))
	}))
	defer srv.Close()
	c := NewClient(srv.Client(), srv.URL)
	err := c.postJSON(context.Background(), "/app/message/send", "token", map[string]any{}, nil)
	if !IsPermissionNotGranted(err) {
		t.Fatalf("expected permission error, got %v", err)
	}
}

// TestClientClassifiesTopLevelErrcode: the SDK's flat failure shape
// {code:"error",errcode,errmsg} must not be decoded as a success payload.
func TestClientClassifiesTopLevelErrcode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":"error","errcode":40070,"errmsg":"boom"}`))
	}))
	defer srv.Close()
	c := NewClient(srv.Client(), srv.URL)
	var out struct {
		Content string `json:"content"`
	}
	err := c.getJSON(context.Background(), "/im/message/getSingleMsg", "token", nil, &out)
	if err == nil {
		t.Fatal("expected error")
	}
	var apiErr *apiError
	if !errors.As(err, &apiErr) || apiErr.Code != 40070 {
		t.Fatalf("expected apiError 40070, got %v", err)
	}
	if out.Content != "" {
		t.Fatalf("error envelope must not decode into out, got %q", out.Content)
	}
}

// TestDecodeBusinessErrorDataCode covers the live getSingleMsg failure envelope:
// data.code/data.message must be classified, while 200 and the legacy
// data.errcode shape keep their existing behaviour (DEC16).
func TestDecodeBusinessErrorDataCode(t *testing.T) {
	for _, tc := range []struct {
		name     string
		raw      string
		wantCode int
		wantNil  bool
	}{
		{name: "live denied", raw: `{"code":"ok","data":{"code":720001,"message":"Can not read other robots's private chat"}}`, wantCode: 720001},
		{name: "live invalid param", raw: `{"code":"ok","data":{"code":"720027","message":"get group member error"}}`, wantCode: 720027},
		{name: "success 200", raw: `{"code":"ok","data":{"code":200,"message":"ok","data":{"items":null}}}`, wantNil: true},
		{name: "success string 200", raw: `{"code":"ok","data":{"code":"200","message":"ok"}}`, wantNil: true},
		{name: "legacy errcode", raw: `{"code":"ok","data":{"errcode":720001,"errmsg":"legacy"}}`, wantCode: 720001},
		{name: "no business code", raw: `{"code":"ok","data":{"content":"legacy payload"}}`, wantNil: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var envelope businessEnvelope
			if err := json.Unmarshal([]byte(tc.raw), &envelope); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			err := decodeBusinessError("/im/message/getSingleMsg", envelope)
			if tc.wantNil {
				if err != nil {
					t.Fatalf("want success, got %v", err)
				}
				return
			}
			var apiErr *apiError
			if !errors.As(err, &apiErr) || apiErr.Code != tc.wantCode {
				t.Fatalf("got %v, want apiError code %d", err, tc.wantCode)
			}
		})
	}
}

// TestQuoteMessageResponseDetail covers both success shapes and treats empty
// items/content as not found.
func TestQuoteMessageResponseDetail(t *testing.T) {
	for _, tc := range []struct {
		name   string
		raw    string
		want   string
		wantOK bool
	}{
		{name: "live items", raw: `{"code":"ok","data":{"code":200,"message":"ok","data":{"items":[{"msgId":"m8","content":"全文"}]}}}`, want: "全文", wantOK: true},
		{name: "empty items", raw: `{"code":"ok","data":{"code":200,"message":"ok","data":{"items":null}}}`, wantOK: false},
		{name: "item without content", raw: `{"code":"ok","data":{"data":{"items":[{"msgId":"m8"}]}}}`, wantOK: false},
		{name: "legacy flat", raw: `{"code":"ok","data":{"msgId":"m8","content":"旧结构"}}`, want: "旧结构", wantOK: true},
		{name: "legacy empty", raw: `{"code":"ok","data":{"msgId":"m8"}}`, wantOK: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var resp quoteMessageResponse
			if err := json.Unmarshal([]byte(tc.raw), &resp); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			detail, ok := resp.detail()
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v (detail=%+v)", ok, tc.wantOK, detail)
			}
			if ok && detail.Content != tc.want {
				t.Fatalf("content = %q, want %q", detail.Content, tc.want)
			}
		})
	}
}

// TestRedactedJSONSkeletonNeverLeaksValues proves the skeleton helper keeps key
// names/types only, so the not-found log cannot expose content or identifiers.
func TestRedactedJSONSkeletonNeverLeaksValues(t *testing.T) {
	raw := []byte(`{"code":"ok","data":{"code":200,"message":"SECRET-MSG","data":{"items":[{"msgId":"SECRET-ID","content":"SECRET-CONTENT","fromId":"SECRET-USER","url":"https://secret.example/x"}]}}}`)
	skeleton := redactedJSONSkeleton(raw)
	for _, secret := range []string{"SECRET", "secret.example", "https://"} {
		if strings.Contains(skeleton, secret) {
			t.Fatalf("skeleton leaked %q: %s", secret, skeleton)
		}
	}
	for _, want := range []string{"code:number", "message:string", "items:[", "content:string", "fromId:string"} {
		if !strings.Contains(skeleton, want) {
			t.Fatalf("skeleton missing %q: %s", want, skeleton)
		}
	}
}
