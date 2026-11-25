// Package schedule parses the per-job schedules used by the master.
//
// A schedule can be a duration ("15m", "@every 30s"), a standard five-field
// cron expression ("*/15 * * * *"), one of the usual aliases ("@hourly",
// "@daily", ...), or "@off" to leave a job to manual runs only. Parsing lives
// here so both config loading and the scheduler use exactly the same rules.
package schedule

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Schedule computes the next time a job should run.
type Schedule interface {
	// Next returns the first time after t at which the job should run. It
	// returns the zero time when there is no such time (an impossible cron
	// date, or a disabled schedule).
	Next(t time.Time) time.Time
	// String returns the original specification, for logs and diagnostics.
	String() string
	// Enabled reports whether the schedule ever triggers a run.
	Enabled() bool
}

var monthNames = map[string]int{
	"jan": 1, "january": 1,
	"feb": 2, "february": 2,
	"mar": 3, "march": 3,
	"apr": 4, "april": 4,
	"may": 5,
	"jun": 6, "june": 6,
	"jul": 7, "july": 7,
	"aug": 8, "august": 8,
	"sep": 9, "september": 9,
	"oct": 10, "october": 10,
	"nov": 11, "november": 11,
	"dec": 12, "december": 12,
}

var dayNames = map[string]int{
	"sun": 0, "sunday": 0,
	"mon": 1, "monday": 1,
	"tue": 2, "tuesday": 2,
	"wed": 3, "wednesday": 3,
	"thu": 4, "thursday": 4,
	"fri": 5, "friday": 5,
	"sat": 6, "saturday": 6,
}

// Parse turns a schedule specification into a Schedule.
func Parse(spec string) (Schedule, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return nil, errors.New("empty schedule")
	}
	lower := strings.ToLower(spec)

	switch lower {
	case "off", "@off", "disabled", "@disabled":
		return disabled{spec: spec}, nil
	case "@yearly", "@annually":
		return parseCron("0 0 1 1 *")
	case "@monthly":
		return parseCron("0 0 1 * *")
	case "@weekly":
		return parseCron("0 0 * * 0")
	case "@daily", "@midnight":
		return parseCron("0 0 * * *")
	case "@hourly":
		return parseCron("0 * * * *")
	}

	if strings.HasPrefix(lower, "@every") {
		fields := strings.Fields(spec)
		if len(fields) != 2 || !strings.EqualFold(fields[0], "@every") {
			return nil, fmt.Errorf("schedule %q: expected @every <duration>", spec)
		}
		return everyDuration(fields[1], spec)
	}

	// A bare duration is accepted as a shorthand for @every, which keeps
	// simple per-job intervals readable: `interval: 30m` / `schedule: 30m`.
	if _, err := time.ParseDuration(spec); err == nil {
		return everyDuration(spec, spec)
	}
	return parseCron(spec)
}

func everyDuration(raw, spec string) (Schedule, error) {
	d, err := time.ParseDuration(raw)
	if err != nil {
		return nil, fmt.Errorf("schedule %q: invalid duration: %w", spec, err)
	}
	if d <= 0 {
		return nil, fmt.Errorf("schedule %q: duration must be positive", spec)
	}
	return every{d: d, spec: spec}, nil
}

type disabled struct{ spec string }

func (d disabled) Next(time.Time) time.Time { return time.Time{} }
func (d disabled) String() string           { return d.spec }
func (d disabled) Enabled() bool            { return false }

type every struct {
	d    time.Duration
	spec string
}

func (e every) Next(t time.Time) time.Time { return t.Add(e.d) }
func (e every) String() string             { return e.spec }
func (e every) Enabled() bool              { return true }

type cron struct {
	spec          string
	minutes       map[int]bool
	hours         map[int]bool
	days          map[int]bool
	months        map[int]bool
	weekdays      map[int]bool
	dayRestricted bool
	dowRestricted bool
}

func (c cron) Next(t time.Time) time.Time {
	// Cron schedules have minute resolution: always jump to the start of the
	// next minute before testing for a match.
	cursor := t.Add(time.Minute).Truncate(time.Minute)
	limit := cursor.AddDate(5, 0, 0) // impossible dates (e.g. Feb 30) give up
	for !cursor.After(limit) {
		if c.matches(cursor) {
			return cursor
		}
		cursor = cursor.Add(time.Minute)
	}
	return time.Time{}
}

func (c cron) matches(t time.Time) bool {
	if !c.minutes[t.Minute()] || !c.hours[t.Hour()] || !c.months[int(t.Month())] {
		return false
	}
	dayMatches := c.days[t.Day()]
	weekdayMatches := c.weekdays[int(t.Weekday())]
	switch {
	case c.dayRestricted && c.dowRestricted:
		// Vixie cron semantics: when both are restricted, either one counts.
		return dayMatches || weekdayMatches
	case c.dayRestricted:
		return dayMatches
	case c.dowRestricted:
		return weekdayMatches
	default:
		return true
	}
}

func (c cron) String() string { return c.spec }
func (c cron) Enabled() bool  { return true }

func parseCron(spec string) (Schedule, error) {
	fields := strings.Fields(spec)
	if len(fields) != 5 {
		return nil, fmt.Errorf("schedule %q: cron expressions need 5 fields (minute hour day-of-month month day-of-week)", spec)
	}

	minutes, _, err := parseField(fields[0], 0, 59, nil, false, nil)
	if err != nil {
		return nil, fmt.Errorf("schedule %q: minute: %w", spec, err)
	}
	hours, _, err := parseField(fields[1], 0, 23, nil, false, nil)
	if err != nil {
		return nil, fmt.Errorf("schedule %q: hour: %w", spec, err)
	}
	days, dayRestricted, err := parseField(fields[2], 1, 31, nil, true, nil)
	if err != nil {
		return nil, fmt.Errorf("schedule %q: day-of-month: %w", spec, err)
	}
	months, _, err := parseField(fields[3], 1, 12, monthNames, false, nil)
	if err != nil {
		return nil, fmt.Errorf("schedule %q: month: %w", spec, err)
	}
	normalizeDOW := func(v int) int {
		if v == 7 { // both 0 and 7 mean Sunday
			return 0
		}
		return v
	}
	weekdays, dowRestricted, err := parseField(fields[4], 0, 7, dayNames, true, normalizeDOW)
	if err != nil {
		return nil, fmt.Errorf("schedule %q: day-of-week: %w", spec, err)
	}

	c := cron{
		spec:          spec,
		minutes:       minutes,
		hours:         hours,
		days:          days,
		months:        months,
		weekdays:      weekdays,
		dayRestricted: dayRestricted,
		dowRestricted: dowRestricted,
	}
	if c.Next(time.Now()).IsZero() {
		return nil, fmt.Errorf("schedule %q has no upcoming occurrence", spec)
	}
	return c, nil
}

// parseField expands a cron field into the set of matching values. The
// restricted flag is false for "*" and "?", which matters for the
// day-of-month / day-of-week OR rule.
func parseField(field string, min, max int, names map[string]int, question bool, normalize func(int) int) (map[int]bool, bool, error) {
	restricted := true
	if field == "*" || (question && field == "?") {
		restricted = false
		field = fmt.Sprintf("%d-%d", min, max)
	}

	values := make(map[int]bool)
	for _, part := range strings.Split(field, ",") {
		if part == "" {
			return nil, false, errors.New("empty list element")
		}
		base, step, err := splitStep(part)
		if err != nil {
			return nil, false, err
		}
		lo, hi, err := parseRange(base, min, max, names)
		if err != nil {
			return nil, false, err
		}
		if lo < min || hi > max {
			return nil, false, fmt.Errorf("value out of range %d-%d (want %d-%d)", lo, hi, min, max)
		}
		for raw := lo; raw <= hi; raw += step {
			v := raw
			if normalize != nil {
				v = normalize(v)
			}
			values[v] = true
		}
	}
	if len(values) == 0 {
		return nil, false, errors.New("field matches no values")
	}
	return values, restricted, nil
}

func splitStep(part string) (string, int, error) {
	halves := strings.SplitN(part, "/", 2)
	if len(halves) == 1 {
		return halves[0], 1, nil
	}
	if halves[1] == "" {
		return "", 0, errors.New("missing step value after '/'")
	}
	step, err := strconv.Atoi(halves[1])
	if err != nil || step <= 0 {
		return "", 0, fmt.Errorf("invalid step %q", halves[1])
	}
	return halves[0], step, nil
}

func parseRange(part string, min, max int, names map[string]int) (int, int, error) {
	if part == "*" || part == "?" {
		return min, max, nil
	}
	if strings.Contains(part, "-") {
		halves := strings.SplitN(part, "-", 2)
		lo, err := parseValue(halves[0], names)
		if err != nil {
			return 0, 0, err
		}
		hi, err := parseValue(halves[1], names)
		if err != nil {
			return 0, 0, err
		}
		if lo > hi {
			return 0, 0, fmt.Errorf("range %q is backwards", part)
		}
		return lo, hi, nil
	}
	v, err := parseValue(part, names)
	if err != nil {
		return 0, 0, err
	}
	return v, v, nil
}

func parseValue(value string, names map[string]int) (int, error) {
	lower := strings.ToLower(value)
	if names != nil {
		if v, ok := names[lower]; ok {
			return v, nil
		}
	}
	v, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("invalid value %q", value)
	}
	return v, nil
}
