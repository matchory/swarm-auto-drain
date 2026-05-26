package main

import (
	"errors"
	"testing"

	"github.com/matchory/swarm-auto-drain/internal/hostinstall"
)

func TestInstallCmd_RequiresGID(t *testing.T) {
	saved := installFn
	defer func() { installFn = saved }()
	called := false
	installFn = func(_ hostinstall.Options) error {
		called = true
		return nil
	}
	code := installCmd(nil)
	if code != exitConfigErr {
		t.Errorf("exit code = %d, want %d", code, exitConfigErr)
	}
	if called {
		t.Errorf("installFn called despite missing --gid")
	}
}

func TestInstallCmd_PassesFlagsThrough(t *testing.T) {
	saved := installFn
	defer func() { installFn = saved }()
	var got hostinstall.Options
	installFn = func(opts hostinstall.Options) error {
		got = opts
		return nil
	}
	code := installCmd([]string{"--gid", "8888", "--root", "/tmp/x", "--force", "--user", "alice"})
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if got.GID != 8888 || got.Root != "/tmp/x" || !got.Force || got.User != "alice" {
		t.Errorf("opts plumbed wrong: %+v", got)
	}
}

func TestInstallCmd_PropagatesError(t *testing.T) {
	saved := installFn
	defer func() { installFn = saved }()
	installFn = func(_ hostinstall.Options) error {
		return errors.New("boom")
	}
	code := installCmd([]string{"--gid", "8888"})
	if code != 1 {
		t.Errorf("exit = %d, want 1", code)
	}
}
