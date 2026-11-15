package billing

import (
	"testing"
	"time"
)

func TestLinesAndTotals(t *testing.T) {
	p := Prices{Monthly: 500, VCPUHour: 1.0, GBHour: 0.5, GBOut: 2.0}
	// 250 millicores and 512 MB for 30 days, 10 GB out.
	minutes := int64(30 * 24 * 60)
	u := Usage{CPUMilliMinutes: 250 * minutes, MemMBMinutes: 512 * minutes, EgressBytes: 10e9}
	lines := Lines("Starter", p, u, 1)
	if len(lines) != 4 {
		t.Fatalf("want 4 lines, got %d: %+v", len(lines), lines)
	}
	// 180 vCPU-h * 1c = 180c; 360 GB-h * 0.5c = 180c; 10 GB * 2c = 20c; fee 500c
	want := []int64{500, 180, 180, 20}
	for i, l := range lines {
		if l.Amount != want[i] {
			t.Errorf("line %d (%s): amount %d, want %d", i, l.Description, l.Amount, want[i])
		}
	}
	sub, tax, total := Totals(lines, 20)
	if sub != 880 || tax != 176 || total != 1056 {
		t.Errorf("totals %d/%d/%d, want 880/176/1056", sub, tax, total)
	}
	if got := Money(total, "EUR"); got != "10.56 EUR" {
		t.Errorf("Money = %q", got)
	}
}

func TestFractionProratesFromCreation(t *testing.T) {
	start, end := MonthBounds(time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC))
	if start.Day() != 1 || end.Month() != 12 {
		t.Fatalf("bounds %v %v", start, end)
	}
	if f := Fraction(start, end, start.AddDate(0, -1, 0), end); f != 1 {
		t.Errorf("full month: %v", f)
	}
	half := Fraction(start, end, start.Add(15*24*time.Hour), end)
	if half < 0.49 || half > 0.51 {
		t.Errorf("created mid-month: %v", half)
	}
	if f := Fraction(start, end, end.Add(time.Hour), end); f != 0 {
		t.Errorf("created after the month: %v", f)
	}
}

func TestNoUsageNoFeeNoLines(t *testing.T) {
	if l := Lines("Free", Prices{}, Usage{}, 1); len(l) != 0 {
		t.Errorf("want no lines, got %+v", l)
	}
}
