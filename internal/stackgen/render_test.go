package stackgen

import (
	"bytes"
	"strings"
	"testing"
)

func TestRender_DefaultsProduceEncryptedOverlay(t *testing.T) {
	var buf bytes.Buffer
	if err := Render(&buf, Options{Version: "v0.2.0"}); err != nil {
		t.Fatalf("err = %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, `encrypted: "true"`) {
		t.Errorf("default render missing encrypted overlay:\n%s", out)
	}
	if !strings.Contains(out, "image: ghcr.io/matchory/swarm-auto-drain:v0.2.0") {
		t.Errorf("default render missing pinned image:\n%s", out)
	}
	if !strings.Contains(out, `UNIX_SOCKET_GID: "9999"`) {
		t.Errorf("default render missing default GID:\n%s", out)
	}
	if strings.Contains(out, "ALLOW_UNENCRYPTED_OVERLAY") {
		t.Errorf("default render leaked ALLOW_UNENCRYPTED_OVERLAY:\n%s", out)
	}
}

func TestRender_AllowUnencrypted(t *testing.T) {
	var buf bytes.Buffer
	if err := Render(&buf, Options{Version: "v0.2.0", AllowUnencrypted: true}); err != nil {
		t.Fatalf("err = %v", err)
	}
	out := buf.String()
	if strings.Contains(out, `encrypted: "true"`) {
		t.Errorf("AllowUnencrypted rendered an encrypted overlay:\n%s", out)
	}
	if !strings.Contains(out, `ALLOW_UNENCRYPTED_OVERLAY: "true"`) {
		t.Errorf("AllowUnencrypted didn't set the runtime env var:\n%s", out)
	}
}

func TestRender_CustomServiceName(t *testing.T) {
	var buf bytes.Buffer
	if err := Render(&buf, Options{ServiceName: "reboot-coord"}); err != nil {
		t.Fatalf("err = %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "  reboot-coord:") {
		t.Errorf("service name not applied:\n%s", out)
	}
	if !strings.Contains(out, "reboot-coord-net:") {
		t.Errorf("network name not applied:\n%s", out)
	}
}

func TestRender_CustomRegistry(t *testing.T) {
	var buf bytes.Buffer
	if err := Render(&buf, Options{ImageRegistry: "myreg.example.com/team"}); err != nil {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(buf.String(), "image: myreg.example.com/team/swarm-auto-drain:latest") {
		t.Errorf("registry not applied:\n%s", buf.String())
	}
}

func TestRender_InvalidServiceName(t *testing.T) {
	var buf bytes.Buffer
	if err := Render(&buf, Options{ServiceName: "bad.name.with.dots"}); err == nil {
		t.Error("expected error for non-DNS-safe service name")
	}
}

func TestRender_PreservesGoTemplateLiteral(t *testing.T) {
	// The stack template emits `{{.Node.Hostname}}` literally so swarm
	// substitutes the node hostname at task-creation time. A regression
	// here would silently break NODE_HOSTNAME on every replica.
	var buf bytes.Buffer
	if err := Render(&buf, Options{Version: "v0.2.0"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), `NODE_HOSTNAME: "{{.Node.Hostname}}"`) {
		t.Errorf("swarm template literal not preserved:\n%s", buf.String())
	}
}
