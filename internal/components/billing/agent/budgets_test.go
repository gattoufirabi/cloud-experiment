package agent

import (
	"testing"
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
