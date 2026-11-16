package cron

import (
	"testing"
	"time"
)

func at(s string) time.Time {
	t, err := time.Parse("2006-01-02 15:04", s)
	if err != nil {
		panic(err)
	}
	return t
}

func TestNext(t *testing.T) {
	cases := []struct{ spec, from, want string }{
		{"* * * * *", "2026-11-16 10:00", "2026-11-16 10:01"},
		{"*/15 * * * *", "2026-11-16 10:07", "2026-11-16 10:15"},
		{"0 3 * * *", "2026-11-16 10:00", "2026-11-17 03:00"},
		{"@daily", "2026-11-16 00:00", "2026-11-17 00:00"},
		{"@hourly", "2026-11-16 10:30", "2026-11-16 11:00"},
		{"30 9 * * MON-FRI", "2026-11-20 10:00", "2026-11-23 09:30"}, // Friday after 9:30 -> Monday
		{"0 0 1 * *", "2026-12-15 00:00", "2027-01-01 00:00"},
		{"0 12 * * 7", "2026-11-16 00:00", "2026-11-22 12:00"},   // 7 = Sunday
		{"0 0 13 * FRI", "2026-11-01 00:00", "2026-11-06 00:00"}, // dom OR dow: Friday 6th first
		{"5/20 * * * *", "2026-11-16 10:06", "2026-11-16 10:25"}, // 5, 25, 45
		{"0 0 29 FEB *", "2026-03-01 00:00", "2028-02-29 00:00"}, // leap year
		{"0,30 8-10 * JAN,NOV *", "2026-11-16 10:31", "2026-11-17 08:00"},
	}
	for _, c := range cases {
		s, err := Parse(c.spec)
		if err != nil {
			t.Fatalf("%s: %v", c.spec, err)
		}
		if got := s.Next(at(c.from)); !got.Equal(at(c.want)) {
			t.Errorf("%s from %s: got %s, want %s", c.spec, c.from, got.Format("2006-01-02 15:04"), c.want)
		}
	}
}

func TestParseErrors(t *testing.T) {
	for _, spec := range []string{"", "* * * *", "60 * * * *", "* 24 * * *", "* * 0 * *", "* * * 13 *", "*/0 * * * *", "5-1 * * * *", "a * * * *", "* * * * * *"} {
		if _, err := Parse(spec); err == nil {
			t.Errorf("%q: expected an error", spec)
		}
	}
}

func TestNeverAndInterval(t *testing.T) {
	s, _ := Parse("0 0 31 2 *")
	if !s.Next(at("2026-01-01 00:00")).IsZero() {
		t.Error("31 February should never run")
	}
	s, _ = Parse("*/5 * * * *")
	if d := s.MinInterval(at("2026-01-01 00:00")); d != 5*time.Minute {
		t.Errorf("interval %v", d)
	}
	s, _ = Parse("0,1 * * * *")
	if d := s.MinInterval(at("2026-01-01 00:00")); d != time.Minute {
		t.Errorf("interval %v", d)
	}
}
