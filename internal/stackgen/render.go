// Package stackgen renders the docker-stack template for the
// swarm-auto-drain coordinator service. The output is consumed as
//
//	swarm-auto-drain stack | docker stack deploy -c - swarm-auto-drain
//
// Defaults produce a working stack pinned to the binary's own version.
package stackgen

import (
	"bytes"
	"embed"
	"fmt"
	"io"
	"text/template"

	dockerpkg "github.com/matchory/swarm-auto-drain/internal/docker"
)

//go:embed files/stack.yml.tmpl
var embeddedFiles embed.FS

// Options controls the rendered stack. All fields except Out have sane
// defaults set by Render — pass a zero Options and you get a deployable
// stack pinned to the binary's version, the default registry, and the
// default service name with an encrypted overlay.
type Options struct {
	Version          string
	GID              int
	ImageRegistry    string
	ServiceName      string
	AllowUnencrypted bool
}

// Defaults applied when Options fields are zero values. Exported so the
// cmd-layer flag defaults can reference them.
const (
	DefaultImageRegistry = "ghcr.io/matchory"
	DefaultServiceName   = "swarm-auto-drain"
	DefaultGID           = 9999
)

// Render writes the stack YAML to w. Returns an error if the resulting
// stack would fail the runtime's own startup checks (encrypted overlay,
// valid DNS-safe service name).
func Render(w io.Writer, opts Options) error {
	if opts.Version == "" {
		opts.Version = "latest"
	}
	if opts.GID == 0 {
		opts.GID = DefaultGID
	}
	if opts.ImageRegistry == "" {
		opts.ImageRegistry = DefaultImageRegistry
	}
	if opts.ServiceName == "" {
		opts.ServiceName = DefaultServiceName
	}

	if err := dockerpkg.ValidateServiceName(opts.ServiceName); err != nil {
		return fmt.Errorf("service name: %w", err)
	}

	tmpl, err := template.New("stack").Parse(mustReadTemplate())
	if err != nil {
		return fmt.Errorf("parse template: %w", err)
	}
	var rendered bytes.Buffer
	if err := tmpl.Execute(&rendered, opts); err != nil {
		return fmt.Errorf("render template: %w", err)
	}

	// Defense-in-depth: when AllowUnencrypted is false, the rendered
	// output must contain `encrypted: "true"`. A template typo would
	// otherwise silently produce a plaintext-overlay stack that the
	// runtime would refuse to start against. Fail loud here.
	if !opts.AllowUnencrypted && !bytes.Contains(rendered.Bytes(), []byte(`encrypted: "true"`)) {
		return fmt.Errorf("rendered stack is missing `encrypted: \"true\"`; refusing")
	}

	_, err = w.Write(rendered.Bytes())
	return err
}

func mustReadTemplate() string {
	b, err := embeddedFiles.ReadFile("files/stack.yml.tmpl")
	if err != nil {
		panic(fmt.Sprintf("embedded stack template missing: %v", err))
	}
	return string(b)
}
