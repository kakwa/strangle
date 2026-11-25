// Package schedule wraps the robfig/cron v3 parser used for per-job
// schedules. Specifications use that library's standard five-field cron
// format plus its descriptors (@hourly, @daily, @every 30m, ...); no local
// aliases or duration shorthand are accepted.
package schedule

import (
	"fmt"
	"time"

	"github.com/robfig/cron/v3"
)

// Schedule is a parsed robfig/cron v3 schedule.
type Schedule struct {
	spec string
	cron.Schedule
}

// Parse parses a standard cron expression or robfig descriptor. Schedules with
// no occurrence in the next five years are rejected.
func Parse(spec string) (Schedule, error) {
	parsed, err := cron.ParseStandard(spec)
	if err != nil {
		return Schedule{}, fmt.Errorf("invalid schedule %q: %w", spec, err)
	}
	if parsed.Next(time.Now()).IsZero() {
		return Schedule{}, fmt.Errorf("schedule %q has no upcoming occurrence", spec)
	}
	return Schedule{spec: spec, Schedule: parsed}, nil
}

// String returns the original specification, for logs and diagnostics.
func (s Schedule) String() string { return s.spec }

// IsZero reports whether this is the zero (unparsed) schedule.
func (s Schedule) IsZero() bool { return s.Schedule == nil }
