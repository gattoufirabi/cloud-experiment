package agent

import (
	"testing"
)

func TestDesiredInstances(t *testing.T) {
	cases := []struct {
		cur              int
		cpu              float64
		target, min, max int
		want             int
	}{
		{1, 100, 50, 1, 5, 2}, // twice the target: double
		{2, 85, 70, 1, 5, 3},  // ceil(2*85/70) = 3
		{2, 90, 70, 1, 5, 4},  // at the CPU limit: at least double
		{3, 98, 70, 1, 10, 6}, // at the CPU limit: double, not ceil(3*98/70) = 5
		{4, 99, 70, 1, 5, 5},  // doubling still stops at max
		{4, 20, 70, 1, 5, 2},  // ceil(4*20/70) = 2
		{3, 5, 70, 2, 5, 2},   // never below min
		{2, 400, 50, 1, 4, 4}, // never above max
		{1, 0, 70, 1, 3, 1},
	}
	for _, c := range cases {
		if got := desiredInstances(c.cur, c.cpu, c.target, c.min, c.max); got != c.want {
			t.Errorf("desiredInstances(%d, %.0f, %d, %d, %d) = %d, want %d", c.cur, c.cpu, c.target, c.min, c.max, got, c.want)
		}
	}
}
