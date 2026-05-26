package identity

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"
)

// shortSocketDir returns a temp directory short enough for macOS's
// 104-char sun_path limit. /tmp instead of t.TempDir() — the default
// /var/folders prefix on darwin regularly exceeds the budget.
//
//nolint:usetesting // sun_path 104-char limit precludes t.TempDir on macOS
func shortSocketDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("/tmp", "pc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(d) })
	return d
}

func TestReadPeerCred_ReturnsOwnProcessCredentials(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("SO_PEERCRED is Linux-only")
	}
	dir := shortSocketDir(t)
	path := filepath.Join(dir, "s.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()

	type result struct {
		creds PeerCred
		err   error
	}
	got := make(chan result, 1)
	var wg sync.WaitGroup
	wg.Go(func() {
		conn, acceptErr := ln.Accept()
		if acceptErr != nil {
			got <- result{err: acceptErr}
			return
		}
		defer func() { _ = conn.Close() }()
		uc, ok := conn.(*net.UnixConn)
		if !ok {
			got <- result{err: fmt.Errorf("conn is %T, want *net.UnixConn", conn)}
			return
		}
		c, readErr := ReadPeerCred(uc)
		got <- result{creds: c, err: readErr}
	})

	client, err := net.DialTimeout("unix", path, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()

	select {
	case r := <-got:
		if r.err != nil {
			t.Fatalf("ReadPeerCred: %v", r.err)
		}
		// The client side IS our test process, so the kernel-reported
		// peer credentials must match our own UID/GID/PID exactly.
		if r.creds.UID != uint32(os.Getuid()) {
			t.Errorf("UID = %d, want %d (own process)", r.creds.UID, os.Getuid())
		}
		if r.creds.GID != uint32(os.Getgid()) {
			t.Errorf("GID = %d, want %d (own process)", r.creds.GID, os.Getgid())
		}
		if r.creds.PID != uint32(os.Getpid()) {
			t.Errorf("PID = %d, want %d (own process)", r.creds.PID, os.Getpid())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for peer-cred read")
	}
	wg.Wait()
}

func TestReadPeerCred_RejectsNilConn(t *testing.T) {
	_, err := ReadPeerCred(nil)
	if !errors.Is(err, ErrInvalidConn) {
		t.Errorf("err = %v, want ErrInvalidConn", err)
	}
}

func TestPeerCred_FromContextEmptyOnBareCtx(t *testing.T) {
	// Defense-in-depth: callers that forget to inject peer creds get a
	// zero value and a false ok, never some half-resolved garbage.
	pc, ok := PeerCredFromContext(t.Context())
	if ok {
		t.Error("ok = true, want false on bare context")
	}
	if pc != (PeerCred{}) {
		t.Errorf("pc = %+v, want zero value", pc)
	}
}

func TestWithPeerCred_RoundTrip(t *testing.T) {
	want := PeerCred{PID: 1234, UID: 0, GID: 0}
	ctx := WithPeerCred(t.Context(), want)
	got, ok := PeerCredFromContext(ctx)
	if !ok {
		t.Fatal("ok = false, want true after WithPeerCred")
	}
	if got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
}
