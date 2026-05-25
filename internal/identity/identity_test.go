package identity

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestMiddleware_LoopbackUsesNodeHostname(t *testing.T) {
	mw := Middleware{NodeHostname: "test-host"}

	var got string
	h := mw.Wrap(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got = FromContext(r.Context())
	}))

	r := httptest.NewRequest("POST", "/lock", nil)
	r.RemoteAddr = "127.0.0.1:54321"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if got != "test-host" {
		t.Errorf("identity = %q, want test-host", got)
	}
	if w.Code != 200 {
		t.Errorf("status = %d", w.Code)
	}
}

func TestMiddleware_LoopbackIPv6(t *testing.T) {
	mw := Middleware{NodeHostname: "test-host"}
	var got string
	h := mw.Wrap(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got = FromContext(r.Context())
	}))
	r := httptest.NewRequest("GET", "/role", nil)
	r.RemoteAddr = "[::1]:54321"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if got != "test-host" {
		t.Errorf("identity = %q", got)
	}
}

func TestMiddleware_OverlayHeader(t *testing.T) {
	mw := Middleware{NodeHostname: "manager-1"}
	var got string
	h := mw.Wrap(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got = FromContext(r.Context())
	}))
	r := httptest.NewRequest("POST", "/drain", nil)
	r.RemoteAddr = "10.0.0.42:34567"
	r.Header.Set("X-Origin-Node", "worker-3")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if got != "worker-3" {
		t.Errorf("identity = %q", got)
	}
}

func TestMiddleware_OverlayWithoutHeader(t *testing.T) {
	mw := Middleware{NodeHostname: "manager-1"}
	h := mw.Wrap(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("handler should not be called when identity is unresolvable")
	}))
	r := httptest.NewRequest("POST", "/drain", nil)
	r.RemoteAddr = "10.0.0.42:34567"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestMiddleware_PublicEndpointsBypass(t *testing.T) {
	mw := Middleware{NodeHostname: "manager-1", PublicPaths: []string{"/healthz", "/role"}}
	h := mw.Wrap(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
	}))
	r := httptest.NewRequest("GET", "/healthz", nil)
	r.RemoteAddr = "10.0.0.42:34567"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Errorf("status = %d, want 200", w.Code)
	}
	id := FromContext(context.Background())
	if id != "" {
		t.Errorf("FromContext on bare context = %q, want empty", id)
	}
}
