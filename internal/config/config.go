// Package config loads the coordinator's configuration from environment
// variables, documented in docs/specs/2026-10-22-v2-host-coordinator-design.md.
package config

import (
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	defaultDrainTimeout        = 5 * time.Minute
	defaultMetricsPushInterval = 15 * time.Second
)

var (
	// ErrRequired means a required variable is unset or empty.
	ErrRequired = errors.New("required")
	// ErrInvalidInt means a variable that must hold a positive integer does not.
	ErrInvalidInt = errors.New("not a positive integer")
	// ErrWildcardListen means LISTEN_ADDR would listen on every interface.
	// The coordinator listens on the tailnet address only.
	ErrWildcardListen = errors.New("must name one address, e.g. the tailnet IP")
	// ErrInvalidTag means an ALLOWED_TAGS entry is not a "tag:" name.
	ErrInvalidTag = errors.New(`must start with "tag:"`)
)

// Config holds runtime configuration loaded from environment variables.
// Field order groups pointer-carrying fields first for a compact GC scan.
type Config struct {
	ListenAddr          string
	SlotPrefix          string
	DockerSocket        string
	TailscaleSocket     string
	LogLevel            string
	MetricsListenAddr   string // empty disables the Prometheus listener
	OTLPEndpoint        string // empty disables OTLP push
	AllowedTags         []string
	WorkerSlots         int
	DrainTimeout        time.Duration
	MetricsPushInterval time.Duration
}

// Load reads configuration from environment variables.
func Load() (*Config, error) {
	listen, err := listenAddr()
	if err != nil {
		return nil, err
	}
	tags, err := allowedTags()
	if err != nil {
		return nil, err
	}

	workerSlots, err := envPositive("WORKER_SLOTS", 1)
	if err != nil {
		return nil, err
	}
	drainSecs, err := envPositive("DRAIN_TIMEOUT_SECONDS", int(defaultDrainTimeout.Seconds()))
	if err != nil {
		return nil, err
	}
	pushSecs, err := envPositive("METRICS_PUSH_INTERVAL_SECONDS", int(defaultMetricsPushInterval.Seconds()))
	if err != nil {
		return nil, err
	}

	return &Config{
		ListenAddr:          listen,
		SlotPrefix:          envStr("SLOT_PREFIX", "reboot-slot"),
		DockerSocket:        envStr("DOCKER_SOCKET", "/var/run/docker.sock"),
		TailscaleSocket:     envStr("TAILSCALE_SOCKET", "/var/run/tailscale/tailscaled.sock"),
		LogLevel:            envStr("LOG_LEVEL", "info"),
		MetricsListenAddr:   os.Getenv("METRICS_LISTEN_ADDR"),
		OTLPEndpoint:        os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"),
		AllowedTags:         tags,
		WorkerSlots:         workerSlots,
		DrainTimeout:        time.Duration(drainSecs) * time.Second,
		MetricsPushInterval: time.Duration(pushSecs) * time.Second,
	}, nil
}

// listenAddr reads LISTEN_ADDR, which must name one address.
func listenAddr() (string, error) {
	listen := os.Getenv("LISTEN_ADDR")
	if listen == "" {
		return "", fmt.Errorf("LISTEN_ADDR: %w", ErrRequired)
	}
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		return "", fmt.Errorf("LISTEN_ADDR: %q: %w", listen, err)
	}
	if ip := net.ParseIP(host); host == "" || (ip != nil && ip.IsUnspecified()) {
		return "", fmt.Errorf("LISTEN_ADDR: %q: %w", listen, ErrWildcardListen)
	}
	return listen, nil
}

// allowedTags reads ALLOWED_TAGS: comma-separated, at least one, each "tag:…".
func allowedTags() ([]string, error) {
	var tags []string
	for tag := range strings.SplitSeq(os.Getenv("ALLOWED_TAGS"), ",") {
		tag = strings.TrimSpace(tag)
		if tag == "" {
			continue
		}
		if !strings.HasPrefix(tag, "tag:") {
			return nil, fmt.Errorf("ALLOWED_TAGS: %q: %w", tag, ErrInvalidTag)
		}
		tags = append(tags, tag)
	}
	if len(tags) == 0 {
		return nil, fmt.Errorf("ALLOWED_TAGS: %w", ErrRequired)
	}
	return tags, nil
}

func envStr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// envPositive parses a positive decimal integer, def when unset.
func envPositive(key string, def int) (int, error) {
	raw := os.Getenv(key)
	if raw == "" {
		return def, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 {
		return 0, fmt.Errorf("%s: %q: %w", key, raw, ErrInvalidInt)
	}
	return n, nil
}
