package infoflow

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

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
