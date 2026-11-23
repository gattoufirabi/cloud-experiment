package api

import (
	"strings"
	"testing"
)

func TestParseLabels(t *testing.T) {
	got, err := parseLabels(" GPU, arm64  gpu,large\tx86_64 ")
	if err != nil || strings.Join(got, ",") != "arm64,gpu,large,x86_64" {
		t.Fatalf("got %v, %v", got, err)
	}
	if got, err := parseLabels(""); err != nil || len(got) != 0 {
		t.Fatalf("empty: %v, %v", got, err)
	}
	for _, bad := range []string{"-gpu", "a/b", strings.Repeat("a", 31), "a,b,c,d,e,f,g,h,i,j,k"} {
		if _, err := parseLabels(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestBuiltTag(t *testing.T) {
	for tag, want := range map[string]bool{"git-0123456789ab": true, "zip-0123456789ab": true, "git-0123": false, "latest": false, "git-0123456789AB": false} {
		if builtTagRe.MatchString(tag) != want {
			t.Errorf("%s: want %v", tag, want)
		}
	}
}
