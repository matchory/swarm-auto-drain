package policy

import (
	"errors"
	"fmt"
	"os"
)

// ErrEmptyPath is returned when NeedsReboot is called with "". Catching
// this loudly (rather than treating empty as "file doesn't exist") makes
// a config typo surface as a runtime error instead of silently
// suppressing reboots forever.
var ErrEmptyPath = errors.New("sentinel path is empty")

// NeedsReboot reports whether the sentinel file at path exists. Stat errors
// other than NotExist are returned to the caller — silently swallowing a
// permission denied here would mask configuration bugs that operators need
// to see at policy-evaluation time, not the next reboot window later.
func NeedsReboot(path string) (bool, error) {
	if path == "" {
		return false, ErrEmptyPath
	}
	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, fmt.Errorf("stat sentinel %s: %w", path, err)
}
