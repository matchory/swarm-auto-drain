package main

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"
)

// recordedCall captures one invocation of a subcommand so tests can
// assert which subcommand the dispatcher selected and with what args.
type recordedCall struct {
	name string
	args []string
}

// installFakes swaps the dispatcher's subcommand registry with fakes that
// record their invocation. Returns the slice of recorded calls (appended
// to in-place by the fakes) and a cleanup function that restores the
// original registry.
func installFakes(t *testing.T) (*[]recordedCall, func()) {
	t.Helper()
	calls := &[]recordedCall{}
	saved := subcommands
	recordCall := func(name string) subcommand {
		return func(args []string) int {
			*calls = append(*calls, recordedCall{name: name, args: args})
			return 0
		}
	}
	subcommands = map[string]subcommand{
		"run":     recordCall("run"),
		"version": recordCall("version"),
	}
	return calls, func() { subcommands = saved }
}

func TestDispatch_NoArgsDefaultsToRun(t *testing.T) {
	calls, restore := installFakes(t)
	defer restore()

	code := dispatch([]string{"swarm-auto-drain"})

	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	if len(*calls) != 1 || (*calls)[0].name != "run" {
		t.Errorf("calls = %+v, want one run call", *calls)
	}
}

func TestDispatch_ExplicitRunSubcommand(t *testing.T) {
	calls, restore := installFakes(t)
	defer restore()

	code := dispatch([]string{"swarm-auto-drain", "run"})

	if code != 0 {
		t.Errorf("exit code = %d", code)
	}
	if len(*calls) != 1 || (*calls)[0].name != "run" {
		t.Errorf("calls = %+v, want one run call", *calls)
	}
}

func TestDispatch_VersionSubcommand(t *testing.T) {
	calls, restore := installFakes(t)
	defer restore()

	code := dispatch([]string{"swarm-auto-drain", "version"})

	if code != 0 {
		t.Errorf("exit code = %d", code)
	}
	if len(*calls) != 1 || (*calls)[0].name != "version" {
		t.Errorf("calls = %+v, want one version call", *calls)
	}
}

func TestDispatch_PassesTrailingArgsToSubcommand(t *testing.T) {
	// E.g. `swarm-auto-drain run --foo bar` — the dispatcher peels
	// the binary name and subcommand off, hands the rest to the subcommand.
	calls, restore := installFakes(t)
	defer restore()

	dispatch([]string{"swarm-auto-drain", "run", "--foo", "bar"})

	if len(*calls) != 1 {
		t.Fatalf("calls = %+v", *calls)
	}
	got := (*calls)[0].args
	want := []string{"--foo", "bar"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("args = %v, want %v", got, want)
	}
}

func TestDispatch_UnknownSubcommandExits2(t *testing.T) {
	_, restore := installFakes(t)
	defer restore()
	stderr := captureStderr(t, func() {
		code := dispatch([]string{"swarm-auto-drain", "nope"})
		if code != 2 {
			t.Errorf("exit code = %d, want 2", code)
		}
	})
	if !strings.Contains(stderr, "nope") {
		t.Errorf("stderr = %q, want it to name the unknown subcommand", stderr)
	}
	if !strings.Contains(stderr, "usage") {
		t.Errorf("stderr = %q, want usage hint", stderr)
	}
}

func TestDispatch_HelpFlagPrintsUsage(t *testing.T) {
	// `-h` and `--help` are conventional and shouldn't crash. They print
	// usage to stdout and exit 0.
	_, restore := installFakes(t)
	defer restore()
	for _, flag := range []string{"-h", "--help", "help"} {
		t.Run(flag, func(t *testing.T) {
			stdout := captureStdout(t, func() {
				code := dispatch([]string{"swarm-auto-drain", flag})
				if code != 0 {
					t.Errorf("exit code = %d, want 0", code)
				}
			})
			if !strings.Contains(stdout, "usage") {
				t.Errorf("stdout = %q, want usage info", stdout)
			}
		})
	}
}

// captureStdout / captureStderr swap os.Stdout/Stderr for the duration of
// fn and return everything that was written. Standard pattern for
// asserting on programs that write to standard fds directly.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	return capture(t, &os.Stdout, fn)
}

func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	return capture(t, &os.Stderr, fn)
}

func capture(t *testing.T, target **os.File, fn func()) string {
	t.Helper()
	saved := *target
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	*target = w
	done := make(chan string)
	go func() {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, r)
		done <- buf.String()
	}()
	fn()
	_ = w.Close()
	*target = saved
	return <-done
}
