package worker

import (
	"time"

	"platform/internal/lib/cron"
)

func NextRun(schedule string) (time.Time, bool) {
	s, err := cron.Parse(schedule)
	if err != nil {
		return time.Time{}, false
	}
	n := s.Next(time.Now())
	return n, !n.IsZero()
}
