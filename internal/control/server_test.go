package control

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestNewServerSetsReadHeaderTimeout(t *testing.T) {
	srv := NewServer(Options{})

	if srv.server.ReadHeaderTimeout != 5*time.Second {
		t.Fatalf("ReadHeaderTimeout = %v, want 5s", srv.server.ReadHeaderTimeout)
	}
}

func TestAllowMutationRejectsCrossOrigin(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:17990/switch", nil)
	req.Host = "127.0.0.1:17990"
	req.Header.Set("Origin", "http://example.test")
	w := httptest.NewRecorder()

	if allowMutation(w, req) {
		t.Fatal("cross-origin request was allowed")
	}
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusForbidden)
	}
}

func TestAllowMutationAllowsSameOrigin(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:17990/switch", nil)
	req.Host = "127.0.0.1:17990"
	req.Header.Set("Origin", "http://127.0.0.1:17990")
	w := httptest.NewRecorder()

	if !allowMutation(w, req) {
		t.Fatal("same-origin request was rejected")
	}
}
