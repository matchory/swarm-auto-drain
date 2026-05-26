package policy

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestShouldRebootNow(t *testing.T) {
	dir := t.TempDir()
	touch := func(name string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(""), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	missing := func(name string) string {
		return filepath.Join(dir, name)
	}

	sentinel := touch("reboot-required")
	disabledPresent := touch("disabled")
	disabledAbsent := filepath.Join(dir, "disabled-absent")
	blackoutFile := filepath.Join(dir, "blackouts.txt")
	if err := os.WriteFile(blackoutFile, []byte("2026-05-25\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	mondayInWindow := time.Date(2026, 5, 25, 3, 0, 0, 0, time.UTC)
	mondayOutOfWindow := time.Date(2026, 5, 25, 12, 0, 0, 0, time.UTC)
	tuesday := time.Date(2026, 5, 26, 3, 0, 0, 0, time.UTC)
	monFri := mustWindow(t, "Mon-Fri 02:00-06:00 UTC")

	tests := []struct {
		name       string
		cfg        Config
		now        time.Time
		wantReboot bool
		wantReason string
	}{
		{
			name:       "disabled wins over all others",
			cfg:        Config{DisabledFile: disabledPresent, SentinelFile: sentinel, Window: monFri, BlackoutFile: blackoutFile},
			now:        mondayInWindow,
			wantReboot: false,
			wantReason: ReasonDisabled,
		},
		{
			name:       "no sentinel wins after disabled",
			cfg:        Config{DisabledFile: disabledAbsent, SentinelFile: missing("nope"), Window: monFri, BlackoutFile: blackoutFile},
			now:        mondayInWindow,
			wantReboot: false,
			wantReason: ReasonNoRebootRequired,
		},
		{
			name:       "outside window",
			cfg:        Config{DisabledFile: disabledAbsent, SentinelFile: sentinel, Window: monFri, BlackoutFile: missing("bo")},
			now:        mondayOutOfWindow,
			wantReboot: false,
			wantReason: ReasonOutsideWindow,
		},
		{
			name:       "blackout date",
			cfg:        Config{DisabledFile: disabledAbsent, SentinelFile: sentinel, Window: monFri, BlackoutFile: blackoutFile},
			now:        mondayInWindow,
			wantReboot: false,
			wantReason: ReasonInBlackout,
		},
		{
			name:       "ok when all gates pass",
			cfg:        Config{DisabledFile: disabledAbsent, SentinelFile: sentinel, Window: monFri, BlackoutFile: blackoutFile},
			now:        tuesday,
			wantReboot: true,
			wantReason: ReasonOK,
		},
		{
			name:       "empty disabled path skips kill switch",
			cfg:        Config{DisabledFile: "", SentinelFile: sentinel, Window: monFri, BlackoutFile: missing("bo")},
			now:        mondayInWindow,
			wantReboot: true,
			wantReason: ReasonOK,
		},
		{
			name:       "empty blackout path skips blackout check",
			cfg:        Config{DisabledFile: disabledAbsent, SentinelFile: sentinel, Window: monFri, BlackoutFile: ""},
			now:        mondayInWindow,
			wantReboot: true,
			wantReason: ReasonOK,
		},
		{
			name:       "zero window admits any time",
			cfg:        Config{DisabledFile: disabledAbsent, SentinelFile: sentinel, Window: Window{}, BlackoutFile: missing("bo")},
			now:        mondayOutOfWindow,
			wantReboot: true,
			wantReason: ReasonOK,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d, err := ShouldRebootNow(tc.cfg, tc.now)
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			if d.Reboot != tc.wantReboot {
				t.Errorf("Reboot = %v, want %v (reason=%q)", d.Reboot, tc.wantReboot, d.Reason)
			}
			if d.Reason != tc.wantReason {
				t.Errorf("Reason = %q, want %q", d.Reason, tc.wantReason)
			}
		})
	}
}

func TestShouldRebootNow_BlackoutParseError(t *testing.T) {
	dir := t.TempDir()
	sentinel := filepath.Join(dir, "sentinel")
	if err := os.WriteFile(sentinel, []byte(""), 0o600); err != nil {
		t.Fatal(err)
	}
	bo := filepath.Join(dir, "bo")
	if err := os.WriteFile(bo, []byte("garbage\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := Config{
		SentinelFile: sentinel,
		Window:       Window{},
		BlackoutFile: bo,
	}
	d, err := ShouldRebootNow(cfg, time.Now())
	if err == nil {
		t.Fatalf("want error, got Decision=%+v", d)
	}
	if d.Reboot || d.Reason != "" {
		t.Errorf("on error, Decision must be zero, got %+v", d)
	}
}
