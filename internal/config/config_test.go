package config

import (
	"testing"
	"time"
)

func TestLoad_Defaults(t *testing.T) {
	t.Setenv("NODE_HOSTNAME", "test-node")

	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if c.NodeHostname != "test-node" {
		t.Errorf("NodeHostname = %q, want test-node", c.NodeHostname)
	}
	if c.ListenAddr != "127.0.0.1:7777" {
		t.Errorf("ListenAddr = %q, want 127.0.0.1:7777", c.ListenAddr)
	}
	if c.DockerSocket != "/var/run/docker.sock" {
		t.Errorf("DockerSocket = %q", c.DockerSocket)
	}
	if c.LockConfigName != "swarm-auto-drain-lock" {
		t.Errorf("LockConfigName = %q", c.LockConfigName)
	}
	if c.LockTTL != 30*time.Minute {
		t.Errorf("LockTTL = %v, want 30m", c.LockTTL)
	}
	if c.DrainTimeout != 5*time.Minute {
		t.Errorf("DrainTimeout = %v, want 5m", c.DrainTimeout)
	}
	if c.RoleRecheck != 30*time.Second {
		t.Errorf("RoleRecheck = %v, want 30s", c.RoleRecheck)
	}
	if c.ForwardTimeout != 60*time.Second {
		t.Errorf("ForwardTimeout = %v, want 60s", c.ForwardTimeout)
	}
	if c.ManagerDiscoveryTTL != 60*time.Second {
		t.Errorf("ManagerDiscoveryTTL = %v, want 60s", c.ManagerDiscoveryTTL)
	}
	if c.LogLevel != "info" {
		t.Errorf("LogLevel = %q", c.LogLevel)
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
