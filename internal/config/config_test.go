package config

import (
	"errors"
	"reflect"
	"testing"
	"time"
)

func required(t *testing.T) {
	t.Helper()
	t.Setenv("LISTEN_ADDR", "100.64.0.1:7777")
	t.Setenv("ALLOWED_TAGS", "tag:swarm-pipeline")
}

func TestLoad_Defaults(t *testing.T) {
	required(t)
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := Config{
		ListenAddr:          "100.64.0.1:7777",
		AllowedTags:         []string{"tag:swarm-pipeline"},
		SlotPrefix:          "reboot-slot",
		DockerSocket:        "/var/run/docker.sock",
		TailscaleSocket:     "/var/run/tailscale/tailscaled.sock",
		LogLevel:            "info",
		WorkerSlots:         1,
		DrainTimeout:        5 * time.Minute,
		MetricsPushInterval: 15 * time.Second,
	}
	if !reflect.DeepEqual(*c, want) {
		t.Fatalf("Load() = %+v, want %+v", *c, want)
	}
}

func TestLoad_Overrides(t *testing.T) {
	required(t)
	t.Setenv("ALLOWED_TAGS", " tag:swarm-pipeline , tag:swarm-app ,")
	t.Setenv("WORKER_SLOTS", "3")
	t.Setenv("SLOT_PREFIX", "test-slot")
	t.Setenv("DRAIN_TIMEOUT_SECONDS", "60")
	t.Setenv("METRICS_LISTEN_ADDR", ":9100")
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "collector:4318")
	t.Setenv("METRICS_PUSH_INTERVAL_SECONDS", "30")
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(c.AllowedTags) != 2 || c.AllowedTags[0] != "tag:swarm-pipeline" || c.AllowedTags[1] != "tag:swarm-app" {
		t.Errorf("AllowedTags = %q", c.AllowedTags)
	}
	if c.WorkerSlots != 3 || c.SlotPrefix != "test-slot" || c.DrainTimeout != time.Minute {
		t.Errorf("WorkerSlots=%d SlotPrefix=%q DrainTimeout=%v", c.WorkerSlots, c.SlotPrefix, c.DrainTimeout)
	}
	if c.MetricsListenAddr != ":9100" || c.OTLPEndpoint != "collector:4318" || c.MetricsPushInterval != 30*time.Second {
		t.Errorf("metrics = %q %q %v", c.MetricsListenAddr, c.OTLPEndpoint, c.MetricsPushInterval)
	}
}

func TestLoad_Rejects(t *testing.T) {
	cases := map[string]struct {
		key, value string
		want       error
	}{
		"no listen addr":     {"LISTEN_ADDR", "", ErrRequired},
		"wildcard listen":    {"LISTEN_ADDR", "0.0.0.0:7777", ErrWildcardListen},
		"empty-host listen":  {"LISTEN_ADDR", ":7777", ErrWildcardListen},
		"no tags":            {"ALLOWED_TAGS", " , ", ErrRequired},
		"untagged tag":       {"ALLOWED_TAGS", "swarm-pipeline", ErrInvalidTag},
		"zero worker slots":  {"WORKER_SLOTS", "0", ErrInvalidInt},
		"bad worker slots":   {"WORKER_SLOTS", "two", ErrInvalidInt},
		"bad drain timeout":  {"DRAIN_TIMEOUT_SECONDS", "soon", ErrInvalidInt},
		"zero drain timeout": {"DRAIN_TIMEOUT_SECONDS", "0", ErrInvalidInt},
		"bad metrics push":   {"METRICS_PUSH_INTERVAL_SECONDS", "x", ErrInvalidInt},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			required(t)
			t.Setenv(tc.key, tc.value)
			if _, err := Load(); !errors.Is(err, tc.want) {
				t.Fatalf("Load() err = %v, want %v", err, tc.want)
			}
		})
	}
}
