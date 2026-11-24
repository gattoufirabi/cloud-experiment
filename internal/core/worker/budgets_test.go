package worker

import (
	"testing"
	"time"

	"platform/internal/lib/billing"
)

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
