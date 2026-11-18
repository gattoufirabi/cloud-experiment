package agent

import (
	"testing"
	"time"

	"platform/internal/billing"
)

func TestBudgetLevel(t *testing.T) {
	th := []int32{50, 80, 100}
	for _, c := range []struct {
		spent, limit int64
		want         int
	}{{0, 1000, 0}, {499, 1000, 0}, {500, 1000, 50}, {850, 1000, 80}, {1000, 1000, 100}, {5000, 1000, 100}} {
		if got := budgetLevel(c.spent, c.limit, th); got != c.want {
			t.Errorf("budgetLevel(%d, %d) = %d, want %d", c.spent, c.limit, got, c.want)
		}
	}
}

func TestForecast(t *testing.T) {
	start := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	end := start.AddDate(0, 1, 0)
	if got := billing.Forecast(100, start, end, start.Add(15*24*time.Hour)); got != 200 {
		t.Errorf("half-way forecast = %d, want 200", got)
	}
	if got := billing.Forecast(100, start, end, start.Add(time.Minute)); got != 100*720 {
		t.Errorf("first-hour forecast = %d, want %d", got, 100*720)
	}
	if got := billing.Forecast(100, start, end, end.Add(time.Hour)); got != 100 {
		t.Errorf("finished month forecast = %d, want 100", got)
	}
}
