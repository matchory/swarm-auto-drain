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
		{"Slots.Acquire", m.Slots.Acquire != nil},
		{"Slots.Release", m.Slots.Release != nil},
		{"Slots.Hold", m.Slots.Hold != nil},
		{"Drain.Total", m.Drain.Total != nil},
		{"Drain.Duration", m.Drain.Duration != nil},
		{"Drain.TasksRemaining", m.Drain.TasksRemaining != nil},
		{"Drain.QuorumCheck", m.Drain.QuorumCheck != nil},
		{"Identity.Resolutions", m.Identity.Resolutions != nil},
		{"HTTP.Requests", m.HTTP.Requests != nil},
		{"HTTP.Duration", m.HTTP.Duration != nil},
	}
	for _, c := range checks {
		if !c.ok {
			t.Errorf("%s is nil", c.name)
		}
	}
}
