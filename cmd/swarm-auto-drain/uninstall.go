package main

import (
	"flag"
	"log/slog"

	"github.com/matchory/swarm-auto-drain/internal/hostinstall"
)

var uninstallFn = hostinstall.Uninstall

func uninstallCmd(args []string) int {
	fs := flag.NewFlagSet("uninstall", flag.ContinueOnError)
	root := fs.String("root", "/", "install path prefix (for tests)")
	if err := fs.Parse(args); err != nil {
		return exitConfigErr
	}
	if err := uninstallFn(hostinstall.Options{Root: *root}); err != nil {
		slog.Error("uninstall", "err", err)
		return 1
	}
	return 0
}
