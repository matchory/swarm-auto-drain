package policy

import "time"

// Config holds the inputs needed to make a reboot decision.
type Config struct {
	DisabledFile string
	SentinelFile string
	Window       Window
	BlackoutFile string
}

// Decision is the result of evaluating Config at a specific moment. Reason
// is a stable kebab-case identifier; downstream log/telemetry consumers
// switch on it, so the strings here are part of the package contract.
type Decision struct {
	Reboot bool
	Reason string
}

// Reason values returned by ShouldRebootNow. Stable; consumers depend on them.
const (
	ReasonDisabled         = "disabled"
	ReasonNoRebootRequired = "no-reboot-required"
	ReasonOutsideWindow    = "outside-window"
	ReasonInBlackout       = "in-blackout"
	ReasonOK               = "ok"
)

// ShouldRebootNow evaluates the policy gates in fixed order:
//  1. kill switch  → reason=disabled
//  2. sentinel     → reason=no-reboot-required (if absent)
//  3. window       → reason=outside-window
//  4. blackout     → reason=in-blackout
//  5. otherwise    → reason=ok, Reboot=true
//
// I/O errors from any check propagate as a third return value with a zero
// Decision — callers must not act on Decision when err != nil.
func ShouldRebootNow(cfg Config, now time.Time) (Decision, error) {
	if cfg.DisabledFile != "" {
		disabled, err := NeedsReboot(cfg.DisabledFile)
		if err != nil {
			return Decision{}, err
		}
		if disabled {
			return Decision{Reboot: false, Reason: ReasonDisabled}, nil
		}
	}

	needs, err := NeedsReboot(cfg.SentinelFile)
	if err != nil {
		return Decision{}, err
	}
	if !needs {
		return Decision{Reboot: false, Reason: ReasonNoRebootRequired}, nil
	}

	if !InWindow(now, cfg.Window) {
		return Decision{Reboot: false, Reason: ReasonOutsideWindow}, nil
	}

	if cfg.BlackoutFile != "" {
		ranges, err := ParseBlackoutFile(cfg.BlackoutFile)
		if err != nil {
			return Decision{}, err
		}
		if InBlackout(now, ranges) {
			return Decision{Reboot: false, Reason: ReasonInBlackout}, nil
		}
	}

	return Decision{Reboot: true, Reason: ReasonOK}, nil
}
