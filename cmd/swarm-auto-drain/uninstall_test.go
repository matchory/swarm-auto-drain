package main

import (
	"errors"
	"testing"

	"github.com/matchory/swarm-auto-drain/internal/hostinstall"
)

func TestUninstallCmd_DefaultRoot(t *testing.T) {
	saved := uninstallFn
	defer func() { uninstallFn = saved }()
	var got hostinstall.Options
	uninstallFn = func(opts hostinstall.Options) error {
		got = opts
		return nil
	}
	code := uninstallCmd(nil)
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if got.Root != "/" {
		t.Errorf("root = %q, want /", got.Root)
	}
}

func TestUninstallCmd_PassesRoot(t *testing.T) {
	saved := uninstallFn
	defer func() { uninstallFn = saved }()
	var got hostinstall.Options
	uninstallFn = func(opts hostinstall.Options) error {
		got = opts
		return nil
	}
	code := uninstallCmd([]string{"--root", "/tmp/y"})
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if got.Root != "/tmp/y" {
		t.Errorf("root = %q, want /tmp/y", got.Root)
	}
}

func TestUninstallCmd_PropagatesError(t *testing.T) {
	saved := uninstallFn
	defer func() { uninstallFn = saved }()
	uninstallFn = func(_ hostinstall.Options) error {
		return errors.New("boom")
	}
	code := uninstallCmd(nil)
	if code != 1 {
		t.Errorf("exit = %d, want 1", code)
	}
}
