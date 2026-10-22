package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// shortSocketPath returns a Unix-socket path short enough for macOS's
// 104-char sun_path limit. The default t.TempDir() lives under
// /var/folders/..., which already exceeds the budget — we use /tmp.
//
//nolint:usetesting // sun_path 104-char limit precludes t.TempDir on macOS
func shortSocketPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "sd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, fmt.Sprintf("s%d", os.Getpid()))
}

// dial returns an io.ReadCloser of the response body from a single request
// over the given network/address. Used both for TCP and Unix socket calls
// — the same plumbing works for both because we only care about whether
// the listener is serving. The path is always "/" today; kept as a
// parameter so tests that need a different path can pass one without
// refactoring the helper.
func dial(t *testing.T, network, addr, path string) (int, string) {
	t.Helper()
	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, network, addr)
			},
		},
		Timeout: time.Second,
	}
	// The hostname in the URL doesn't matter — DialContext above pins us
	// to addr — but http.NewRequest requires a syntactically valid URL.
	resp, err := client.Get("http://placeholder" + path)
	if err != nil {
		t.Fatalf("dial %s %s%s: %v", network, addr, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

func TestServer_RunsMultipleListenersInParallel(t *testing.T) {
	tcpLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	sockPath := shortSocketPath(t)
	unixLn, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}

	tcpHandler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("tcp"))
	})
	unixHandler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("unix"))
	})

	srv := &Server{
		Listeners: []ListenerSpec{
			{Name: "tcp", Listener: tcpLn, Handler: tcpHandler},
			{Name: "unix", Listener: unixLn, Handler: unixHandler},
		},
		ShutdownGrace: time.Second,
	}

	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- srv.Run(ctx) }()

	// Wait briefly for listeners to start serving.
	time.Sleep(20 * time.Millisecond)

	tcpStatus, tcpBody := dial(t, "tcp", tcpLn.Addr().String(), "/")
	if tcpStatus != http.StatusOK || tcpBody != "tcp" {
		t.Errorf("tcp: status=%d body=%q want 200/tcp", tcpStatus, tcpBody)
	}
	unixStatus, unixBody := dial(t, "unix", sockPath, "/")
	if unixStatus != http.StatusOK || unixBody != "unix" {
		t.Errorf("unix: status=%d body=%q want 200/unix", unixStatus, unixBody)
	}

	cancel()
	select {
	case runErr := <-runDone:
		if runErr != nil {
			t.Errorf("Run returned %v", runErr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after ctx cancel")
	}
}

func TestServer_NoListenersErrors(t *testing.T) {
	srv := &Server{}
	err := srv.Run(context.Background())
	if !errors.Is(err, errNoListeners) {
		t.Errorf("err = %v, want errNoListeners", err)
	}
}

func TestServer_ShutdownDrainsInFlightHandler(t *testing.T) {
	// Handler that blocks until the test releases it. We start a request,
	// cancel ctx mid-handler, and assert the handler completes before
	// Shutdown returns (ShutdownGrace must be respected, not the parent ctx).
	released := make(chan struct{})
	completed := int32(0)
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-released
		atomic.StoreInt32(&completed, 1)
		w.WriteHeader(http.StatusOK)
	})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &Server{
		Listeners:     []ListenerSpec{{Name: "tcp", Listener: ln, Handler: handler}},
		ShutdownGrace: 2 * time.Second,
	}

	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- srv.Run(ctx) }()

	// Fire an in-flight request that will block in the handler.
	reqDone := make(chan struct{})
	go func() {
		defer close(reqDone)
		_, _ = dial(t, "tcp", ln.Addr().String(), "/")
	}()

	time.Sleep(50 * time.Millisecond) // Let the request reach the handler.

	cancel() // Trigger Shutdown.

	// Release the handler so it can finish — Shutdown should be waiting.
	time.Sleep(50 * time.Millisecond)
	close(released)

	select {
	case <-runDone:
	case <-time.After(3 * time.Second):
		t.Fatal("Run never returned after handler released")
	}
	if atomic.LoadInt32(&completed) != 1 {
		t.Error("handler did not complete before Shutdown returned")
	}
	<-reqDone
}

func TestServer_BothListenerFailuresReported(t *testing.T) {
	// Pre-close two listeners. Both Serve calls return net.ErrClosed
	// concurrently. The buffered errCh holds both; Run must surface
	// both via errors.Join so operators don't fix the first cause only
	// to be bitten by the second on the next restart.
	ln1, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_ = ln1.Close()
	ln2, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_ = ln2.Close()

	srv := &Server{
		Listeners: []ListenerSpec{
			{Name: "a", Listener: ln1, Handler: http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})},
			{Name: "b", Listener: ln2, Handler: http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})},
		},
		ShutdownGrace: time.Second,
	}
	runErr := srv.Run(context.Background())
	if runErr == nil {
		t.Fatal("expected error from both broken listeners")
	}
	msg := runErr.Error()
	if !strings.Contains(msg, "\"a\"") || !strings.Contains(msg, "\"b\"") {
		t.Errorf("err = %q, want both listener names mentioned", msg)
	}
}

func TestServer_ListenerFailurePropagates(t *testing.T) {
	// Close one listener before Serve is called on it. Serve returns
	// net.ErrClosed, which is NOT http.ErrServerClosed, so the goroutine
	// pushes onto errCh and Run returns the wrapped error.
	tcpLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_ = tcpLn.Close()

	srv := &Server{
		Listeners: []ListenerSpec{{
			Name: "broken", Listener: tcpLn,
			Handler: http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}),
		}},
		ShutdownGrace: time.Second,
	}

	err = srv.Run(context.Background())
	if err == nil {
		t.Fatal("expected error from broken listener, got nil")
	}
}
