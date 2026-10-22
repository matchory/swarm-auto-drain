// Package main implements the swarm-auto-drain coordinator: a daemon run by
// systemd on each swarm manager (`run`), plus `version`.
package main

import (
	"fmt"
	"io"
	"os"
)

// version is overridden at link time via `-ldflags="-X main.version=..."`.
var version = "dev"

// subcommand is the signature every dispatchable command implements.
// Returning an int rather than an error lets each command decide its own
// exit code (config errors → 2, runtime errors → 1, etc.) without
// teaching main about every command's failure modes.
type subcommand func(args []string) int

// subcommands is the dispatch table for the binary's subcommands. It's
// a var rather than a const so tests can swap it for fakes — the
// dispatcher-level tests rely on this. Treat as immutable in production.
var subcommands = map[string]subcommand{
	"run":     runCmd,
	"version": versionCmd,
}

// minArgsForSubcommand is the argv length below which we treat the
// invocation as "no subcommand specified" and default to `run`. Argv[0]
// is the binary name, so anything < 2 means no subcommand was given.
const minArgsForSubcommand = 2

// exitUnknownSubcommand is the exit code for an unrecognized subcommand.
// Distinct from exitConfigErr (also 2) at the dispatch layer — same
// value, different semantics, no caller currently disambiguates.
const exitUnknownSubcommand = 2

func main() {
	os.Exit(dispatch(os.Args))
}

// dispatch routes argv[1] to the appropriate subcommand, defaulting to "run".
func dispatch(args []string) int {
	if len(args) < minArgsForSubcommand {
		return subcommands["run"](nil)
	}
	switch args[1] {
	case "-h", "--help", "help":
		printUsage(os.Stdout)
		return 0
	}
	cmd, ok := subcommands[args[1]]
	if !ok {
		fmt.Fprintf(os.Stderr, "unknown subcommand: %s\n", args[1])
		printUsage(os.Stderr)
		return exitUnknownSubcommand
	}
	return cmd(args[2:])
}

func printUsage(w io.Writer) {
	fmt.Fprint(w, `usage: swarm-auto-drain <subcommand> [args...]

subcommands:
  run        start the coordinator daemon (default if no subcommand given)
  version    print version and exit

See docs/specs/ for the full design.
`)
}
