package infoflow

import (
	"context"
	"net/http"
	"net/http/httptest"
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
