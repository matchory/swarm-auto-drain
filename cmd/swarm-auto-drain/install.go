package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"

	"github.com/matchory/swarm-auto-drain/internal/hostinstall"
)

// installFn is the seam tests substitute to assert flag-parsing + opts
// plumbing without touching the filesystem.
var installFn = hostinstall.Install

func installCmd(args []string) int {
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	gid := fs.Int("gid", 0, "numeric GID for swarm-coord (required)")
	root := fs.String("root", "/", "install path prefix (for tests)")
	force := fs.Bool("force", false, "overwrite env file if present")
	userName := fs.String("user", "", "optional username to add to swarm-coord")
	if err := fs.Parse(args); err != nil {
		return exitConfigErr
	}
	if *gid == 0 {
		fmt.Fprintln(os.Stderr, "--gid is required")
		return exitConfigErr
	}
	if err := installFn(hostinstall.Options{
		Root:  *root,
		GID:   *gid,
		Force: *force,
		User:  *userName,
	}); err != nil {
		slog.Error("install", "err", err)
		return 1
	}
	return 0
}
