// Package main is the entry point for the swarm-auto-drain coordinator daemon.
//
// Implementation lives under internal/. See docs/specs/ for the design and
// docs/plans/ for the task-by-task implementation plan.
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "swarm-auto-drain: not yet implemented (see docs/plans/)")
	os.Exit(1)
}
