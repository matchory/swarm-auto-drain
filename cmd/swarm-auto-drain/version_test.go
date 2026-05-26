package main

import (
	"runtime"
	"strings"
	"testing"
)

func TestVersionCmd_PrintsBinaryAndGoVersion(t *testing.T) {
	// Override version at the package level for the duration of the test.
	saved := version
	version = "v1.2.3-test"
	defer func() { version = saved }()

	stdout := captureStdout(t, func() {
		code := versionCmd(nil)
		if code != 0 {
			t.Errorf("exit code = %d, want 0", code)
		}
	})

	if !strings.Contains(stdout, "v1.2.3-test") {
		t.Errorf("stdout = %q, want it to include the binary version", stdout)
	}
	if !strings.Contains(stdout, runtime.Version()) {
		t.Errorf("stdout = %q, want it to include the Go runtime version (for diagnostics)", stdout)
	}
}
