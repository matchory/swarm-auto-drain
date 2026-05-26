package hostclient

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// shortSocketPath returns a Unix-socket path short enough for macOS's
// 104-char sun_path limit. Matches the helper in internal/api/server_test.go.
//
//nolint:usetesting // sun_path 104-char limit precludes t.TempDir on macOS
func shortSocketPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "hc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, fmt.Sprintf("s%d", os.Getpid()))
}

// serve spins up an httptest.Server backed by a Unix listener at a temp path
// and returns a Client pointed at it. The mux is the caller's; the t.Cleanup
// hook stops the server when the test ends.
func serve(t *testing.T, handler http.Handler) *Client {
	t.Helper()
	path := shortSocketPath(t)
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(handler)
	srv.Listener = ln
	srv.Start()
	t.Cleanup(srv.Close)
	return New(path)
}

func TestClient_GetHealthz(t *testing.T) {
	t.Run("ok", func(t *testing.T) {
		c := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/healthz" || r.Method != http.MethodGet {
				t.Errorf("got %s %s", r.Method, r.URL.Path)
			}
			w.WriteHeader(http.StatusOK)
		}))
		if err := c.GetHealthz(context.Background()); err != nil {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("non-2xx", func(t *testing.T) {
		c := serve(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "down", http.StatusServiceUnavailable)
		}))
		err := c.GetHealthz(context.Background())
		if err == nil || !strings.Contains(err.Error(), "503") {
			t.Errorf("err = %v, want non-2xx", err)
		}
	})
}

func TestClient_GetRole(t *testing.T) {
	t.Run("ok includes version", func(t *testing.T) {
		c := serve(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(Role{Role: "manager", Leader: "n1", Version: "1.2.3"})
		}))
		r, err := c.GetRole(context.Background())
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if r.Role != "manager" || r.Leader != "n1" || r.Version != "1.2.3" {
			t.Errorf("got %+v", r)
		}
	})
	t.Run("old daemon omits version", func(t *testing.T) {
		c := serve(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"role":"worker","leader":"m1"}`))
		}))
		r, err := c.GetRole(context.Background())
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if r.Version != "" {
			t.Errorf("Version = %q, want empty for old daemon", r.Version)
		}
	})
	t.Run("non-2xx", func(t *testing.T) {
		c := serve(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "oops", http.StatusInternalServerError)
		}))
		if _, err := c.GetRole(context.Background()); err == nil {
			t.Error("err = nil, want non-2xx error")
		}
	})
	t.Run("bad json", func(t *testing.T) {
		c := serve(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("not json"))
		}))
		if _, err := c.GetRole(context.Background()); err == nil {
			t.Error("err = nil, want decode error")
		}
	})
}

func TestClient_AcquireLock(t *testing.T) {
	tests := []struct {
		name       string
		respStatus int
		respBody   string
		wantHolder string
		wantTTL    int
		wantStatus int
		wantErr    bool
	}{
		{name: "201 acquired", respStatus: 201, respBody: `{"holder":"a","acquired_at":1748131200,"ttl_seconds":60}`, wantHolder: "a", wantTTL: 60, wantStatus: 201},
		{name: "410 stole", respStatus: 410, respBody: `{"holder":"b","ttl_seconds":30}`, wantHolder: "b", wantTTL: 30, wantStatus: 410},
		{name: "409 held by other", respStatus: 409, respBody: `{"holder":"c"}`, wantHolder: "c", wantStatus: 409},
		{name: "500 unexpected", respStatus: 500, respBody: "boom", wantErr: true},
		{name: "201 with empty body", respStatus: 201, respBody: "", wantStatus: 201},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != "/lock" {
					t.Errorf("got %s %s", r.Method, r.URL.Path)
				}
				w.WriteHeader(tc.respStatus)
				_, _ = w.Write([]byte(tc.respBody))
			}))
			res, err := c.AcquireLock(context.Background())
			if tc.wantErr {
				if err == nil {
					t.Fatal("err = nil, want error")
				}
				return
			}
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			if res.Status != tc.wantStatus {
				t.Errorf("Status = %d, want %d", res.Status, tc.wantStatus)
			}
			if res.Lock.Holder != tc.wantHolder {
				t.Errorf("Lock.Holder = %q, want %q", res.Lock.Holder, tc.wantHolder)
			}
			if res.Lock.TTLSeconds != tc.wantTTL {
				t.Errorf("Lock.TTLSeconds = %d, want %d", res.Lock.TTLSeconds, tc.wantTTL)
			}
		})
	}
}

func TestClient_ReleaseLock(t *testing.T) {
	tests := []struct {
		name       string
		respStatus int
		wantStatus int
		wantErr    bool
	}{
		{name: "204", respStatus: 204, wantStatus: 204},
		{name: "403", respStatus: 403, wantStatus: 403},
		{name: "404", respStatus: 404, wantStatus: 404},
		{name: "500", respStatus: 500, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodDelete {
					t.Errorf("got method %s", r.Method)
				}
				w.WriteHeader(tc.respStatus)
			}))
			status, err := c.ReleaseLock(context.Background())
			if tc.wantErr {
				if err == nil {
					t.Fatal("err = nil, want error")
				}
				return
			}
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			if status != tc.wantStatus {
				t.Errorf("status = %d, want %d", status, tc.wantStatus)
			}
		})
	}
}

func TestClient_Drain(t *testing.T) {
	tests := []struct {
		name       string
		respStatus int
		respBody   string
	}{
		{name: "200", respStatus: 200, respBody: `{"tasks_remaining":0,"elapsed_seconds":12}`},
		{name: "408", respStatus: 408, respBody: `{"tasks_remaining":3,"elapsed_seconds":300}`},
		{name: "503", respStatus: 503, respBody: `{"error":"quorum"}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := serve(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.respStatus)
				_, _ = w.Write([]byte(tc.respBody))
			}))
			status, body, err := c.Drain(context.Background())
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			if status != tc.respStatus {
				t.Errorf("status = %d, want %d", status, tc.respStatus)
			}
			if string(body) != tc.respBody {
				t.Errorf("body = %q, want %q", body, tc.respBody)
			}
		})
	}
}

func TestClient_Activate(t *testing.T) {
	t.Run("ok", func(t *testing.T) {
		c := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost || r.URL.Path != "/active" {
				t.Errorf("got %s %s", r.Method, r.URL.Path)
			}
			w.WriteHeader(http.StatusOK)
		}))
		if err := c.Activate(context.Background()); err != nil {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("non-2xx", func(t *testing.T) {
		c := serve(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "oops", http.StatusInternalServerError)
		}))
		if err := c.Activate(context.Background()); err == nil {
			t.Error("err = nil, want non-2xx error")
		}
	})
}

func TestClient_DialError(t *testing.T) {
	// Pointing at a nonexistent socket exercises the transport error path —
	// the do() helper must wrap with method/path/socket context.
	c := New(filepath.Join(t.TempDir(), "missing.sock"))
	err := c.GetHealthz(context.Background())
	if err == nil {
		t.Fatal("err = nil, want dial error")
	}
	if !strings.Contains(err.Error(), "GET /healthz") {
		t.Errorf("err = %q, want method/path in message", err.Error())
	}
}

func TestClient_ContextDeadline(t *testing.T) {
	// Handler blocks forever; the caller's context cancellation must unblock
	// the request rather than hanging until the default 30s client timeout.
	c := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	err := c.GetHealthz(ctx)
	if err == nil {
		t.Fatal("err = nil, want ctx deadline error")
	}
}

func TestClient_CustomHTTPClient(t *testing.T) {
	// When the caller supplies HTTPClient, the Client must use it as-is.
	// We assert by handing in a client with a transport that always errors,
	// then confirming the error is surfaced.
	c := New("/dev/null")
	c.HTTPClient = &http.Client{
		Transport: errTransport{},
	}
	if err := c.GetHealthz(context.Background()); err == nil {
		t.Fatal("err = nil, want injected transport error")
	}
}

type errTransport struct{}

func (errTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, fmt.Errorf("injected")
}
