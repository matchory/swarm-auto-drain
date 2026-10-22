package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/matchory/swarm-auto-drain/internal/drain"
	"github.com/matchory/swarm-auto-drain/internal/identity"
	"github.com/matchory/swarm-auto-drain/internal/metrics"
	"github.com/matchory/swarm-auto-drain/internal/slots"
)

const notManager = "Error response from daemon: This node is not a swarm manager."

type fakeSlots struct {
	held       map[string]*slots.Slot
	full       []string // non-nil: Acquire reports every slot taken by these
	err        error
	acquired   bool
	releaseErr error
}

func (f *fakeSlots) Acquire(_ context.Context, holder string, _ slots.Kind) (*slots.Slot, []string, error) {
	if f.err != nil {
		return nil, nil, f.err
	}
	if s, ok := f.held[holder]; ok {
		return s, nil, nil
	}
	if f.full != nil {
		return nil, f.full, nil
	}
	f.acquired = true
	s := &slots.Slot{Name: "reboot-slot-1", Holder: holder, AcquiredAt: 1}
	f.held[holder] = s
	return s, nil, nil
}

func (f *fakeSlots) Release(_ context.Context, holder string) (bool, error) {
	if f.releaseErr != nil {
		return false, f.releaseErr
	}
	if _, ok := f.held[holder]; !ok {
		return false, nil
	}
	delete(f.held, holder)
	return true, nil
}

func (f *fakeSlots) HeldBy(_ context.Context, holder string) (*slots.Slot, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.held[holder], nil
}

type fakeDrainer struct {
	quorumOK    bool
	drainRes    drain.Result
	drainErr    error
	activateErr error
	quorumFor   string
	drained     string
	activated   string
}

func (f *fakeDrainer) QuorumOK(_ context.Context, id string) (bool, drain.QuorumReport, error) {
	f.quorumFor = id
	return f.quorumOK, drain.QuorumReport{TotalManagers: 3, Required: 2}, nil
}

func (*fakeDrainer) Managers(context.Context) (drain.ManagersHealth, error) {
	return drain.ManagersHealth{}, nil
}

func (f *fakeDrainer) Drain(_ context.Context, id string, _, _ time.Duration) (drain.Result, error) {
	f.drained = id
	return f.drainRes, f.drainErr
}

func (f *fakeDrainer) Activate(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.activated = id
	return f.activateErr
}

var (
	worker  = identity.Caller{Hostname: "whale-4", Availability: "active"}
	manager = identity.Caller{Hostname: "whale-1", Availability: "active", Manager: true}
)

// call sends one request as caller and decodes the JSON body.
func call(t *testing.T, st *fakeSlots, dr *fakeDrainer, caller identity.Caller, method, path string) (int, map[string]any) {
	t.Helper()
	if st.held == nil {
		st.held = map[string]*slots.Slot{}
	}
	h := Handlers{
		Slots:        st,
		Drainer:      dr,
		DrainTimeout: time.Second,
		PollInterval: time.Millisecond,
		HTTPMetrics:  metrics.NewNoop().HTTP,
		DrainMetrics: metrics.NewNoop().Drain,
	}
	req := httptest.NewRequest(method, path, http.NoBody)
	req = req.WithContext(identity.WithCaller(req.Context(), caller))
	rec := httptest.NewRecorder()
	h.Router().ServeHTTP(rec, req)
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	return rec.Code, body
}

func TestAcquire_WorkerDrainsAndHoldsASlot(t *testing.T) {
	st, dr := &fakeSlots{}, &fakeDrainer{quorumOK: true}
	code, body := call(t, st, dr, worker, http.MethodPost, "/acquire")
	if code != http.StatusOK {
		t.Fatalf("status = %d, body %v", code, body)
	}
	if dr.drained != "whale-4" || dr.quorumFor != "" {
		t.Fatalf("drained %q, quorum checked for %q; want whale-4 and no quorum check", dr.drained, dr.quorumFor)
	}
	if slotName(body) != "reboot-slot-1" {
		t.Fatalf("body = %v", body)
	}
}

// slotName returns body.slot.name, or "" when absent.
func slotName(body map[string]any) string {
	slot, ok := body["slot"].(map[string]any)
	if !ok {
		return ""
	}
	if name, ok := slot["name"].(string); ok {
		return name
	}
	return ""
}

func TestAcquire_FullIs409AndDrainsNothing(t *testing.T) {
	st, dr := &fakeSlots{full: []string{"whale-5"}}, &fakeDrainer{quorumOK: true}
	code, body := call(t, st, dr, worker, http.MethodPost, "/acquire")
	if code != http.StatusConflict || dr.drained != "" {
		t.Fatalf("status = %d, drained %q", code, dr.drained)
	}
	if holders, ok := body["holders"].([]any); !ok || len(holders) != 1 || holders[0] != "whale-5" {
		t.Fatalf("body = %v", body)
	}
}

func TestAcquire_ManagerWithoutQuorumIs503AndTakesNoSlot(t *testing.T) {
	st, dr := &fakeSlots{}, &fakeDrainer{quorumOK: false}
	code, _ := call(t, st, dr, manager, http.MethodPost, "/acquire")
	if code != http.StatusServiceUnavailable || st.acquired || dr.drained != "" {
		t.Fatalf("status = %d, acquired %v, drained %q", code, st.acquired, dr.drained)
	}
	if dr.quorumFor != "whale-1" {
		t.Fatalf("quorum checked for %q, want whale-1", dr.quorumFor)
	}
}

func TestAcquire_AlreadyHoldingSkipsQuorumAndDrainsAgain(t *testing.T) {
	st := &fakeSlots{held: map[string]*slots.Slot{"whale-1": {Name: "reboot-slot-manager", Holder: "whale-1"}}}
	dr := &fakeDrainer{quorumOK: false}
	code, _ := call(t, st, dr, manager, http.MethodPost, "/acquire")
	if code != http.StatusOK || dr.quorumFor != "" || dr.drained != "whale-1" {
		t.Fatalf("status = %d, quorum for %q, drained %q", code, dr.quorumFor, dr.drained)
	}
}

func TestAcquire_DrainTimeoutRollsBack(t *testing.T) {
	st := &fakeSlots{}
	dr := &fakeDrainer{quorumOK: true, drainRes: drain.Result{TasksRemaining: 2, TimedOut: true}}
	code, _ := call(t, st, dr, worker, http.MethodPost, "/acquire")
	if code != http.StatusRequestTimeout {
		t.Fatalf("status = %d, want 408", code)
	}
	if dr.activated != "whale-4" || st.held["whale-4"] != nil {
		t.Fatalf("activated %q, slot %v; want node active and slot freed", dr.activated, st.held["whale-4"])
	}
}

func TestAcquire_DrainErrorRollsBack(t *testing.T) {
	st := &fakeSlots{}
	dr := &fakeDrainer{quorumOK: true, drainErr: errors.New("node update: boom")}
	code, _ := call(t, st, dr, worker, http.MethodPost, "/acquire")
	if code != http.StatusInternalServerError || dr.activated != "whale-4" || st.held["whale-4"] != nil {
		t.Fatalf("status = %d, activated %q, slot %v", code, dr.activated, st.held["whale-4"])
	}
}

func TestAcquire_CallerGoneStillRollsBack(t *testing.T) {
	st := &fakeSlots{held: map[string]*slots.Slot{}}
	dr := &fakeDrainer{quorumOK: true, drainErr: context.Canceled}
	h := Handlers{
		Slots: st, Drainer: dr, DrainTimeout: time.Second, PollInterval: time.Millisecond,
		HTTPMetrics: metrics.NewNoop().HTTP, DrainMetrics: metrics.NewNoop().Drain,
	}
	ctx, cancel := context.WithCancel(identity.WithCaller(context.Background(), worker))
	cancel() // the caller hung up mid-drain
	req := httptest.NewRequest(http.MethodPost, "/acquire", http.NoBody).WithContext(ctx)
	h.Router().ServeHTTP(httptest.NewRecorder(), req)
	if dr.activated != "whale-4" || st.held["whale-4"] != nil {
		t.Fatalf("activated %q, slot %v; want the rollback to finish without the caller", dr.activated, st.held["whale-4"])
	}
}

func TestAcquire_OverlappingRequestFromTheSameNodeIs409(t *testing.T) {
	h := Handlers{inFlight: &sync.Map{}}
	release, ok := h.claim("whale-4")
	if !ok {
		t.Fatal("first claim refused")
	}
	if _, again := h.claim("whale-4"); again {
		t.Fatal("second claim for the same node granted while the first is in flight")
	}
	if _, other := h.claim("whale-5"); !other {
		t.Fatal("claim for another node refused")
	}
	release()
	if _, ok = h.claim("whale-4"); !ok {
		t.Fatal("claim refused after release")
	}
}

func TestNotAManagerIs503(t *testing.T) {
	for _, path := range []string{"/acquire", "/release"} {
		st := &fakeSlots{err: errors.New(notManager)}
		code, body := call(t, st, &fakeDrainer{quorumOK: true}, worker, http.MethodPost, path)
		if code != http.StatusServiceUnavailable || body["error"] != "not a manager" {
			t.Fatalf("%s: status = %d, body %v", path, code, body)
		}
	}
}

func TestRelease_HolderIsActivatedThenFreed(t *testing.T) {
	st := &fakeSlots{held: map[string]*slots.Slot{"whale-4": {Name: "reboot-slot-1", Holder: "whale-4"}}}
	dr := &fakeDrainer{}
	code, body := call(t, st, dr, worker, http.MethodPost, "/release")
	released, ok := body["released"].(bool)
	if code != http.StatusOK || !ok || !released || dr.activated != "whale-4" || st.held["whale-4"] != nil {
		t.Fatalf("status = %d, body %v, activated %q", code, body, dr.activated)
	}
}

func TestRelease_NonHolderChangesNothing(t *testing.T) {
	dr := &fakeDrainer{}
	code, body := call(t, &fakeSlots{}, dr, worker, http.MethodPost, "/release")
	if released, ok := body["released"].(bool); code != http.StatusOK || !ok || released || dr.activated != "" {
		t.Fatalf("status = %d, body %v, activated %q", code, body, dr.activated)
	}
}

func TestRelease_ActivateFailureKeepsTheSlot(t *testing.T) {
	st := &fakeSlots{held: map[string]*slots.Slot{"whale-4": {Name: "reboot-slot-1", Holder: "whale-4"}}}
	dr := &fakeDrainer{activateErr: errors.New("node update: boom")}
	code, _ := call(t, st, dr, worker, http.MethodPost, "/release")
	if code != http.StatusInternalServerError || st.held["whale-4"] == nil {
		t.Fatalf("status = %d, slot %v", code, st.held["whale-4"])
	}
}

func TestStatus(t *testing.T) {
	code, body := call(t, &fakeSlots{}, &fakeDrainer{}, worker, http.MethodGet, "/status")
	if code != http.StatusOK || body["node"] != "whale-4" || body["availability"] != "active" || body["slot"] != nil {
		t.Fatalf("status = %d, body %v", code, body)
	}
	st := &fakeSlots{held: map[string]*slots.Slot{"whale-4": {Name: "reboot-slot-1", Holder: "whale-4"}}}
	_, body = call(t, st, &fakeDrainer{}, worker, http.MethodGet, "/status")
	if slotName(body) != "reboot-slot-1" {
		t.Fatalf("body = %v", body)
	}
}

func TestMethodNotAllowed(t *testing.T) {
	for method, path := range map[string]string{
		http.MethodGet:  "/acquire",
		http.MethodPut:  "/release",
		http.MethodPost: "/status",
	} {
		if code, _ := call(t, &fakeSlots{}, &fakeDrainer{}, worker, method, path); code != http.StatusMethodNotAllowed {
			t.Errorf("%s %s = %d, want 405", method, path, code)
		}
	}
}

func TestHealthz(t *testing.T) {
	if code, _ := call(t, &fakeSlots{}, &fakeDrainer{}, identity.Caller{}, http.MethodGet, "/healthz"); code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
}
