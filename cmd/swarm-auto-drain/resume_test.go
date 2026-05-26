package main

import (
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
)

// resumeFakeDaemon mirrors fakeDaemon but tracks /active calls too.
type resumeFakeDaemon struct {
	activateStatus int
	releaseStatus  int

	gotActivate  atomic.Int32
	gotRelease   atomic.Int32
	healthyAfter atomic.Int32 // number of /healthz hits before returning 200
}

func (f *resumeFakeDaemon) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		n := f.healthyAfter.Add(-1)
		if n >= 0 {
			http.Error(w, "starting", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/active", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		f.gotActivate.Add(1)
		status := f.activateStatus
		if status == 0 {
			status = http.StatusOK
		}
		w.WriteHeader(status)
	})
	mux.HandleFunc("/lock", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		f.gotRelease.Add(1)
		status := f.releaseStatus
		if status == 0 {
			status = http.StatusNoContent
		}
		w.WriteHeader(status)
	})
	return mux
}

func serveResumeDaemon(t *testing.T, f *resumeFakeDaemon) string {
	t.Helper()
	path := shortSocketPath(t)
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(f.handler())
	srv.Listener = ln
	srv.Start()
	t.Cleanup(srv.Close)
	return path
}

// resumeEnv sets the env vars used by resumeCmd. Returns the temp dir
// holding the resume sentinel.
func resumeEnv(t *testing.T, socket string) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("COORDINATOR_SOCKET", socket)
	t.Setenv("RESUME_SENTINEL", filepath.Join(dir, "draining"))
	t.Setenv("LOCK_TIMEOUT_SECONDS", "5")
	return dir
}

func TestResume_HappyPath(t *testing.T) {
	f := &resumeFakeDaemon{}
	dir := resumeEnv(t, serveResumeDaemon(t, f))
	writeFile(t, filepath.Join(dir, "draining"), "")

	if rc := resumeCmd(nil); rc != 0 {
		t.Errorf("exit = %d, want 0", rc)
	}
	if f.gotActivate.Load() != 1 {
		t.Errorf("activate = %d, want 1", f.gotActivate.Load())
	}
	if f.gotRelease.Load() != 1 {
		t.Errorf("release = %d, want 1", f.gotRelease.Load())
	}
	if _, err := os.Stat(filepath.Join(dir, "draining")); !os.IsNotExist(err) {
		t.Errorf("resume sentinel still present: %v", err)
	}
}

func TestResume_PeerTookOver_403(t *testing.T) {
	f := &resumeFakeDaemon{releaseStatus: http.StatusForbidden}
	dir := resumeEnv(t, serveResumeDaemon(t, f))
	writeFile(t, filepath.Join(dir, "draining"), "")

	if rc := resumeCmd(nil); rc != 0 {
		t.Errorf("exit = %d, want 0 (403 is end state)", rc)
	}
	if _, err := os.Stat(filepath.Join(dir, "draining")); !os.IsNotExist(err) {
		t.Errorf("resume sentinel still present: %v", err)
	}
}

func TestResume_PeerTookOver_404(t *testing.T) {
	f := &resumeFakeDaemon{releaseStatus: http.StatusNotFound}
	_ = resumeEnv(t, serveResumeDaemon(t, f))
	if rc := resumeCmd(nil); rc != 0 {
		t.Errorf("exit = %d, want 0 (404 is end state)", rc)
	}
}

func TestResume_ActivateFails(t *testing.T) {
	f := &resumeFakeDaemon{activateStatus: http.StatusInternalServerError}
	dir := resumeEnv(t, serveResumeDaemon(t, f))
	writeFile(t, filepath.Join(dir, "draining"), "")

	if rc := resumeCmd(nil); rc != 1 {
		t.Errorf("exit = %d, want 1", rc)
	}
	if _, err := os.Stat(filepath.Join(dir, "draining")); err != nil {
		t.Errorf("resume sentinel should remain on failure: %v", err)
	}
}

func TestResume_ReleaseUnexpectedStatus(t *testing.T) {
	f := &resumeFakeDaemon{releaseStatus: http.StatusInternalServerError}
	_ = resumeEnv(t, serveResumeDaemon(t, f))
	if rc := resumeCmd(nil); rc != 1 {
		t.Errorf("exit = %d, want 1", rc)
	}
}

func TestResume_MissingResumeSentinel(t *testing.T) {
	// Sentinel never written. Resume still runs (in practice, gated by
	// ConditionPathExists at the unit level — the binary itself doesn't
	// re-check). os.Remove of a missing path is not an error.
	f := &resumeFakeDaemon{}
	_ = resumeEnv(t, serveResumeDaemon(t, f))
	if rc := resumeCmd(nil); rc != 0 {
		t.Errorf("exit = %d, want 0", rc)
	}
}
