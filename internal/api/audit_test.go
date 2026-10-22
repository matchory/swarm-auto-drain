package api

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/matchory/swarm-auto-drain/internal/identity"
)

// captureSlog redirects slog to a buffer for the duration of fn, then
// returns every JSON-encoded log record as a parsed map. JSON output
// keeps the assertions structural rather than string-substring fragile.
func captureSlog(t *testing.T, fn func()) []map[string]any {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(prev)
	fn()
	var out []map[string]any
	for line := range bytes.SplitSeq(bytes.TrimSpace(buf.Bytes()), []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal(line, &m); err != nil {
			t.Fatalf("parse log line %q: %v", line, err)
		}
		out = append(out, m)
	}
	return out
}

// findAuditRecord returns the first record whose msg is "audit", or nil.
// We could parameterize, but every caller wants "audit" anyway.
func findAuditRecord(records []map[string]any) map[string]any {
	for _, r := range records {
		if r["msg"] == "audit" {
			return r
		}
	}
	return nil
}

func TestAuditLog_EmitsRecordForAcquireRelease(t *testing.T) {
	for _, path := range []string{"/acquire", "/release"} {
		t.Run(path, func(t *testing.T) {
			handler := AuditLog(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusOK)
			}))
			r := httptest.NewRequest(http.MethodPost, path, http.NoBody)
			r = r.WithContext(identity.WithCaller(r.Context(), identity.Caller{Hostname: "whale-4"}))
			w := httptest.NewRecorder()
			records := captureSlog(t, func() { handler.ServeHTTP(w, r) })
			rec := findAuditRecord(records)
			if rec == nil {
				t.Fatalf("no audit record emitted; got %v", records)
			}
			if rec["path"] != path || rec["node"] != "whale-4" || rec["remote"] != r.RemoteAddr {
				t.Errorf("record = %v", rec)
			}
		})
	}
}

func TestAuditLog_SkipsHealthzAndStatus(t *testing.T) {
	// Probes would drown the audit trail.
	for _, path := range []string{"/healthz", "/status"} {
		t.Run(path, func(t *testing.T) {
			handler := AuditLog(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusOK)
			}))
			r := httptest.NewRequest(http.MethodGet, path, http.NoBody)
			w := httptest.NewRecorder()
			records := captureSlog(t, func() { handler.ServeHTTP(w, r) })
			if rec := findAuditRecord(records); rec != nil {
				t.Errorf("got audit record for %s: %v", path, rec)
			}
		})
	}
}

func TestAuditLog_CapturesActualStatusCode(t *testing.T) {
	// The audit logger records what the handler actually returned, not
	// what the request asked for. A 503 should show as status=503.
	handler := AuditLog(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	r := httptest.NewRequest(http.MethodPost, "/acquire", http.NoBody)
	w := httptest.NewRecorder()
	records := captureSlog(t, func() { handler.ServeHTTP(w, r) })
	rec := findAuditRecord(records)
	if rec == nil {
		t.Fatal("no audit record")
	}
	status, ok := rec["status"].(float64)
	if !ok || status != http.StatusServiceUnavailable {
		t.Errorf("record.status = %v, want 503", rec["status"])
	}
}

func TestAuditLog_NoExplicitWriteHeaderStillRecords200(t *testing.T) {
	// The ResponseWriter implicitly sends 200 on first Write if
	// WriteHeader hasn't been called. The audit record must capture
	// that — otherwise a "happy path" handler that just writes a body
	// would log status=0.
	handler := AuditLog(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	r := httptest.NewRequest(http.MethodPost, "/release", http.NoBody)
	w := httptest.NewRecorder()
	records := captureSlog(t, func() { handler.ServeHTTP(w, r) })
	rec := findAuditRecord(records)
	if rec == nil {
		t.Fatal("no audit record")
	}
	status, ok := rec["status"].(float64)
	if !ok || status != http.StatusOK {
		t.Errorf("record.status = %v, want 200 (implicit)", rec["status"])
	}
}
