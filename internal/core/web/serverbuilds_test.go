package web

import (
	"strings"
	"testing"
)

func TestParseLabels(t *testing.T) {
	got, err := ParseLabels(" GPU, arm64  gpu,large\tx86_64 ")
	if err != nil || strings.Join(got, ",") != "arm64,gpu,large,x86_64" {
		t.Fatalf("got %v, %v", got, err)
	}
	if got, err := ParseLabels(""); err != nil || len(got) != 0 {
		t.Fatalf("empty: %v, %v", got, err)
	}
	for _, bad := range []string{"-gpu", "a/b", strings.Repeat("a", 31), "a,b,c,d,e,f,g,h,i,j,k"} {
		if _, err := ParseLabels(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
