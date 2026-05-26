package policy

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseBlackoutFile_Missing(t *testing.T) {
	got, err := ParseBlackoutFile(filepath.Join(t.TempDir(), "nope.txt"))
	if err != nil {
		t.Fatalf("err = %v, want nil for missing file", err)
	}
	if got != nil {
		t.Errorf("got %v, want nil", got)
	}
}

func TestParseBlackoutFile_HappyPath(t *testing.T) {
	contents := `# Holiday blackouts
2026-12-24:2026-12-26

# A single day
2026-01-01

# trailing comment
2026-07-04
`
	path := writeTemp(t, contents)
	got, err := ParseBlackoutFile(path)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	want := []Range{
		{From: mustDate(t, "2026-12-24"), To: mustDate(t, "2026-12-26")},
		{From: mustDate(t, "2026-01-01"), To: mustDate(t, "2026-01-01")},
		{From: mustDate(t, "2026-07-04"), To: mustDate(t, "2026-07-04")},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d ranges, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if !got[i].From.Equal(want[i].From) || !got[i].To.Equal(want[i].To) {
			t.Errorf("range[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestParseBlackoutFile_Errors(t *testing.T) {
	tests := []struct {
		name    string
		content string
		errFrag string
	}{
		{name: "bad date", content: "2026-13-99\n", errFrag: "line 1"},
		{name: "reversed range", content: "2026-12-26:2026-12-24\n", errFrag: "before start"},
		{name: "bad to date", content: "2026-12-24:not-a-date\n", errFrag: "line 1"},
		{name: "second line bad", content: "2026-01-01\nnope\n", errFrag: "line 2"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := writeTemp(t, tc.content)
			_, err := ParseBlackoutFile(path)
			if err == nil {
				t.Fatal("want error, got nil")
			}
			if !errors.Is(err, ErrBlackoutParse) {
				t.Errorf("err = %v, want errors.Is(ErrBlackoutParse)", err)
			}
			if !strings.Contains(err.Error(), tc.errFrag) {
				t.Errorf("err = %q, want fragment %q", err.Error(), tc.errFrag)
			}
		})
	}
}

func TestInBlackout(t *testing.T) {
	ranges := []Range{
		{From: mustDate(t, "2026-12-24"), To: mustDate(t, "2026-12-26")},
		{From: mustDate(t, "2026-01-01"), To: mustDate(t, "2026-01-01")},
	}

	tests := []struct {
		name string
		now  time.Time
		want bool
	}{
		{name: "inside range start", now: mustDate(t, "2026-12-24"), want: true},
		{name: "inside range end", now: mustDate(t, "2026-12-26"), want: true},
		{name: "inside range middle", now: mustDate(t, "2026-12-25"), want: true},
		{name: "single day match", now: mustDate(t, "2026-01-01"), want: true},
		{name: "outside before", now: mustDate(t, "2026-12-23"), want: false},
		{name: "outside after", now: mustDate(t, "2026-12-27"), want: false},
		{
			name: "time of day ignored",
			now:  time.Date(2026, 12, 25, 23, 59, 59, 0, time.UTC),
			want: true,
		},
		{
			name: "non-utc time on boundary",
			// 2026-12-24 01:00 UTC+02:00 == 2026-12-23 23:00 UTC -> outside.
			now:  time.Date(2026, 12, 24, 1, 0, 0, 0, time.FixedZone("CEST", 2*3600)),
			want: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := InBlackout(tc.now, ranges); got != tc.want {
				t.Errorf("InBlackout = %v, want %v", got, tc.want)
			}
		})
	}

	if InBlackout(time.Now(), nil) {
		t.Error("InBlackout with nil ranges must be false")
	}
}

func mustDate(t *testing.T, s string) time.Time {
	t.Helper()
	d, err := time.Parse(dateLayout, s)
	if err != nil {
		t.Fatalf("parse date %q: %v", s, err)
	}
	return d
}

func writeTemp(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "blackout.txt")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
