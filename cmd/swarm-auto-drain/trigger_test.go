package main

import (
	"bytes"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// shortSocketPath: same pattern as internal/api/server_test.go and
// internal/hostclient/client_test.go — macOS sun_path is 104 chars so
// we can't use t.TempDir().
//
//nolint:usetesting // sun_path 104-char limit precludes t.TempDir on macOS
func shortSocketPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "trg")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, fmt.Sprintf("s%d.sock", os.Getpid()))
}

// fakeDaemon is a minimal HTTP fake the trigger flow drives against.
// Counters are atomic so subtests can read them after triggerCmd returns
// without worrying about handler goroutines lingering.
type fakeDaemon struct {
	roleBody    string
	lockStatus  int
	lockBody    string
	drainStatus int
	drainBody   string

	gotLock        atomic.Int32
	gotDrain       atomic.Int32
	gotLockRelease atomic.Int32
}

func (f *fakeDaemon) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/role", func(w http.ResponseWriter, _ *http.Request) {
		body := f.roleBody
		if body == "" {
			body = `{"role":"manager","leader":"leader","version":"v0.2.0"}`
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	})
	mux.HandleFunc("/lock", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			f.gotLock.Add(1)
			status := f.lockStatus
			if status == 0 {
				status = http.StatusCreated
			}
			w.WriteHeader(status)
			body := f.lockBody
			if body == "" {
				body = `{"holder":"self","acquired_at":1,"ttl_seconds":60}`
			}
			_, _ = w.Write([]byte(body))
		case http.MethodDelete:
			f.gotLockRelease.Add(1)
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})
	mux.HandleFunc("/drain", func(w http.ResponseWriter, _ *http.Request) {
		f.gotDrain.Add(1)
		status := f.drainStatus
		if status == 0 {
			status = http.StatusOK
		}
		w.WriteHeader(status)
		body := f.drainBody
		if body == "" {
			body = `{"tasks_remaining":0,"elapsed_seconds":1}`
		}
		_, _ = w.Write([]byte(body))
	})
	return mux
}

func serveFakeDaemon(t *testing.T, f *fakeDaemon) string {
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

func triggerEnv(t *testing.T, socket string) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("COORDINATOR_SOCKET", socket)
	t.Setenv("DISABLED_FILE", filepath.Join(dir, "disabled"))
	t.Setenv("SENTINEL_FILE", filepath.Join(dir, "reboot-required"))
	t.Setenv("RESUME_SENTINEL", filepath.Join(dir, "draining"))
	t.Setenv("BLACKOUT_FILE", filepath.Join(dir, "blackout"))
	t.Setenv("REBOOT_WINDOW", "")
	t.Setenv("LOCK_TIMEOUT_SECONDS", "5")
	t.Setenv("DRAIN_TIMEOUT_SECONDS", "5")
	return dir
}

func stubReboot(t *testing.T) *atomic.Int32 {
	t.Helper()
	orig := systemctlReboot
	var n atomic.Int32
	systemctlReboot = func() error {
		n.Add(1)
		return nil
	}
	t.Cleanup(func() { systemctlReboot = orig })
	return &n
}

func stubNow(t *testing.T, now time.Time) {
	t.Helper()
	orig := triggerNow
	triggerNow = func() time.Time { return now }
	t.Cleanup(func() { triggerNow = orig })
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// captureLogs swaps slog's default logger for one that writes into a
// bytes.Buffer the test can grep. Restored via t.Cleanup.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	buf := &bytes.Buffer{}
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(orig) })
	return buf
}

func TestTrigger_Disabled(t *testing.T) {
	f := &fakeDaemon{}
	dir := triggerEnv(t, serveFakeDaemon(t, f))
	writeFile(t, filepath.Join(dir, "disabled"), "")
	writeFile(t, filepath.Join(dir, "reboot-required"), "")
	rebootN := stubReboot(t)

	if rc := triggerCmd(nil); rc != 0 {
		t.Errorf("exit = %d, want 0", rc)
	}
	if rebootN.Load() != 0 {
		t.Errorf("reboots = %d, want 0", rebootN.Load())
	}
	if f.gotLock.Load() != 0 {
		t.Error("lock acquire attempted while disabled")
	}
}

func TestTrigger_NoRebootRequired(t *testing.T) {
	f := &fakeDaemon{}
	_ = triggerEnv(t, serveFakeDaemon(t, f))
	rebootN := stubReboot(t)
	if rc := triggerCmd(nil); rc != 0 {
		t.Errorf("exit = %d, want 0", rc)
	}
	if rebootN.Load() != 0 {
		t.Errorf("reboots = %d, want 0", rebootN.Load())
	}
	if f.gotLock.Load() != 0 {
		t.Error("lock acquire attempted without sentinel")
	}
}

func TestTrigger_OutsideWindow(t *testing.T) {
	f := &fakeDaemon{}
	dir := triggerEnv(t, serveFakeDaemon(t, f))
	writeFile(t, filepath.Join(dir, "reboot-required"), "")
	t.Setenv("REBOOT_WINDOW", "Mon 03:00-04:00 UTC")
	stubNow(t, time.Date(2026, 5, 22, 12, 0, 0, 0, time.UTC)) // Friday noon
	rebootN := stubReboot(t)

	if rc := triggerCmd(nil); rc != 0 {
		t.Errorf("exit = %d", rc)
	}
	if rebootN.Load() != 0 {
		t.Errorf("reboots = %d, want 0", rebootN.Load())
	}
}

func TestTrigger_InBlackout(t *testing.T) {
	f := &fakeDaemon{}
	dir := triggerEnv(t, serveFakeDaemon(t, f))
	writeFile(t, filepath.Join(dir, "reboot-required"), "")
	writeFile(t, filepath.Join(dir, "blackout"), "2026-05-22\n")
	stubNow(t, time.Date(2026, 5, 22, 3, 0, 0, 0, time.UTC))
	rebootN := stubReboot(t)

	if rc := triggerCmd(nil); rc != 0 {
		t.Errorf("exit = %d", rc)
	}
	if rebootN.Load() != 0 {
		t.Errorf("reboots = %d", rebootN.Load())
	}
}

func TestTrigger_HappyPath(t *testing.T) {
	f := &fakeDaemon{}
	dir := triggerEnv(t, serveFakeDaemon(t, f))
	writeFile(t, filepath.Join(dir, "reboot-required"), "")
	rebootN := stubReboot(t)

	if rc := triggerCmd(nil); rc != 0 {
		t.Errorf("exit = %d, want 0", rc)
	}
	if rebootN.Load() != 1 {
		t.Errorf("reboots = %d, want 1", rebootN.Load())
	}
	if f.gotLock.Load() != 1 {
		t.Errorf("lock attempts = %d, want 1", f.gotLock.Load())
	}
	if f.gotDrain.Load() != 1 {
		t.Errorf("drain attempts = %d, want 1", f.gotDrain.Load())
	}
	if _, err := os.Stat(filepath.Join(dir, "draining")); err != nil {
		t.Errorf("resume sentinel not written: %v", err)
	}
}

func TestTrigger_LockHeldByPeer(t *testing.T) {
	f := &fakeDaemon{
		lockStatus: http.StatusConflict,
		lockBody:   `{"holder":"peer","acquired_at":1,"ttl_seconds":60}`,
	}
	dir := triggerEnv(t, serveFakeDaemon(t, f))
	writeFile(t, filepath.Join(dir, "reboot-required"), "")
	rebootN := stubReboot(t)

	if rc := triggerCmd(nil); rc != 0 {
		t.Errorf("exit = %d, want 0 (skip)", rc)
	}
	if rebootN.Load() != 0 {
		t.Errorf("reboots = %d, want 0", rebootN.Load())
	}
	if f.gotLockRelease.Load() != 0 {
		t.Errorf("releases = %d, want 0 (we never acquired)", f.gotLockRelease.Load())
	}
}

func TestTrigger_DrainTimeout(t *testing.T) {
	f := &fakeDaemon{drainStatus: http.StatusRequestTimeout}
	dir := triggerEnv(t, serveFakeDaemon(t, f))
	writeFile(t, filepath.Join(dir, "reboot-required"), "")
	rebootN := stubReboot(t)

	if rc := triggerCmd(nil); rc != 1 {
		t.Errorf("exit = %d, want 1", rc)
	}
	if rebootN.Load() != 0 {
		t.Errorf("reboots = %d, want 0", rebootN.Load())
	}
	if f.gotLockRelease.Load() != 1 {
		t.Errorf("releases = %d, want 1 (cleanup after drain failure)", f.gotLockRelease.Load())
	}
}

func TestTrigger_QuorumGuard(t *testing.T) {
	f := &fakeDaemon{drainStatus: http.StatusServiceUnavailable}
	dir := triggerEnv(t, serveFakeDaemon(t, f))
	writeFile(t, filepath.Join(dir, "reboot-required"), "")
	rebootN := stubReboot(t)

	if rc := triggerCmd(nil); rc != 0 {
		t.Errorf("exit = %d, want 0 (skip)", rc)
	}
	if rebootN.Load() != 0 {
		t.Errorf("reboots = %d", rebootN.Load())
	}
	if f.gotLockRelease.Load() != 0 {
		t.Error("release attempted on quorum-guard exit 0 — should only release when exiting non-zero")
	}
}

func TestTrigger_VersionSkewWarning(t *testing.T) {
	f := &fakeDaemon{
		roleBody: `{"role":"manager","leader":"leader","version":"v999.0.0"}`,
	}
	dir := triggerEnv(t, serveFakeDaemon(t, f))
	writeFile(t, filepath.Join(dir, "reboot-required"), "")
	rebootN := stubReboot(t)

	prev := version
	version = "v0.2.0"
	t.Cleanup(func() { version = prev })

	logs := captureLogs(t)
	if rc := triggerCmd(nil); rc != 0 {
		t.Errorf("exit = %d, want 0", rc)
	}
	if rebootN.Load() != 1 {
		t.Errorf("reboots = %d, want 1 (skew doesn't block)", rebootN.Load())
	}
	if !strings.Contains(logs.String(), "daemon version skew") {
		t.Errorf("expected skew warning in logs:\n%s", logs.String())
	}
}

func TestMajorSkew(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"v0.2.0", "v0.3.0", false},
		{"v0.2.0", "v1.0.0", true},
		{"v2", "v1", true},
		{"v1.0", "1.0", false},
		{"", "v1", false},
		{"dev", "v1", false},
		{"v1", "v1", false},
	}
	for _, tc := range cases {
		t.Run(tc.a+"/"+tc.b, func(t *testing.T) {
			if got := majorSkew(tc.a, tc.b); got != tc.want {
				t.Errorf("majorSkew(%q,%q) = %v, want %v", tc.a, tc.b, got, tc.want)
			}
		})
	}
}
