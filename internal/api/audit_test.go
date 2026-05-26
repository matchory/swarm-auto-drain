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

func TestAuditLog_EmitsRecordForLockDrainActive(t *testing.T) {
	for _, path := range []string{"/lock", "/drain", "/active"} {
		t.Run(path, func(t *testing.T) {
			handler := AuditLog(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusOK)
			}))
			r := httptest.NewRequest(http.MethodPost, path, http.NoBody)
			ctx := identity.WithPeerCred(r.Context(), identity.PeerCred{PID: 4242, UID: 0, GID: 0})
			r = r.WithContext(ctx)
			w := httptest.NewRecorder()
			records := captureSlog(t, func() { handler.ServeHTTP(w, r) })
			rec := findAuditRecord(records)
			if rec == nil {
				t.Fatalf("no audit record emitted; got %v", records)
			}
			if rec["path"] != path {
				t.Errorf("record.path = %v, want %s", rec["path"], path)
			}
			// Peer_pid arrives via JSON as a float64; compare via float cast.
			pid, ok := rec["peer_pid"].(float64)
			if !ok || pid != 4242 {
				t.Errorf("record.peer_pid = %v, want 4242", rec["peer_pid"])
			}
		})
	}
}

func TestAuditLog_SkipsHealthzAndRole(t *testing.T) {
	// /healthz and /role are high-frequency probes. Logging every hit
	// drowns the audit trail. Skip them entirely at INFO; operators who
	// want to see them can raise the log level.
	for _, path := range []string{"/healthz", "/role"} {
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

func TestAuditLog_OmitsPeerPidWhenAbsent(t *testing.T) {
	// On the overlay TCP path, peer creds are not set. The audit record
	// still gets emitted, it just lacks peer_pid. Don't fabricate a
	// zero PID — that would suggest a real process at PID 0.
	handler := AuditLog(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	r := httptest.NewRequest(http.MethodPost, "/drain", http.NoBody)
	w := httptest.NewRecorder()
	records := captureSlog(t, func() { handler.ServeHTTP(w, r) })
	rec := findAuditRecord(records)
	if rec == nil {
		t.Fatal("no audit record")
	}
	if _, present := rec["peer_pid"]; present {
		t.Errorf("peer_pid present without peer creds in ctx: %v", rec)
	}
}

func TestAuditLog_CapturesActualStatusCode(t *testing.T) {
	// The audit logger records what the handler actually returned, not
	// what the request asked for. A 503 should show as status=503.
	handler := AuditLog(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	r := httptest.NewRequest(http.MethodPost, "/drain", http.NoBody)
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
	r := httptest.NewRequest(http.MethodPost, "/active", http.NoBody)
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
