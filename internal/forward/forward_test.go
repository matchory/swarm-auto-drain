package forward

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestForward_AddsOriginNodeHeader(t *testing.T) {
	var gotHeader string
	var gotBody string
	mgr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeader = r.Header.Get("X-Origin-Node")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer mgr.Close()

	f := &Forwarder{
		OriginNode:     "node-a",
		FindManagerURL: func() (string, error) { return mgr.URL, nil },
		Client:         &http.Client{Timeout: time.Second},
	}

	req := httptest.NewRequest("POST", "/lock", strings.NewReader(`{"x":1}`))
	rr := httptest.NewRecorder()
	f.ServeHTTP(rr, req)

	if rr.Code != http.StatusCreated {
		t.Errorf("status = %d", rr.Code)
	}
	if gotHeader != "node-a" {
		t.Errorf("X-Origin-Node = %q", gotHeader)
	}
	if gotBody != `{"x":1}` {
		t.Errorf("body = %q", gotBody)
	}
}

func TestForward_NoManagerFound(t *testing.T) {
	f := &Forwarder{
		OriginNode:     "node-a",
		FindManagerURL: func() (string, error) { return "", errNoManagerForTest },
		Client:         &http.Client{Timeout: time.Second},
	}
	req := httptest.NewRequest("GET", "/lock", nil)
	rr := httptest.NewRecorder()
	f.ServeHTTP(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rr.Code)
	}
}

func TestForward_ManagerUnreachableInvalidatesCache(t *testing.T) {
	var calls int32
	mgr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`ok`))
	}))
	defer mgr.Close()

	f := &Forwarder{
		OriginNode: "node-a",
		FindManagerURL: func() (string, error) {
			n := atomic.AddInt32(&calls, 1)
			if n == 1 {
				return "http://127.0.0.1:1", nil // unreachable
			}
			return mgr.URL, nil
		},
		Client:  &http.Client{Timeout: 200 * time.Millisecond},
		Retries: 2,
	}
	req := httptest.NewRequest("GET", "/healthz", nil)
	rr := httptest.NewRecorder()
	f.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 after retry", rr.Code)
	}
	if atomic.LoadInt32(&calls) != 2 {
		t.Errorf("FindManagerURL calls = %d, want 2", calls)
	}
}

var errNoManagerForTest = errString("no manager available")

type errString string

func (e errString) Error() string { return string(e) }
