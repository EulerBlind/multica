package infoflow

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// memberListOK mirrors the LIVE gateway shape: the robot list is nested one
// level deeper (data.data.agentInfoList) and agentId/imId arrive as JSON
// numbers. Reading only data.agentInfoList silently yielded zero tokens.
const memberListOK = `{"code":"ok","data":{"data":{"agentInfoList":[
  {"agentId":14534,"imId":4100104388,"name":"Multica"},
  {"agentId":99999,"imId":1234567890,"name":"other bot"}
],"errcode":0,"errmsg":"ok"}}}`

// memberListFlatOK is the shallower alternate shape the parser also accepts.
const memberListFlatOK = `{"code":"ok","data":{"agentInfoList":[
  {"agentId":"14534","imId":"4100104388","name":"Multica"}
]}}`

func TestGroupIdentityResolverExtractsOwnTokens(t *testing.T) {
	var memberPath, authHeader string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch req.URL.Path {
		case "/auth/app_access_token":
			_, _ = w.Write([]byte(`{"code":"ok","data":{"app_access_token":"tok"}}`))
		case pathGroupMemberList:
			memberPath = req.URL.Path
			authHeader = req.Header.Get("Authorization")
			_, _ = w.Write([]byte(memberListOK))
		default:
			http.NotFound(w, req)
		}
	}))
	defer srv.Close()
	resolver := newGroupIdentityResolver(NewClient(srv.Client(), srv.URL), "key", "secret", "14534")

	tokens, live := resolver.lookup(context.Background(), "987654321")
	if !live {
		t.Fatal("first lookup should be live")
	}
	if memberPath != pathGroupMemberList {
		t.Fatalf("member list path = %q", memberPath)
	}
	if authHeader == "" || authHeader == "Bearer-" {
		t.Fatalf("member list call must carry an access token, got %q", authHeader)
	}
	if !containsToken(tokens, "4100104388") || !containsToken(tokens, "Multica") {
		t.Fatalf("tokens = %v, want the bot's imId and name", tokens)
	}
	if containsToken(tokens, "1234567890") || containsToken(tokens, "other bot") {
		t.Fatalf("tokens = %v must not contain another agent's identity", tokens)
	}
}

func TestGroupIdentityResolverCachesAndFailsClosed(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if req.URL.Path == "/auth/app_access_token" {
			_, _ = w.Write([]byte(`{"code":"ok","data":{"app_access_token":"tok"}}`))
			return
		}
		atomic.AddInt32(&calls, 1)
		_, _ = w.Write([]byte(memberListOK))
	}))
	defer srv.Close()
	resolver := newGroupIdentityResolver(NewClient(srv.Client(), srv.URL), "key", "secret", "14534")

	if _, live := resolver.lookup(context.Background(), "g1"); !live {
		t.Fatal("first lookup should be live")
	}
	if tokens, live := resolver.lookup(context.Background(), "g1"); live || len(tokens) == 0 {
		t.Fatalf("second lookup should be cached, live=%v tokens=%v", live, tokens)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("memberList calls = %d, want 1 (cached)", got)
	}
}

func TestGroupIdentityResolverLookupFailureFailsClosed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if req.URL.Path == "/app/access_token" {
			_, _ = w.Write([]byte(`{"code":"ok","data":{"app_access_token":"tok"}}`))
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	resolver := newGroupIdentityResolver(NewClient(srv.Client(), srv.URL), "key", "secret", "14534")

	if tokens, _ := resolver.lookup(context.Background(), "g1"); len(tokens) != 0 {
		t.Fatalf("failed lookup must yield no tokens, got %v", tokens)
	}
	// An empty group id / agent id never hits the network.
	if tokens, _ := resolver.lookup(context.Background(), ""); tokens != nil {
		t.Fatalf("empty group id must yield nil, got %v", tokens)
	}
}

// TestGroupIdentityResolverAcceptsBothEnvelopeShapes pins the parsing contract:
// the live gateway nests the list under data.data, and agentId/imId are JSON
// numbers; the shallower shape must keep working too.
func TestGroupIdentityResolverAcceptsBothEnvelopeShapes(t *testing.T) {
	for name, body := range map[string]string{"nested_live": memberListOK, "flat_legacy": memberListFlatOK} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if req.URL.Path == "/auth/app_access_token" {
					_, _ = w.Write([]byte(`{"code":"ok","data":{"app_access_token":"tok"}}`))
					return
				}
				_, _ = w.Write([]byte(body))
			}))
			defer srv.Close()
			resolver := newGroupIdentityResolver(NewClient(srv.Client(), srv.URL), "key", "secret", "14534")
			tokens, _ := resolver.lookup(context.Background(), "g1")
			if !containsToken(tokens, "4100104388") || !containsToken(tokens, "Multica") {
				t.Fatalf("tokens = %v, want [4100104388 Multica]", tokens)
			}
		})
	}
}

func containsToken(tokens []string, want string) bool {
	for _, token := range tokens {
		if token == want {
			return true
		}
	}
	return false
}
