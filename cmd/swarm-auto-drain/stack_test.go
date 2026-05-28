package main

import (
	"strings"
	"testing"

	"github.com/matchory/swarm-auto-drain/internal/docker"
	"github.com/matchory/swarm-auto-drain/internal/stackgen"
)

// TestStackCmd_DefaultsCrossValidate renders with the cmd's defaults and
// pipes the result through the runtime's own startup checks. If the
// runtime tightens a check, this test fails and forces the template to
// match.
func TestStackCmd_DefaultsCrossValidate(t *testing.T) {
	var buf strings.Builder
	if err := stackgen.Render(&buf, stackgen.Options{Version: "v0.2.0"}); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, `encrypted: "true"`) {
		t.Error("default render missing encrypted overlay")
	}
	if err := docker.ValidateServiceName(stackgen.DefaultServiceName); err != nil {
		t.Errorf("default service name fails runtime validation: %v", err)
	}
}

func TestStackCmd_BadServiceName(t *testing.T) {
	rc := stackCmd([]string{"--service-name", "bad.dot.name"})
	if rc != 1 {
		t.Errorf("exit = %d, want 1", rc)
	}
}

func TestStackCmd_BadFlag(t *testing.T) {
	rc := stackCmd([]string{"--nope"})
	if rc != 2 {
		t.Errorf("exit = %d, want 2", rc)
	}
}
