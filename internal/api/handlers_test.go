package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/matchory/swarm-auto-drain/internal/drain"
	"github.com/matchory/swarm-auto-drain/internal/identity"
	"github.com/matchory/swarm-auto-drain/internal/lock"
)

type fakeStore struct {
	state *lock.Lock
}

func (f *fakeStore) Acquire(_ context.Context, holder string, ttl time.Duration) (lock.AcquireResult, error) {
	if f.state == nil {
		f.state = &lock.Lock{Holder: holder, AcquiredAt: 1, TTLSeconds: int(ttl.Seconds())}
		return lock.AcquireResult{Status: lock.Acquired, Current: f.state}, nil
	}
	if f.state.Holder == holder {
		return lock.AcquireResult{Status: lock.Acquired, Current: f.state}, nil
	}
	return lock.AcquireResult{Status: lock.HeldByOther, Current: f.state}, nil
}
func (f *fakeStore) Release(_ context.Context, holder string) error {
	if f.state == nil {
		return lock.ErrNoLock
	}
	if f.state.Holder != holder {
		return lock.ErrNotHolder
	}
	f.state = nil
	return nil
}
func (f *fakeStore) Inspect(_ context.Context) (*lock.Lock, error) { return f.state, nil }

type fakeDrainer struct {
	quorumOK    bool
	quorumRep   drain.QuorumReport
	drainRes    drain.Result
	drainErr    error
	activateErr error
}

func (f *fakeDrainer) QuorumOK(_ context.Context, _ string) (bool, drain.QuorumReport, error) {
	return f.quorumOK, f.quorumRep, nil
}
func (f *fakeDrainer) Drain(_ context.Context, _ string, _, _ time.Duration) (drain.Result, error) {
	return f.drainRes, f.drainErr
}
func (f *fakeDrainer) Activate(_ context.Context, _ string) error { return f.activateErr }

func newTestServer(t *testing.T, store lock.Store, dr drain.Drainer) *httptest.Server {
	t.Helper()
	h := Handlers{
		Store:        store,
		Drainer:      dr,
		LockTTL:      30 * time.Minute,
		DrainTimeout: time.Second,
		PollInterval: 10 * time.Millisecond,
		Role:         func() string { return "manager" },
	}
	mw := identity.Middleware{NodeHostname: "self-host", PublicPaths: []string{"/healthz", "/role"}}
	return httptest.NewServer(mw.Wrap(h.Router()))
}

func TestHealthz(t *testing.T) {
	srv := newTestServer(t, &fakeStore{}, &fakeDrainer{quorumOK: true})
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Errorf("status = %d", resp.StatusCode)
	}
}

func TestRole(t *testing.T) {
	srv := newTestServer(t, &fakeStore{}, &fakeDrainer{quorumOK: true})
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/role")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body struct{ Role string }
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if body.Role != "manager" {
		t.Errorf("role = %q", body.Role)
	}
}

func TestLockAcquire_Fresh(t *testing.T) {
	srv := newTestServer(t, &fakeStore{}, &fakeDrainer{quorumOK: true})
	defer srv.Close()
	resp, _ := http.Post(srv.URL+"/lock", "application/json", nil)
	if resp.StatusCode != http.StatusCreated {
		t.Errorf("status = %d, want 201", resp.StatusCode)
	}
}

func TestLockAcquire_HeldByOther(t *testing.T) {
	store := &fakeStore{state: &lock.Lock{Holder: "other-host", AcquiredAt: 1, TTLSeconds: 1800}}
	srv := newTestServer(t, store, &fakeDrainer{quorumOK: true})
	defer srv.Close()
	resp, _ := http.Post(srv.URL+"/lock", "application/json", nil)
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("status = %d, want 409", resp.StatusCode)
	}
}

func TestLockRelease_Holder(t *testing.T) {
	store := &fakeStore{state: &lock.Lock{Holder: "self-host", AcquiredAt: 1, TTLSeconds: 1800}}
	srv := newTestServer(t, store, &fakeDrainer{quorumOK: true})
	defer srv.Close()
	req, _ := http.NewRequest("DELETE", srv.URL+"/lock", nil)
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("status = %d, want 204", resp.StatusCode)
	}
}

func TestLockRelease_NotHolder(t *testing.T) {
	store := &fakeStore{state: &lock.Lock{Holder: "other-host", AcquiredAt: 1, TTLSeconds: 1800}}
	srv := newTestServer(t, store, &fakeDrainer{quorumOK: true})
	defer srv.Close()
	req, _ := http.NewRequest("DELETE", srv.URL+"/lock", nil)
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403", resp.StatusCode)
	}
}

func TestLockRelease_NoLock(t *testing.T) {
	srv := newTestServer(t, &fakeStore{}, &fakeDrainer{quorumOK: true})
	defer srv.Close()
	req, _ := http.NewRequest("DELETE", srv.URL+"/lock", nil)
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestDrain_QuorumFail(t *testing.T) {
	srv := newTestServer(t, &fakeStore{}, &fakeDrainer{quorumOK: false, quorumRep: drain.QuorumReport{
		TotalManagers: 3, OthersReachable: 1, Required: 2,
	}})
	defer srv.Close()
	resp, _ := http.Post(srv.URL+"/drain", "application/json", nil)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", resp.StatusCode)
	}
}

func TestDrain_Happy(t *testing.T) {
	srv := newTestServer(t, &fakeStore{}, &fakeDrainer{
		quorumOK: true,
		drainRes: drain.Result{TasksRemaining: 0, Elapsed: 100 * time.Millisecond},
	})
	defer srv.Close()
	resp, _ := http.Post(srv.URL+"/drain", "application/json", nil)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d", resp.StatusCode)
	}
}

func TestDrain_Timeout(t *testing.T) {
	srv := newTestServer(t, &fakeStore{}, &fakeDrainer{
		quorumOK: true,
		drainRes: drain.Result{TasksRemaining: 4, Elapsed: time.Second, TimedOut: true},
	})
	defer srv.Close()
	resp, _ := http.Post(srv.URL+"/drain", "application/json", nil)
	if resp.StatusCode != http.StatusRequestTimeout {
		t.Errorf("status = %d, want 408", resp.StatusCode)
	}
}

func TestDrain_Error(t *testing.T) {
	srv := newTestServer(t, &fakeStore{}, &fakeDrainer{quorumOK: true, drainErr: errors.New("boom")})
	defer srv.Close()
	resp, _ := http.Post(srv.URL+"/drain", "application/json", nil)
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("status = %d", resp.StatusCode)
	}
}

func TestActive(t *testing.T) {
	srv := newTestServer(t, &fakeStore{}, &fakeDrainer{quorumOK: true})
	defer srv.Close()
	resp, _ := http.Post(srv.URL+"/active", "application/json", nil)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d", resp.StatusCode)
	}
}

func TestIdentityMissing_Drain(t *testing.T) {
	srv := newTestServer(t, &fakeStore{}, &fakeDrainer{quorumOK: true})
	defer srv.Close()
	req, _ := http.NewRequest("POST", srv.URL+"/drain", strings.NewReader(""))
	req.Header.Set("Host", "from-overlay")
	resp, _ := http.DefaultClient.Do(req)
	_ = resp
}
