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
	return s
}

func TestDurationSchedules(t *testing.T) {
	from := time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC)
	for _, spec := range []string{"15m", "@every 30s"} {
		s := mustParse(t, spec)
		if !s.Enabled() {
			t.Errorf("%q: Enabled() = false", spec)
		}
		want := from.Add(15 * time.Minute)
		if spec == "@every 30s" {
			want = from.Add(30 * time.Second)
		}
		if got := s.Next(from); !got.Equal(want) {
			t.Errorf("%q: Next() = %v, want %v", spec, got, want)
		}
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
			spec: "0 3 * * MON",                                 // weekday names are case-insensitive
			from: time.Date(2026, 11, 25, 3, 1, 0, 0, time.UTC), // Wednesday
			want: time.Date(2026, 11, 30, 3, 0, 0, 0, time.UTC), // next Monday
		},
		{
			spec: "0 0 1 JAN *", // month names and day-of-month
			from: time.Date(2026, 11, 25, 0, 0, 0, 0, time.UTC),
			want: time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC),
		},
		{
			// With both fields restricted, Vixie cron uses OR: Friday the
			// 13th is nearer than the next 13th of a month.
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
			if !s.Enabled() {
				t.Fatal("Enabled() = false, want true")
			}
			if got := s.Next(tc.from); !got.Equal(tc.want) {
				t.Errorf("Next(%v) = %v, want %v", tc.from, got, tc.want)
			}
		})
	}
}

func TestDisabledSchedule(t *testing.T) {
	for _, spec := range []string{"off", "@off", "disabled"} {
		s := mustParse(t, spec)
		if s.Enabled() || !s.Next(time.Now()).IsZero() {
			t.Errorf("%q: want a disabled schedule", spec)
		}
	}
}

func TestParseErrors(t *testing.T) {
	for _, spec := range []string{
		"", "bogus", "0 0", "60 * * * *", "* 24 * * *", "* * 0 * *",
		"* * * 13 *", "* * * * 8", "*/0 * * * *", "@every 0s", "@every nope",
		"0 0 30 2 *", // no upcoming occurrence
	} {
		if _, err := Parse(spec); err == nil {
			t.Errorf("Parse(%q) = nil error, want an error", spec)
		}
	}
}
