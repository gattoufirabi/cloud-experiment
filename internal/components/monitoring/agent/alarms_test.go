package agent

import (
	"testing"
)

func TestAlarmComparison(t *testing.T) {
	cases := []struct {
		vals      []float64
		op        string
		threshold float64
		worst     float64
		breach    bool
	}{
		{[]float64{0.2, 0.9, 0.5}, ">", 0.8, 0.9, true},
		{[]float64{0.2, 0.5}, ">", 0.8, 0.5, false},
		{[]float64{3, 1, 2}, "<", 2, 1, true},
		{[]float64{3, 4}, "<=", 2, 3, false},
		{[]float64{5}, ">=", 5, 5, true},
	}
	for _, c := range cases {
		w := worst(c.vals, c.op)
		if w != c.worst || breaches(w, c.op, c.threshold) != c.breach {
			t.Errorf("%v %s %v: worst %v breach %v", c.vals, c.op, c.threshold, w, breaches(w, c.op, c.threshold))
		}
	}
}
