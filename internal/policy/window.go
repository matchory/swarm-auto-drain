// Package policy implements the host-side reboot policy primitives:
// recurring reboot windows, blackout date ranges, the reboot-required
// sentinel, and the top-level decision combiner.
package policy

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// Window is a recurring weekly time window during which reboots are allowed.
// The zero value (empty Days, Start == 0, End == 0) is treated as "always in
// window" so callers that don't configure a window default to permissive.
type Window struct {
	Days  []time.Weekday // empty == every day
	Start time.Duration  // since midnight UTC; -1 if no time range
	End   time.Duration  // since midnight UTC
}

// ErrWindowParse is returned for any syntactic problem parsing a window spec.
var ErrWindowParse = errors.New("invalid window spec")

// errWindowCrossesMidnight is referenced by tests via its exact message; we
// keep the message verbatim because operators paste it into bug reports.
const crossMidnightMsg = "window may not cross midnight; " +
	"use 'Sun 22:00-23:59,Mon 00:00-06:00' or shift to a same-day range"

var shortWeekdays = map[string]time.Weekday{
	"mon": time.Monday,
	"tue": time.Tuesday,
	"wed": time.Wednesday,
	"thu": time.Thursday,
	"fri": time.Friday,
	"sat": time.Saturday,
	"sun": time.Sunday,
}

// ParseWindow accepts the forms:
//
//	""                              → zero Window, always in-window
//	"Mon-Fri 02:00-06:00 UTC"       → weekday range + time range
//	"Sat,Sun 10:00-14:00 UTC"       → weekday list + time range
//
// The timezone token is required and must be UTC (case-insensitive); any
// other value is a parse error rather than a silent conversion, because
// drift between operator-local time and the daemon's UTC clock has burned
// operators before.
func ParseWindow(s string) (Window, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return Window{}, nil
	}

	parts := strings.Fields(s)
	if len(parts) != 3 {
		return Window{}, fmt.Errorf("%w: %q: expected 'DAYS HH:MM-HH:MM UTC'", ErrWindowParse, s)
	}
	daySpec, timeSpec, tz := parts[0], parts[1], parts[2]

	if !strings.EqualFold(tz, "UTC") {
		return Window{}, fmt.Errorf("%w: timezone must be UTC, got %q", ErrWindowParse, tz)
	}

	days, err := parseDays(daySpec)
	if err != nil {
		return Window{}, err
	}
	start, end, err := parseTimeRange(timeSpec)
	if err != nil {
		return Window{}, err
	}
	return Window{Days: days, Start: start, End: end}, nil
}

func parseDays(spec string) ([]time.Weekday, error) {
	hasComma := strings.Contains(spec, ",")
	hasDash := strings.Contains(spec, "-")
	if hasComma && hasDash {
		return nil, fmt.Errorf("%w: days may use ',' or '-' but not both: %q", ErrWindowParse, spec)
	}

	if hasDash {
		ends := strings.SplitN(spec, "-", 2)
		from, ok1 := shortWeekdays[strings.ToLower(ends[0])]
		to, ok2 := shortWeekdays[strings.ToLower(ends[1])]
		if !ok1 || !ok2 {
			return nil, fmt.Errorf("%w: unknown weekday in range %q", ErrWindowParse, spec)
		}
		// Range wraps: walk forward from `from` until we hit `to` inclusive.
		out := make([]time.Weekday, 0, 7)
		for d := from; ; d = (d + 1) % 7 {
			out = append(out, d)
			if d == to {
				break
			}
			if len(out) > 7 {
				// Defensive: shouldn't happen, but avoid infinite loop on logic bugs.
				return nil, fmt.Errorf("%w: weekday range loop", ErrWindowParse)
			}
		}
		return out, nil
	}

	// Comma list (or a single day with no comma).
	tokens := strings.Split(spec, ",")
	out := make([]time.Weekday, 0, len(tokens))
	seen := make(map[time.Weekday]struct{}, len(tokens))
	for _, t := range tokens {
		t = strings.TrimSpace(t)
		d, ok := shortWeekdays[strings.ToLower(t)]
		if !ok {
			return nil, fmt.Errorf("%w: unknown weekday %q", ErrWindowParse, t)
		}
		if _, dup := seen[d]; dup {
			return nil, fmt.Errorf("%w: duplicate weekday %q", ErrWindowParse, t)
		}
		seen[d] = struct{}{}
		out = append(out, d)
	}
	return out, nil
}

func parseTimeRange(spec string) (start, end time.Duration, err error) {
	ends := strings.SplitN(spec, "-", 2)
	if len(ends) != 2 {
		return 0, 0, fmt.Errorf("%w: time range must be HH:MM-HH:MM: %q", ErrWindowParse, spec)
	}
	start, err = parseHHMM(ends[0])
	if err != nil {
		return 0, 0, err
	}
	end, err = parseHHMM(ends[1])
	if err != nil {
		return 0, 0, err
	}
	if end <= start {
		return 0, 0, fmt.Errorf("%w: %s", ErrWindowParse, crossMidnightMsg)
	}
	return start, end, nil
}

func parseHHMM(s string) (time.Duration, error) {
	if len(s) != 5 || s[2] != ':' {
		return 0, fmt.Errorf("%w: time must be HH:MM, got %q", ErrWindowParse, s)
	}
	var h, m int
	if _, err := fmt.Sscanf(s, "%2d:%2d", &h, &m); err != nil {
		return 0, fmt.Errorf("%w: time %q: %w", ErrWindowParse, s, err)
	}
	if h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, fmt.Errorf("%w: time %q out of range", ErrWindowParse, s)
	}
	return time.Duration(h)*time.Hour + time.Duration(m)*time.Minute, nil
}

// InWindow reports whether now falls inside w. The zero value of Window is
// treated as "always in window" — see ParseWindow's empty-string contract.
func InWindow(now time.Time, w Window) bool {
	if isZeroWindow(w) {
		return true
	}
	now = now.UTC()

	if len(w.Days) > 0 && !slices.Contains(w.Days, now.Weekday()) {
		return false
	}

	offset := time.Duration(now.Hour())*time.Hour +
		time.Duration(now.Minute())*time.Minute +
		time.Duration(now.Second())*time.Second +
		time.Duration(now.Nanosecond())
	return offset >= w.Start && offset < w.End
}

func isZeroWindow(w Window) bool {
	return len(w.Days) == 0 && w.Start == 0 && w.End == 0
}
