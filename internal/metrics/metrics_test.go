package metrics_test

import (
	"testing"

	"github.com/matchory/swarm-auto-drain/internal/metrics"
)

func TestNoopMetrics_AllInstrumentsPopulated(t *testing.T) {
	m := metrics.NewNoop()
	// All sub-struct instrument handles must be non-nil so domain code
	// can call .Add / .Record without nil-checks.
	if m.Lock.Acquire == nil {
		t.Error("Lock.Acquire is nil")
	}
	if m.Lock.Release == nil {
		t.Error("Lock.Release is nil")
	}
	if m.Lock.Steals == nil {
		t.Error("Lock.Steals is nil")
	}
	if m.Lock.Hold == nil {
		t.Error("Lock.Hold is nil")
	}
	if m.Lock.Held == nil {
		t.Error("Lock.Held is nil")
	}
	if m.Drain.Total == nil {
		t.Error("Drain.Total is nil")
	}
	if m.Drain.Duration == nil {
		t.Error("Drain.Duration is nil")
	}
	if m.Drain.TasksRemaining == nil {
		t.Error("Drain.TasksRemaining is nil")
	}
	if m.Drain.QuorumCheck == nil {
		t.Error("Drain.QuorumCheck is nil")
	}
	if m.Role.Current == nil {
		t.Error("Role.Current is nil")
	}
	if m.Role.Refresh == nil {
		t.Error("Role.Refresh is nil")
	}
	if m.Role.Transitions == nil {
		t.Error("Role.Transitions is nil")
	}
	if m.Forward.Attempts == nil {
		t.Error("Forward.Attempts is nil")
	}
	if m.Forward.Duration == nil {
		t.Error("Forward.Duration is nil")
	}
	if m.Forward.CacheInvalidations == nil {
		t.Error("Forward.CacheInvalidations is nil")
	}
	if m.Identity.Resolutions == nil {
		t.Error("Identity.Resolutions is nil")
	}
	if m.HTTP.Requests == nil {
		t.Error("HTTP.Requests is nil")
	}
	if m.HTTP.Duration == nil {
		t.Error("HTTP.Duration is nil")
	}
}
