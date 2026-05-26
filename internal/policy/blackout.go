package policy

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

// Range is an inclusive [From, To] UTC date range. Times are always truncated
// to date precision (no hours/minutes).
type Range struct {
	From time.Time
	To   time.Time
}

// ErrBlackoutParse wraps any syntactic problem parsing a blackout file.
var ErrBlackoutParse = errors.New("invalid blackout entry")

const dateLayout = "2006-01-02"

// ParseBlackoutFile reads path and returns the parsed ranges. A missing file
// is not an error: callers treat the absence as "no blackouts configured" so
// operators can ship configuration before deciding to use it.
func ParseBlackoutFile(path string) ([]Range, error) {
	f, err := os.Open(path) //nolint:gosec // path is operator-supplied, intentional
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("open blackout file %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	var ranges []Range
	scanner := bufio.NewScanner(f)
	line := 0
	for scanner.Scan() {
		line++
		raw := strings.TrimSpace(scanner.Text())
		if raw == "" || strings.HasPrefix(raw, "#") {
			continue
		}
		r, err := parseBlackoutEntry(raw)
		if err != nil {
			return nil, fmt.Errorf("%w: line %d: %q: %v", ErrBlackoutParse, line, raw, err)
		}
		ranges = append(ranges, r)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read blackout file %s: %w", path, err)
	}
	return ranges, nil
}

func parseBlackoutEntry(s string) (Range, error) {
	parts := strings.SplitN(s, ":", 2)
	from, err := time.Parse(dateLayout, parts[0])
	if err != nil {
		return Range{}, err
	}
	if len(parts) == 1 {
		return Range{From: from, To: from}, nil
	}
	to, err := time.Parse(dateLayout, parts[1])
	if err != nil {
		return Range{}, err
	}
	if to.Before(from) {
		return Range{}, fmt.Errorf("range end %s before start %s", parts[1], parts[0])
	}
	return Range{From: from, To: to}, nil
}

// InBlackout reports whether the UTC date of now falls within any of ranges.
// Time-of-day components of now are ignored — blackouts are day-granular.
func InBlackout(now time.Time, ranges []Range) bool {
	d := truncateToDate(now.UTC())
	for _, r := range ranges {
		if !d.Before(r.From) && !d.After(r.To) {
			return true
		}
	}
	return false
}

func truncateToDate(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}
