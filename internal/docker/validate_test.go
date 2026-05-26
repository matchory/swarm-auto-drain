package docker

import (
	"errors"
	"os"
	"strings"
	"testing"
)

func TestValidateServiceName(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		wantErr bool
		wantIs  error
	}{
		{"empty", "", true, ErrServiceNameEmpty},
		{"plain", "swarm-auto-drain", false, nil},
		{"with-digits", "service123", false, nil},
		{"single-char", "x", false, nil},
		{"max-length", strings.Repeat("a", 63), false, nil},
		{"too-long", strings.Repeat("a", 64), true, ErrServiceNameInvalid},
		{"underscore", "swarm_auto_drain", true, ErrServiceNameInvalid},
		{"dot", "my.svc", true, ErrServiceNameInvalid},
		{"leading-hyphen", "-svc", true, ErrServiceNameInvalid},
		{"trailing-hyphen", "svc-", true, ErrServiceNameInvalid},
		{"slash", "svc/x", true, ErrServiceNameInvalid},
		{"uppercase-ok", "MyService", false, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := ValidateServiceName(c.input)
			if c.wantErr && err == nil {
				t.Fatalf("ValidateServiceName(%q) = nil, want error", c.input)
			}
			if !c.wantErr && err != nil {
				t.Fatalf("ValidateServiceName(%q) = %v, want nil", c.input, err)
			}
			if c.wantIs != nil && !errors.Is(err, c.wantIs) {
				t.Errorf("ValidateServiceName(%q) = %v, want errors.Is(_, %v)", c.input, err, c.wantIs)
			}
		})
	}
}

func TestCandidateContainerIDs_HostnameFirst(t *testing.T) {
	// Set $HOSTNAME and confirm it's returned first. Don't assert anything
	// about /proc/self/cgroup — it varies across CI environments.
	t.Setenv("HOSTNAME", "abc123def456")
	got := candidateContainerIDs()
	if len(got) == 0 || got[0] != "abc123def456" {
		t.Fatalf("candidates = %v, want HOSTNAME first", got)
	}
	// Dedup: HOSTNAME must not appear twice even if it also matches in cgroup.
	for i := 1; i < len(got); i++ {
		if got[i] == got[0] {
			t.Errorf("HOSTNAME appears at index 0 and %d", i)
		}
	}
}

func TestCandidateContainerIDs_EmptyHostnameStillTriesCgroup(t *testing.T) {
	t.Setenv("HOSTNAME", "")
	// We can't predict whether /proc/self/cgroup yields a 64-hex on this
	// test host; we just verify the function doesn't crash and tolerates
	// an empty $HOSTNAME by skipping it.
	got := candidateContainerIDs()
	for _, c := range got {
		if c == "" {
			t.Errorf("got empty string in candidates: %v", got)
		}
	}
}

func TestCandidateContainerIDs_NoCgroupFile(t *testing.T) {
	// On non-Linux platforms or hosts without cgroupfs, /proc/self/cgroup
	// won't exist. The function should still return $HOSTNAME (if set)
	// without error.
	t.Setenv("HOSTNAME", "container-id-from-env")
	got := candidateContainerIDs()
	if len(got) == 0 || got[0] != "container-id-from-env" {
		t.Errorf("candidates = %v, want [container-id-from-env, ...]", got)
	}
	// Sanity: if we're on a host without /proc/self/cgroup, length should be 1.
	if _, err := os.Stat("/proc/self/cgroup"); errors.Is(err, os.ErrNotExist) && len(got) != 1 {
		t.Errorf("no cgroupfs but got %d candidates: %v", len(got), got)
	}
}
