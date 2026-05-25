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

func TestForward_StripsHopByHopHeaders(t *testing.T) {
	var gotReqTE string
	mgr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotReqTE = r.Header.Get("Transfer-Encoding")
		w.Header().Set("Transfer-Encoding", "chunked")
		w.Header().Set("Connection", "close")
		w.Header().Set("X-Keep", "yes")
		w.WriteHeader(http.StatusOK)
	}))
	defer mgr.Close()

	f := &Forwarder{
		OriginNode:     "n",
		FindManagerURL: func() (string, error) { return mgr.URL, nil },
		Client:         &http.Client{Timeout: time.Second},
	}
	req := httptest.NewRequest("GET", "/x", nil)
	req.Header.Set("Transfer-Encoding", "chunked")
	req.Header.Set("Connection", "close")
	req.Header.Set("X-Keep", "yes")
	rr := httptest.NewRecorder()
	f.ServeHTTP(rr, req)

	if gotReqTE != "" {
		t.Errorf("upstream saw forwarded Transfer-Encoding = %q, want stripped", gotReqTE)
	}
	if rr.Header().Get("Transfer-Encoding") != "" {
		t.Errorf("response Transfer-Encoding = %q, want stripped", rr.Header().Get("Transfer-Encoding"))
	}
	if rr.Header().Get("Connection") != "" {
		t.Errorf("response Connection = %q, want stripped", rr.Header().Get("Connection"))
	}
	if rr.Header().Get("X-Keep") != "yes" {
		t.Errorf("response X-Keep = %q, want propagated", rr.Header().Get("X-Keep"))
	}
}

func TestForward_OnAttemptFailureCalled(t *testing.T) {
	var failures int32
	f := &Forwarder{
		OriginNode:       "n",
		FindManagerURL:   func() (string, error) { return "http://127.0.0.1:1", nil }, // unreachable
		OnAttemptFailure: func() { atomic.AddInt32(&failures, 1) },
		Client:           &http.Client{Timeout: 100 * time.Millisecond},
		Retries:          2,
	}
	req := httptest.NewRequest("GET", "/x", nil)
	rr := httptest.NewRecorder()
	f.ServeHTTP(rr, req)
	if got := atomic.LoadInt32(&failures); got != 3 {
		t.Errorf("OnAttemptFailure calls = %d, want 3 (1+Retries)", got)
	}
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d", rr.Code)
	}
}

var errNoManagerForTest = errString("no manager available")

type errString string

func (e errString) Error() string { return string(e) }
