// Package config loads the coordinator's runtime configuration from environment
// variables. All knobs documented in docs/specs/.
package config

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

type Config struct {
	NodeHostname        string
	ListenAddr          string
	DockerSocket        string
	LockConfigName      string
	LockTTL             time.Duration
	DrainTimeout        time.Duration
	RoleRecheck         time.Duration
	ForwardTimeout      time.Duration
	ManagerDiscoveryTTL time.Duration
	LogLevel            string
}

// Load reads configuration from environment variables. NODE_HOSTNAME is
// required; everything else has a default documented in the spec.
func Load() (*Config, error) {
	c := &Config{
		NodeHostname:   os.Getenv("NODE_HOSTNAME"),
		ListenAddr:     envStr("LISTEN_ADDR", "0.0.0.0:7777"),
		DockerSocket:   envStr("DOCKER_SOCKET", "/var/run/docker.sock"),
		LockConfigName: envStr("LOCK_CONFIG_NAME", "swarm-auto-drain-lock"),
		LogLevel:       envStr("LOG_LEVEL", "info"),
	}
	if c.NodeHostname == "" {
		return nil, fmt.Errorf("NODE_HOSTNAME is required (set via swarm template: NODE_HOSTNAME={{.Node.Hostname}})")
	}

	var err error
	if c.LockTTL, err = envSecs("LOCK_TTL_SECONDS", 30*time.Minute); err != nil {
		return nil, err
	}
	if c.DrainTimeout, err = envSecs("DRAIN_TIMEOUT_SECONDS", 5*time.Minute); err != nil {
		return nil, err
	}
	if c.RoleRecheck, err = envSecs("ROLE_RECHECK_SECONDS", 30*time.Second); err != nil {
		return nil, err
	}
	if c.ForwardTimeout, err = envSecs("FORWARD_TIMEOUT_SECONDS", 60*time.Second); err != nil {
		return nil, err
	}
	if c.ManagerDiscoveryTTL, err = envSecs("MANAGER_DISCOVERY_TTL_SECONDS", 60*time.Second); err != nil {
		return nil, err
	}
	return c, nil
}

func envStr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envSecs(key string, def time.Duration) (time.Duration, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %q is not an integer", key, v)
	}
	return time.Duration(n) * time.Second, nil
}
