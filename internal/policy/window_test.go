package policy

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestParseWindow(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		wantZero  bool
		wantDays  []time.Weekday
		wantStart time.Duration
		wantEnd   time.Duration
		wantErr   bool
		errFrag   string
	}{
		{name: "empty", input: "", wantZero: true},
		{name: "whitespace", input: "   ", wantZero: true},
		{
			name:      "weekday range",
			input:     "Mon-Fri 02:00-06:00 UTC",
			wantDays:  []time.Weekday{time.Monday, time.Tuesday, time.Wednesday, time.Thursday, time.Friday},
			wantStart: 2 * time.Hour,
			wantEnd:   6 * time.Hour,
		},
		{
			name:      "weekday list",
			input:     "Sat,Sun 10:00-14:00 UTC",
			wantDays:  []time.Weekday{time.Saturday, time.Sunday},
			wantStart: 10 * time.Hour,
			wantEnd:   14 * time.Hour,
		},
		{
			name:      "case insensitive utc and days",
			input:     "mon-WED 09:30-17:45 utc",
			wantDays:  []time.Weekday{time.Monday, time.Tuesday, time.Wednesday},
			wantStart: 9*time.Hour + 30*time.Minute,
			wantEnd:   17*time.Hour + 45*time.Minute,
		},
		{
			name:      "wrap-around range",
			input:     "Fri-Mon 00:00-01:00 UTC",
			wantDays:  []time.Weekday{time.Friday, time.Saturday, time.Sunday, time.Monday},
			wantStart: 0,
			wantEnd:   time.Hour,
		},
		{
			name:      "single day no separators",
			input:     "Wed 00:00-23:59 UTC",
			wantDays:  []time.Weekday{time.Wednesday},
			wantStart: 0,
			wantEnd:   23*time.Hour + 59*time.Minute,
		},
		{name: "bad tz", input: "Mon 02:00-06:00 Europe/Berlin", wantErr: true, errFrag: "timezone"},
		{name: "missing tz", input: "Mon 02:00-06:00", wantErr: true, errFrag: "expected"},
		{name: "comma and dash", input: "Mon,Tue-Fri 02:00-06:00 UTC", wantErr: true, errFrag: "',' or '-'"},
		{name: "unknown weekday in list", input: "Foo,Sun 02:00-06:00 UTC", wantErr: true, errFrag: "unknown weekday"},
		{name: "unknown weekday in range", input: "Foo-Sun 02:00-06:00 UTC", wantErr: true, errFrag: "unknown weekday"},
		{name: "duplicate weekday", input: "Mon,Mon 02:00-06:00 UTC", wantErr: true, errFrag: "duplicate"},
		{name: "cross midnight equal", input: "Mon 02:00-02:00 UTC", wantErr: true, errFrag: "cross midnight"},
		{name: "cross midnight reverse", input: "Mon 22:00-02:00 UTC", wantErr: true, errFrag: "cross midnight"},
		{name: "bad time", input: "Mon 2:00-06:00 UTC", wantErr: true, errFrag: "HH:MM"},
		{name: "out of range hour", input: "Mon 25:00-26:00 UTC", wantErr: true, errFrag: "out of range"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseWindow(tc.input)
			if tc.wantErr {
				assertParseWindowErr(t, err, tc.errFrag)
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.wantZero {
				if !isZeroWindow(got) {
					t.Errorf("got %+v, want zero", got)
				}
				return
			}
			assertWindowFields(t, got, tc.wantDays, tc.wantStart, tc.wantEnd)
		})
	}
}

func assertParseWindowErr(t *testing.T, err error, errFrag string) {
	t.Helper()
	if err == nil {
		t.Fatal("want error, got nil")
	}
	if !errors.Is(err, ErrWindowParse) {
		t.Errorf("err = %v, want errors.Is(ErrWindowParse)", err)
	}
	if errFrag != "" && !strings.Contains(err.Error(), errFrag) {
		t.Errorf("err = %q, want fragment %q", err.Error(), errFrag)
	}
}

func assertWindowFields(t *testing.T, got Window, wantDays []time.Weekday, wantStart, wantEnd time.Duration) {
	t.Helper()
	if !sameWeekdays(got.Days, wantDays) {
		t.Errorf("Days = %v, want %v", got.Days, wantDays)
	}
	if got.Start != wantStart {
		t.Errorf("Start = %v, want %v", got.Start, wantStart)
	}
	if got.End != wantEnd {
		t.Errorf("End = %v, want %v", got.End, wantEnd)
	}
}

func TestParseWindow_CrossMidnightExactMessage(t *testing.T) {
	// Operators paste this verbatim into bug reports; pin the exact text.
	_, err := ParseWindow("Mon 22:00-02:00 UTC")
	if err == nil {
		t.Fatal("want error")
	}
	if !strings.Contains(err.Error(), crossMidnightMsg) {
		t.Errorf("err = %q, want it to contain %q", err.Error(), crossMidnightMsg)
	}
}

func TestInWindow(t *testing.T) {
	monday0300 := time.Date(2026, 5, 25, 3, 0, 0, 0, time.UTC) // Monday
	monday0700 := time.Date(2026, 5, 25, 7, 0, 0, 0, time.UTC) // Monday, after end
	saturday1200 := time.Date(2026, 5, 30, 12, 0, 0, 0, time.UTC)
	monday0600Exact := time.Date(2026, 5, 25, 6, 0, 0, 0, time.UTC) // boundary (exclusive end)
	monday0200Exact := time.Date(2026, 5, 25, 2, 0, 0, 0, time.UTC) // boundary (inclusive start)

	monFri02to06 := mustWindow(t, "Mon-Fri 02:00-06:00 UTC")
	satSun10to14 := mustWindow(t, "Sat,Sun 10:00-14:00 UTC")

	tests := []struct {
		name string
		w    Window
		now  time.Time
		want bool
	}{
		{name: "zero always in", w: Window{}, now: monday0300, want: true},
		{name: "weekday and time match", w: monFri02to06, now: monday0300, want: true},
		{name: "weekday match time before", w: monFri02to06, now: monday0700, want: false},
		{name: "weekday mismatch", w: monFri02to06, now: saturday1200, want: false},
		{name: "weekend match", w: satSun10to14, now: saturday1200, want: true},
		{name: "boundary end exclusive", w: monFri02to06, now: monday0600Exact, want: false},
		{name: "boundary start inclusive", w: monFri02to06, now: monday0200Exact, want: true},
		{
			name: "non-utc input converted",
			w:    monFri02to06,
			// 04:00 in a +02:00 zone == 02:00 UTC, which is inside.
			now:  time.Date(2026, 5, 25, 4, 0, 0, 0, time.FixedZone("CEST", 2*3600)),
			want: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := InWindow(tc.now, tc.w); got != tc.want {
				t.Errorf("InWindow = %v, want %v", got, tc.want)
			}
		})
	}
}

func mustWindow(t *testing.T, s string) Window {
	t.Helper()
	w, err := ParseWindow(s)
	if err != nil {
		t.Fatalf("ParseWindow(%q): %v", s, err)
	}
	return w
}

func sameWeekdays(a, b []time.Weekday) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
