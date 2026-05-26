package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"

	"github.com/matchory/swarm-auto-drain/internal/stackgen"
)

// stackCmd renders the docker-stack YAML to stdout. Defaults produce a
// working stack pinned to the binary's own version; flags allow custom
// registries, service names, and a dev escape hatch for unencrypted
// overlays.
func stackCmd(args []string) int {
	fs := flag.NewFlagSet("stack", flag.ContinueOnError)
	versionFlag := fs.String("version", "", "image tag (default: this binary's version)")
	gid := fs.Int("gid", stackgen.DefaultGID, "UNIX_SOCKET_GID env var in the rendered service")
	registry := fs.String("image-registry", stackgen.DefaultImageRegistry, "image registry prefix")
	serviceName := fs.String("service-name", stackgen.DefaultServiceName, "stack/service name")
	allowUnencrypted := fs.Bool("allow-unencrypted", false, "render without `encrypted: true` (dev only)")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	tag := *versionFlag
	if tag == "" {
		tag = version
	}

	if err := stackgen.Render(os.Stdout, stackgen.Options{
		Version:          tag,
		GID:              *gid,
		ImageRegistry:    *registry,
		ServiceName:      *serviceName,
		AllowUnencrypted: *allowUnencrypted,
	}); err != nil {
		slog.Error("render stack", "err", err)
		fmt.Fprintln(os.Stderr, "render stack:", err)
		return 1
	}
	return 0
}
