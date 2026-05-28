package hostinstall

import (
	"bytes"
	"errors"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// TestEnvTemplateUsesCanonicalSocketPath guards against the embedded env
// file's COORDINATOR_SOCKET value drifting from hostinstall.SocketPath, the
// single source of truth the daemon and host CLI also derive from.
func TestEnvTemplateUsesCanonicalSocketPath(t *testing.T) {
	data, err := embeddedFiles.ReadFile(filepath.Join(embedFilesRoot, "swarm-auto-drain.env"))
	if err != nil {
		t.Fatalf("read embedded env: %v", err)
	}
	want := "COORDINATOR_SOCKET=" + SocketPath
	if !strings.Contains(string(data), want) {
		t.Errorf("embedded env file missing %q (socket path drifted from hostinstall.SocketPath)", want)
	}
}

// fakeRunner records invocations and returns programmable per-call
// results. nil err → success with empty output. The mutex covers tests
// that might exercise concurrent calls in the future; current callers
// are single-threaded.
type fakeRunner struct {
	mu    sync.Mutex
	calls [][]string
	err   map[string]error
}

func newFakeRunner() *fakeRunner {
	return &fakeRunner{err: map[string]error{}}
}

func (f *fakeRunner) run(name string, args ...string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	call := append([]string{name}, args...)
	f.calls = append(f.calls, call)
	if e, ok := f.err[strings.Join(call, " ")]; ok {
		return "", e
	}
	return "", nil
}

func (f *fakeRunner) saw(prefix ...string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
outer:
	for _, c := range f.calls {
		if len(c) < len(prefix) {
			continue
		}
		for i, p := range prefix {
			if c[i] != p {
				continue outer
			}
		}
		return true
	}
	return false
}

func lookupReturning(gid int) GroupLookup {
	return func(name string) (*user.Group, error) {
		return &user.Group{Name: name, Gid: strconv.Itoa(gid)}, nil
	}
}

func lookupMissing() GroupLookup {
	return func(name string) (*user.Group, error) {
		return nil, user.UnknownGroupError(name)
	}
}

func baseOpts(t *testing.T) Options {
	t.Helper()
	return Options{
		Root:        t.TempDir(),
		GID:         8888,
		Runner:      newFakeRunner().run,
		LookupGroup: lookupReturning(8888),
		Out:         &bytes.Buffer{},
	}
}

func TestInstall_WritesUnits(t *testing.T) {
	opts := baseOpts(t)
	if err := Install(opts); err != nil {
		t.Fatalf("install: %v", err)
	}

	for _, name := range []string{UnitTriggerService, UnitTriggerTimer, UnitResumeService} {
		path := filepath.Join(opts.Root, SystemdDir, name)
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		want, err := embeddedFiles.ReadFile("files/" + name)
		if err != nil {
			t.Fatalf("read embedded %s: %v", name, err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s mismatch", name)
		}
	}

	envPath := filepath.Join(opts.Root, EnvFile)
	got, err := os.ReadFile(envPath)
	if err != nil {
		t.Fatalf("read env: %v", err)
	}
	want, _ := embeddedFiles.ReadFile("files/swarm-auto-drain.env")
	if !bytes.Equal(got, want) {
		t.Error("env content mismatch")
	}
}

func TestInstall_Idempotent(t *testing.T) {
	opts := baseOpts(t)
	if err := Install(opts); err != nil {
		t.Fatalf("install 1: %v", err)
	}
	snap1 := snapshotTree(t, opts.Root)
	if err := Install(opts); err != nil {
		t.Fatalf("install 2: %v", err)
	}
	snap2 := snapshotTree(t, opts.Root)
	if !equalSnapshots(snap1, snap2) {
		t.Error("second install changed filesystem state")
	}
}

func TestInstall_PreservesExistingEnvFile(t *testing.T) {
	opts := baseOpts(t)
	envPath := filepath.Join(opts.Root, EnvFile)
	if err := os.MkdirAll(filepath.Dir(envPath), 0o755); err != nil {
		t.Fatal(err)
	}
	original := []byte("# operator override\nCOORDINATOR_SOCKET=/custom\n")
	if err := os.WriteFile(envPath, original, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Install(opts); err != nil {
		t.Fatalf("install: %v", err)
	}
	got, _ := os.ReadFile(envPath)
	if !bytes.Equal(got, original) {
		t.Errorf("env file overwritten without --force\ngot:  %q\nwant: %q", got, original)
	}
}

func TestInstall_ForceOverwritesEnvFile(t *testing.T) {
	opts := baseOpts(t)
	opts.Force = true
	envPath := filepath.Join(opts.Root, EnvFile)
	if err := os.MkdirAll(filepath.Dir(envPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(envPath, []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Install(opts); err != nil {
		t.Fatalf("install: %v", err)
	}
	got, _ := os.ReadFile(envPath)
	want, _ := embeddedFiles.ReadFile("files/swarm-auto-drain.env")
	if !bytes.Equal(got, want) {
		t.Error("env file not overwritten with --force")
	}
}

func TestInstall_GroupGIDMismatch(t *testing.T) {
	opts := baseOpts(t)
	opts.GID = 8888
	opts.LookupGroup = lookupReturning(7777)
	err := Install(opts)
	if err == nil || !strings.Contains(err.Error(), "exists with gid") {
		t.Fatalf("want error containing 'exists with gid', got %v", err)
	}
}

func TestInstall_GroupCreated(t *testing.T) {
	opts := baseOpts(t)
	fr := newFakeRunner()
	opts.Runner = fr.run
	opts.LookupGroup = lookupMissing()
	if err := Install(opts); err != nil {
		t.Fatalf("install: %v", err)
	}
	if !fr.saw("groupadd", "-r", "-g", strconv.Itoa(opts.GID), CoordinatorGroup) {
		t.Errorf("expected groupadd call; calls: %v", fr.calls)
	}
}

func TestInstall_RunnerErrorPropagates(t *testing.T) {
	opts := baseOpts(t)
	fr := newFakeRunner()
	fr.err["groupadd -r -g 8888 "+CoordinatorGroup] = errors.New("boom")
	opts.Runner = fr.run
	opts.LookupGroup = lookupMissing()
	if err := Install(opts); err == nil {
		t.Fatal("want error from failing groupadd, got nil")
	}
}

func TestInstall_UserFlag_RunsUsermod(t *testing.T) {
	opts := baseOpts(t)
	fr := newFakeRunner()
	opts.Runner = fr.run
	opts.User = "alice"
	if err := Install(opts); err != nil {
		t.Fatalf("install: %v", err)
	}
	if !fr.saw("usermod", "-aG", CoordinatorGroup, "alice") {
		t.Errorf("expected usermod -aG %s alice; calls: %v", CoordinatorGroup, fr.calls)
	}
}

func TestUninstall_RemovesUnits(t *testing.T) {
	opts := baseOpts(t)
	if err := Install(opts); err != nil {
		t.Fatalf("install: %v", err)
	}
	if err := Uninstall(Options{Root: opts.Root, Runner: newFakeRunner().run, Out: &bytes.Buffer{}}); err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	for _, name := range []string{UnitTriggerService, UnitTriggerTimer, UnitResumeService} {
		path := filepath.Join(opts.Root, SystemdDir, name)
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("unit %s should be gone, got err=%v", name, err)
		}
	}
	envPath := filepath.Join(opts.Root, EnvFile)
	if _, err := os.Stat(envPath); err != nil {
		t.Errorf("env file should remain after uninstall, got err=%v", err)
	}
}

func TestUninstall_TolerantOfMissingFiles(t *testing.T) {
	root := t.TempDir()
	if err := Uninstall(Options{Root: root, Runner: newFakeRunner().run, Out: &bytes.Buffer{}}); err != nil {
		t.Fatalf("uninstall on fresh tree: %v", err)
	}
}

func TestInstall_Uninstall_RoundTrip(t *testing.T) {
	opts := baseOpts(t)
	if err := Install(opts); err != nil {
		t.Fatalf("install 1: %v", err)
	}
	snap1 := snapshotTree(t, opts.Root)

	if err := Uninstall(Options{Root: opts.Root, Runner: newFakeRunner().run, Out: &bytes.Buffer{}}); err != nil {
		t.Fatalf("uninstall: %v", err)
	}

	// Re-install with same opts must reproduce snap1.
	if err := Install(opts); err != nil {
		t.Fatalf("install 2: %v", err)
	}
	snap2 := snapshotTree(t, opts.Root)
	if !equalSnapshots(snap1, snap2) {
		t.Error("install→uninstall→install did not reproduce original state")
	}
}

func TestInstall_RequiresRootForRealRoot(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root")
	}
	err := Install(Options{Root: "/", GID: 8888, Runner: newFakeRunner().run, LookupGroup: lookupReturning(8888)})
	if err == nil || !strings.Contains(err.Error(), "must run as root") {
		t.Fatalf("want root requirement error, got %v", err)
	}
}

// snapshotTree returns a map of relative-path → content-hash-ish bytes
// under root. Used for byte-identical equality assertions across runs.
func snapshotTree(t *testing.T, root string) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	err := filepath.Walk(root, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if info.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		out[rel] = data
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func equalSnapshots(a, b map[string][]byte) bool {
	if len(a) != len(b) {
		return false
	}
	for k, va := range a {
		vb, ok := b[k]
		if !ok || !bytes.Equal(va, vb) {
			return false
		}
	}
	return true
}
