package integration

import (
	"fmt"
	"time"

	"github.com/robfig/cron/v3"
)

// NextCron returns the next occurrence of a standard 5-field cron expression
// in the given IANA timezone — same semantics as the workflow scheduler's
// nextFireTime, duplicated here to keep internal/integration free of a
// workflow dependency.
func NextCron(expr, timezone string, now time.Time) (time.Time, error) {
	loc := time.UTC
	if timezone != "" {
		l, err := time.LoadLocation(timezone)
		if err != nil {
			return time.Time{}, fmt.Errorf("invalid timezone %q: %w", timezone, err)
		}
		loc = l
	}
	sched, err := cron.ParseStandard(expr)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid cron expression %q: %w", expr, err)
	}
	return sched.Next(now.In(loc)), nil
}
