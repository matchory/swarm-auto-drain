package policy

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestNeedsReboot(t *testing.T) {
	dir := t.TempDir()
	present := filepath.Join(dir, "reboot-required")
	if err := os.WriteFile(present, []byte(""), 0o600); err != nil {
		t.Fatal(err)
	}
	absent := filepath.Join(dir, "nope")

	ok, err := NeedsReboot(present)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if !ok {
		t.Error("present file: NeedsReboot = false, want true")
	}

	ok, err = NeedsReboot(absent)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if ok {
		t.Error("absent file: NeedsReboot = true, want false")
	}
}

func TestNeedsReboot_StatError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permission semantics differ on windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permission checks")
	}
	// A path under an unreadable directory yields a stat error that is
	// neither nil nor ErrNotExist. NeedsReboot must surface it.
	dir := filepath.Join(t.TempDir(), "locked")
	if err := os.Mkdir(dir, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	_, err := NeedsReboot(filepath.Join(dir, "file"))
	if err == nil {
		t.Fatal("want error from unreadable directory, got nil")
	}
}
