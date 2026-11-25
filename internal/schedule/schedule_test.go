package schedule

import (
	"testing"
	"time"
)

func mustParse(t *testing.T, spec string) Schedule {
	t.Helper()
	s, err := Parse(spec)
	if err != nil {
		t.Fatalf("Parse(%q): %v", spec, err)
	}
	if s.IsZero() {
		t.Fatalf("Parse(%q) returned the zero schedule", spec)
	}
	return s
}

func TestEverySchedule(t *testing.T) {
	from := time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC)
	s := mustParse(t, "@every 30s")
	if got, want := s.Next(from), from.Add(30*time.Second); !got.Equal(want) {
		t.Errorf("Next() = %v, want %v", got, want)
	}
	if got := s.String(); got != "@every 30s" {
		t.Errorf("String() = %q, want the original spec", got)
	}
}

func TestCronSchedules(t *testing.T) {
	cases := []struct {
		spec string
		from time.Time
		want time.Time
	}{
		{
			spec: "30 4 * * *",
			from: time.Date(2026, 11, 25, 4, 29, 59, 0, time.UTC),
			want: time.Date(2026, 11, 25, 4, 30, 0, 0, time.UTC),
		},
		{
			spec: "@hourly",
			from: time.Date(2026, 11, 25, 10, 15, 30, 0, time.UTC),
			want: time.Date(2026, 11, 25, 11, 0, 0, 0, time.UTC),
		},
		{
			spec: "@daily",
			from: time.Date(2026, 11, 25, 10, 15, 30, 0, time.UTC),
			want: time.Date(2026, 11, 26, 0, 0, 0, 0, time.UTC),
		},
		{
			spec: "0 3 * * MON",
			from: time.Date(2026, 11, 25, 3, 1, 0, 0, time.UTC), // Wednesday
			want: time.Date(2026, 11, 30, 3, 0, 0, 0, time.UTC), // next Monday
		},
		{
			spec: "0 0 1 JAN *",
			from: time.Date(2026, 11, 25, 0, 0, 0, 0, time.UTC),
			want: time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC),
		},
		{
			// robfig/cron keeps Vixie OR semantics when both day fields are restricted.
			spec: "0 0 13 * 5",
			from: time.Date(2026, 11, 25, 0, 0, 0, 0, time.UTC),
			want: time.Date(2026, 11, 27, 0, 0, 0, 0, time.UTC),
		},
		{
			spec: "0 0 ? * 1-5", // ? means "no specific day"
			from: time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC),
			want: time.Date(2026, 11, 26, 0, 0, 0, 0, time.UTC),
		},
		{
			spec: "*/15 8-10 * * *",
			from: time.Date(2026, 11, 25, 9, 16, 0, 0, time.UTC),
			want: time.Date(2026, 11, 25, 9, 30, 0, 0, time.UTC),
		},
	}
	for _, tc := range cases {
		t.Run(tc.spec, func(t *testing.T) {
			s := mustParse(t, tc.spec)
			if got := s.Next(tc.from); !got.Equal(tc.want) {
				t.Errorf("Next(%v) = %v, want %v", tc.from, got, tc.want)
			}
		})
	}
}

func TestParseErrors(t *testing.T) {
	// Bare durations, disabled aliases, and local extensions are not part of
	// robfig/cron's standard format and must be rejected.
	for _, spec := range []string{
		"", "bogus", "0 0", "60 * * * *", "* 24 * * *", "* * 0 * *",
		"* * * 13 *", "* * * * 7", "*/0 * * * *", "@every nope",
		"15m", "@off", "disabled", "0 0 30 2 *",
	} {
		if _, err := Parse(spec); err == nil {
			t.Errorf("Parse(%q) = nil error, want an error", spec)
		}
	}
}
