package metrics_test

import (
	"testing"

	"github.com/matchory/swarm-auto-drain/internal/metrics"
)

func TestNoopMetrics_AllInstrumentsPopulated(t *testing.T) {
	m := metrics.NewNoop()
	// All sub-struct instrument handles must be non-nil so domain code
	// can call .Add / .Record without nil-checks.
	checks := []struct {
		name string
		ok   bool
	}{
		{"Lock.Acquire", m.Lock.Acquire != nil},
		{"Lock.Release", m.Lock.Release != nil},
		{"Lock.Steals", m.Lock.Steals != nil},
		{"Lock.Hold", m.Lock.Hold != nil},
		{"Lock.Held", m.Lock.Held != nil},
		{"Drain.Total", m.Drain.Total != nil},
		{"Drain.Duration", m.Drain.Duration != nil},
		{"Drain.TasksRemaining", m.Drain.TasksRemaining != nil},
		{"Drain.QuorumCheck", m.Drain.QuorumCheck != nil},
		{"Role.Current", m.Role.Current != nil},
		{"Role.Refresh", m.Role.Refresh != nil},
		{"Role.Transitions", m.Role.Transitions != nil},
		{"Forward.Attempts", m.Forward.Attempts != nil},
		{"Forward.Duration", m.Forward.Duration != nil},
		{"Forward.CacheInvalidations", m.Forward.CacheInvalidations != nil},
		{"Identity.Resolutions", m.Identity.Resolutions != nil},
		{"HTTP.Requests", m.HTTP.Requests != nil},
		{"HTTP.Duration", m.HTTP.Duration != nil},
		{"Reactivate.Steps", m.Reactivate.Steps != nil},
	}
	for _, c := range checks {
		if !c.ok {
			t.Errorf("%s is nil", c.name)
		}
	}
}
