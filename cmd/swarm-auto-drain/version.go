package main

import (
	"fmt"
	"os"
	"runtime"
)

// versionCmd prints the binary's version and Go runtime version, then
// exits 0. Useful for diagnostics and as the simplest possible
// invocation that exercises the subcommand dispatcher.
func versionCmd(_ []string) int {
	fmt.Fprintf(os.Stdout, "swarm-auto-drain %s (built with %s on %s/%s)\n",
		version, runtime.Version(), runtime.GOOS, runtime.GOARCH)
	return 0
}
