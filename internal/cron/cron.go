// Package cron parses standard 5-field cron schedules (minute hour day-of-month month
// day-of-week, in UTC) and computes the next run time. Supported: "*", numbers, lists
// "1,15", ranges "1-5", steps "*/10" and "0-30/5", names for months and days (JAN,
// MON), and the shortcuts @hourly, @daily (@midnight), @weekly, @monthly, @yearly.
// As in classic cron, when both day-of-month and day-of-week are restricted, a day
// matching either one runs.
package cron

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

type Schedule struct {
	minute, hour, dom, month, dow uint64 // bit sets
	domStar, dowStar              bool
}

var shortcuts = map[string]string{
	"@yearly": "0 0 1 1 *", "@annually": "0 0 1 1 *", "@monthly": "0 0 1 * *",
	"@weekly": "0 0 * * 0", "@daily": "0 0 * * *", "@midnight": "0 0 * * *", "@hourly": "0 * * * *",
}

var monthNames = map[string]int{"JAN": 1, "FEB": 2, "MAR": 3, "APR": 4, "MAY": 5, "JUN": 6,
	"JUL": 7, "AUG": 8, "SEP": 9, "OCT": 10, "NOV": 11, "DEC": 12}
var dayNames = map[string]int{"SUN": 0, "MON": 1, "TUE": 2, "WED": 3, "THU": 4, "FRI": 5, "SAT": 6}

// Parse reads a schedule such as "*/15 * * * *" or "@daily".
func Parse(spec string) (*Schedule, error) {
	spec = strings.TrimSpace(spec)
	if s, ok := shortcuts[strings.ToLower(spec)]; ok {
		spec = s
	}
	f := strings.Fields(spec)
	if len(f) != 5 {
		return nil, fmt.Errorf("a schedule has 5 fields: minute hour day month weekday (for example \"0 3 * * *\")")
	}
	var s Schedule
	var err error
	if s.minute, err = field(f[0], 0, 59, nil, "minute"); err != nil {
		return nil, err
	}
	if s.hour, err = field(f[1], 0, 23, nil, "hour"); err != nil {
		return nil, err
	}
	if s.dom, err = field(f[2], 1, 31, nil, "day of month"); err != nil {
		return nil, err
	}
	if s.month, err = field(f[3], 1, 12, monthNames, "month"); err != nil {
		return nil, err
	}
	if s.dow, err = field(f[4], 0, 7, dayNames, "day of week"); err != nil {
		return nil, err
	}
	if s.dow&(1<<7) != 0 { // 7 is Sunday too
		s.dow |= 1
	}
	s.domStar, s.dowStar = f[2] == "*" || strings.HasPrefix(f[2], "*/"), f[4] == "*" || strings.HasPrefix(f[4], "*/")
	return &s, nil
}

func field(expr string, lo, hi int, names map[string]int, what string) (uint64, error) {
	var bits uint64
	for _, part := range strings.Split(expr, ",") {
		step := 1
		if i := strings.IndexByte(part, '/'); i >= 0 {
			n, err := strconv.Atoi(part[i+1:])
			if err != nil || n < 1 || n > hi {
				return 0, fmt.Errorf("invalid step in %s: %q", what, part)
			}
			step, part = n, part[:i]
		}
		from, to := lo, hi
		if part != "*" {
			a, b, isRange := strings.Cut(part, "-")
			var err error
			if from, err = value(a, lo, hi, names); err != nil {
				return 0, fmt.Errorf("invalid %s: %q", what, part)
			}
			to = from
			if isRange {
				if to, err = value(b, lo, hi, names); err != nil || to < from {
					return 0, fmt.Errorf("invalid range in %s: %q", what, part)
				}
			} else if step > 1 {
				to = hi // "5/10" means from 5, every 10
			}
		}
		for v := from; v <= to; v += step {
			bits |= 1 << uint(v)
		}
	}
	return bits, nil
}

func value(s string, lo, hi int, names map[string]int) (int, error) {
	if n, ok := names[strings.ToUpper(s)]; ok {
		return n, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < lo || n > hi {
		return 0, fmt.Errorf("out of range")
	}
	return n, nil
}

func (s *Schedule) dayMatches(t time.Time) bool {
	dom := s.dom&(1<<uint(t.Day())) != 0
	dow := s.dow&(1<<uint(t.Weekday())) != 0
	switch {
	case s.domStar && s.dowStar:
		return true
	case s.domStar:
		return dow
	case s.dowStar:
		return dom
	}
	return dom || dow
}

// Next returns the first matching minute strictly after t (UTC). The zero time means
// the schedule never matches (for example "0 0 31 2 *").
func (s *Schedule) Next(t time.Time) time.Time {
	t = t.UTC().Truncate(time.Minute).Add(time.Minute)
	limit := t.AddDate(5, 0, 0)
	for t.Before(limit) {
		if s.month&(1<<uint(t.Month())) == 0 {
			t = time.Date(t.Year(), t.Month()+1, 1, 0, 0, 0, 0, time.UTC)
			continue
		}
		if !s.dayMatches(t) {
			t = time.Date(t.Year(), t.Month(), t.Day()+1, 0, 0, 0, 0, time.UTC)
			continue
		}
		if s.hour&(1<<uint(t.Hour())) == 0 {
			t = time.Date(t.Year(), t.Month(), t.Day(), t.Hour()+1, 0, 0, 0, time.UTC)
			continue
		}
		if s.minute&(1<<uint(t.Minute())) == 0 {
			t = t.Add(time.Minute)
			continue
		}
		return t
	}
	return time.Time{}
}

// MinInterval is the shortest gap between two runs over the next few days, used to
// refuse schedules that run more often than allowed.
func (s *Schedule) MinInterval(from time.Time) time.Duration {
	prev := s.Next(from)
	if prev.IsZero() {
		return 0
	}
	min := time.Duration(1<<63 - 1)
	for i := 0; i < 60; i++ {
		n := s.Next(prev)
		if n.IsZero() {
			break
		}
		if d := n.Sub(prev); d < min {
			min = d
		}
		prev = n
	}
	return min
}
