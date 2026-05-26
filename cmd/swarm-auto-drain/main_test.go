package main

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// shortSocketDir returns a short-path temp dir for Unix sockets.
// Darwin's sun_path is 104 bytes; t.TempDir() under /var/folders/...
// regularly exceeds that. We use /tmp instead.
//
//nolint:usetesting,godot // sun_path 104-char limit precludes t.TempDir on macOS
func shortSocketDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("/tmp", "sad")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(d) })
	return d
}

func TestOpenUnixListener_CreatesParentDir(t *testing.T) {
	dir := shortSocketDir(t)
	path := filepath.Join(dir, "sub", "coord.sock")
	ln, err := openUnixListener(context.Background(), path, 0o660, os.Getgid())
	if err != nil {
		t.Fatalf("openUnixListener: %v", err)
	}
	defer func() { _ = ln.Close() }()

	if _, statErr := os.Stat(filepath.Dir(path)); statErr != nil {
		t.Errorf("parent dir missing: %v", statErr)
	}
	if _, statErr := os.Stat(path); statErr != nil {
		t.Errorf("socket missing: %v", statErr)
	}
}

func TestOpenUnixListener_ChmodsSocketToConfiguredMode(t *testing.T) {
	dir := shortSocketDir(t)
	path := filepath.Join(dir, "coord.sock")
	ln, err := openUnixListener(context.Background(), path, 0o640, os.Getgid())
	if err != nil {
		t.Fatalf("openUnixListener: %v", err)
	}
	defer func() { _ = ln.Close() }()

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	// Mode().Perm() strips type bits (socket bit etc.) so we can compare
	// the permission triplet directly.
	if got := info.Mode().Perm(); got != 0o640 {
		t.Errorf("socket mode = %o, want 0640", got)
	}
}

func TestOpenUnixListener_TightensParentDirMode(t *testing.T) {
	// Simulate Docker's bind-mount auto-creation as 0755 root:root, then
	// verify openUnixListener tightens it to 0750.
	dir := shortSocketDir(t)
	loose := filepath.Join(dir, "loose")
	if err := os.MkdirAll(loose, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(loose, "coord.sock")
	ln, err := openUnixListener(context.Background(), path, 0o660, os.Getgid())
	if err != nil {
		t.Fatalf("openUnixListener: %v", err)
	}
	defer func() { _ = ln.Close() }()

	info, err := os.Stat(loose)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o750 {
		t.Errorf("parent dir mode = %o, want 0750", got)
	}
}

func TestOpenUnixListener_RemovesStaleSocket(t *testing.T) {
	dir := shortSocketDir(t)
	path := filepath.Join(dir, "coord.sock")
	// Build a socket file directly so the simulation works the same on
	// Linux and macOS — net.Listen+Close cleans up the file on some
	// platforms, which would defeat the fixture.
	if mkErr := makeOrphanSocket(path); mkErr != nil {
		t.Skipf("cannot construct stale-socket fixture: %v", mkErr)
	}
	info, statErr := os.Stat(path)
	if statErr != nil {
		t.Fatalf("stale socket fixture missing: %v", statErr)
	}
	if info.Mode()&os.ModeSocket == 0 {
		t.Fatalf("stale fixture is not a socket: mode=%v", info.Mode())
	}

	ln, err := openUnixListener(context.Background(), path, 0o660, os.Getgid())
	if err != nil {
		t.Fatalf("openUnixListener: %v", err)
	}
	defer func() { _ = ln.Close() }()
	if _, stat2Err := os.Stat(path); stat2Err != nil {
		t.Errorf("socket missing after rebind: %v", stat2Err)
	}
}

// makeOrphanSocket creates a Unix socket file at path with no listener
// behind it — same shape as the file left behind by a crashed process
// that bound a socket but never unlinked it on exit.
func makeOrphanSocket(path string) error {
	// Use net.ListenUnixgram for SOCK_DGRAM: Listen returns a UnixConn
	// (not a Listener) and Close on Linux leaves the file in place. We
	// keep the conn around in the package var so GC doesn't finalize it
	// and trigger any cleanup. On a successful return, the socket file
	// exists with no live process accepting on it — exactly the state
	// openUnixListener must recover from.
	addr, err := net.ResolveUnixAddr("unixgram", path)
	if err != nil {
		return err
	}
	conn, err := net.ListenUnixgram("unixgram", addr)
	if err != nil {
		return err
	}
	staleSocketKeepalive = append(staleSocketKeepalive, conn)
	return nil
}

var staleSocketKeepalive []*net.UnixConn

func TestOpenUnixListener_RefusesToClobberRegularFile(t *testing.T) {
	dir := shortSocketDir(t)
	path := filepath.Join(dir, "coord.sock")
	if err := os.WriteFile(path, []byte("not a socket"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := openUnixListener(context.Background(), path, 0o660, os.Getgid())
	if !errors.Is(err, errNonSocketAtPath) {
		t.Errorf("err = %v, want errNonSocketAtPath", err)
	}
	// And the file must still be there — we refused, didn't clobber.
	if _, statErr := os.Stat(path); statErr != nil {
		t.Errorf("regular file deleted despite refusal: %v", statErr)
	}
}

func TestOpenUnixListener_EmptyPathErrors(t *testing.T) {
	_, err := openUnixListener(context.Background(), "", 0o660, 0)
	if !errors.Is(err, errUnixPathEmpty) {
		t.Errorf("err = %v, want errUnixPathEmpty", err)
	}
}

func TestOpenUnixListener_AcceptsConnections(t *testing.T) {
	// End-to-end: the listener returned actually serves traffic. Combined
	// with the chmod test above, this proves the full bootstrap chain.
	if runtime.GOOS == "windows" {
		t.Skip("unix sockets not supported")
	}
	dir := shortSocketDir(t)
	path := filepath.Join(dir, "coord.sock")
	ln, err := openUnixListener(context.Background(), path, 0o660, os.Getgid())
	if err != nil {
		t.Fatalf("openUnixListener: %v", err)
	}
	defer func() { _ = ln.Close() }()

	accepted := make(chan struct{}, 1)
	go func() {
		conn, acceptErr := ln.Accept()
		if acceptErr == nil {
			_ = conn.Close()
			accepted <- struct{}{}
		}
	}()

	client, err := net.DialTimeout("unix", path, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_ = client.Close()

	select {
	case <-accepted:
	case <-time.After(time.Second):
		t.Error("listener did not accept connection")
	}
}
