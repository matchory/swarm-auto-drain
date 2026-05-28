package config

import (
	"testing"
	"time"

	"github.com/matchory/swarm-auto-drain/internal/hostinstall"
)

// TestSocketPathSingleSourceOfTruth guards against the daemon's default Unix
// socket path drifting from the canonical hostinstall.SocketPath that the host
// CLI and the installed env file derive from.
func TestSocketPathSingleSourceOfTruth(t *testing.T) {
	if defaultUnixSocketPath != hostinstall.SocketPath {
		t.Errorf("defaultUnixSocketPath = %q, want %q (hostinstall.SocketPath)",
			defaultUnixSocketPath, hostinstall.SocketPath)
	}
}

func TestLoad_Defaults(t *testing.T) {
	t.Setenv("NODE_HOSTNAME", "test-node")

	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	// Group defaults into a table to keep cyclomatic complexity bounded —
	// adding a new env var only adds a table row, not another if/then.
	stringCases := []struct {
		field, got, want string
	}{
		{"NodeHostname", c.NodeHostname, "test-node"},
		{"ListenAddr", c.ListenAddr, "0.0.0.0:7777"},
		{"DockerSocket", c.DockerSocket, "/var/run/docker.sock"},
		{"LockConfigName", c.LockConfigName, "swarm-auto-drain-lock"},
		{"LogLevel", c.LogLevel, "info"},
		{"UnixSocketPath", c.UnixSocketPath, "/var/run/swarm-auto-drain/coord.sock"},
	}
	for _, tc := range stringCases {
		if tc.got != tc.want {
			t.Errorf("%s = %q, want %q", tc.field, tc.got, tc.want)
		}
	}
	durationCases := []struct {
		field string
		got   time.Duration
		want  time.Duration
	}{
		{"LockTTL", c.LockTTL, 30 * time.Minute},
		{"DrainTimeout", c.DrainTimeout, 5 * time.Minute},
		{"RoleRecheck", c.RoleRecheck, 30 * time.Second},
		{"ForwardTimeout", c.ForwardTimeout, 60 * time.Second},
		{"ManagerDiscoveryTTL", c.ManagerDiscoveryTTL, 60 * time.Second},
	}
	for _, tc := range durationCases {
		if tc.got != tc.want {
			t.Errorf("%s = %v, want %v", tc.field, tc.got, tc.want)
		}
	}
	if c.UnixSocketMode != 0o660 {
		t.Errorf("UnixSocketMode = %o, want 0660", c.UnixSocketMode)
	}
	if c.AllowUnencryptedOverlay {
		t.Error("AllowUnencryptedOverlay = true, want false (fail-closed default)")
	}
}

func TestLoad_UnixSocketOverrides(t *testing.T) {
	t.Setenv("NODE_HOSTNAME", "n")
	t.Setenv("UNIX_SOCKET_PATH", "/run/sad/coord.sock")
	t.Setenv("UNIX_SOCKET_MODE", "0640")
	t.Setenv("UNIX_SOCKET_GID", "9999")
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.UnixSocketPath != "/run/sad/coord.sock" {
		t.Errorf("UnixSocketPath = %q", c.UnixSocketPath)
	}
	if c.UnixSocketMode != 0o640 {
		t.Errorf("UnixSocketMode = %o, want 0640", c.UnixSocketMode)
	}
	if c.UnixSocketGID != 9999 {
		t.Errorf("UnixSocketGID = %d, want 9999", c.UnixSocketGID)
	}
}

func TestLoad_InvalidGID(t *testing.T) {
	t.Setenv("NODE_HOSTNAME", "n")
	t.Setenv("UNIX_SOCKET_GID", "not-a-number")
	if _, err := Load(); err == nil {
		t.Fatal("expected error on invalid UNIX_SOCKET_GID")
	}
}

func TestLoad_InvalidSocketMode(t *testing.T) {
	t.Setenv("NODE_HOSTNAME", "n")
	t.Setenv("UNIX_SOCKET_MODE", "not-a-mode")
	if _, err := Load(); err == nil {
		t.Fatal("expected error on invalid UNIX_SOCKET_MODE")
	}
}

func TestLoad_AllowUnencryptedOverlayParsing(t *testing.T) {
	// A typo'd "ture" must NOT enable the escape hatch — anything other
	// than recognized truthy values is treated as false.
	cases := []struct {
		val  string
		want bool
	}{
		{"", false},
		{"true", true},
		{"TRUE", true},
		{"1", true},
		{"yes", true},
		{"false", false},
		{"0", false},
		{"ture", false}, // Typo guard.
		{"on", false},   // We don't accept "on" — keep the set explicit.
	}
	for _, c := range cases {
		t.Run(c.val, func(t *testing.T) {
			t.Setenv("NODE_HOSTNAME", "n")
			t.Setenv("ALLOW_UNENCRYPTED_OVERLAY", c.val)
			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.AllowUnencryptedOverlay != c.want {
				t.Errorf("AllowUnencryptedOverlay = %v, want %v (env=%q)",
					cfg.AllowUnencryptedOverlay, c.want, c.val)
			}
		})
	}
}

func TestLoad_Overrides(t *testing.T) {
	t.Setenv("NODE_HOSTNAME", "override-node")
	t.Setenv("LISTEN_ADDR", "0.0.0.0:9999")
	t.Setenv("LOCK_TTL_SECONDS", "60")
	t.Setenv("LOG_LEVEL", "debug")

	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.ListenAddr != "0.0.0.0:9999" {
		t.Errorf("ListenAddr = %q", c.ListenAddr)
	}
	if c.LockTTL != 60*time.Second {
		t.Errorf("LockTTL = %v", c.LockTTL)
	}
	if c.LogLevel != "debug" {
		t.Errorf("LogLevel = %q", c.LogLevel)
	}
}

func TestLoad_MissingNodeHostname(t *testing.T) {
	t.Setenv("NODE_HOSTNAME", "")
	if _, err := Load(); err == nil {
		t.Fatal("expected error when NODE_HOSTNAME is empty")
	}
}

func TestLoad_BadInteger(t *testing.T) {
	t.Setenv("NODE_HOSTNAME", "n")
	t.Setenv("LOCK_TTL_SECONDS", "not-a-number")
	if _, err := Load(); err == nil {
		t.Fatal("expected error for non-numeric LOCK_TTL_SECONDS")
	}
}

func TestLoad_MetricsListenAddr_DefaultEmpty(t *testing.T) {
	t.Setenv("NODE_HOSTNAME", "node-a")
	t.Setenv("METRICS_LISTEN_ADDR", "")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.MetricsListenAddr != "" {
		t.Errorf("default should be empty, got %q", cfg.MetricsListenAddr)
	}
}

func TestLoad_MetricsListenAddr_FromEnv(t *testing.T) {
	t.Setenv("NODE_HOSTNAME", "node-a")
	t.Setenv("METRICS_LISTEN_ADDR", ":9100")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.MetricsListenAddr != ":9100" {
		t.Errorf("got %q want :9100", cfg.MetricsListenAddr)
	}
}

func TestLoad_OTLPEndpoint_FromEnv(t *testing.T) {
	t.Setenv("NODE_HOSTNAME", "node-a")
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "collector.observability:4318")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.OTLPEndpoint != "collector.observability:4318" {
		t.Errorf("got %q", cfg.OTLPEndpoint)
	}
}

func TestLoad_MetricsPushInterval_DefaultFifteen(t *testing.T) {
	t.Setenv("NODE_HOSTNAME", "node-a")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.MetricsPushInterval != 15*time.Second {
		t.Errorf("default should be 15s, got %v", cfg.MetricsPushInterval)
	}
}

func TestLoad_MetricsPushInterval_FromEnv(t *testing.T) {
	t.Setenv("NODE_HOSTNAME", "node-a")
	t.Setenv("METRICS_PUSH_INTERVAL_SECONDS", "30")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.MetricsPushInterval != 30*time.Second {
		t.Errorf("got %v want 30s", cfg.MetricsPushInterval)
	}
}
