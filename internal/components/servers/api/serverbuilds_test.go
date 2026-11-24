package api

import (
	"testing"
)

func TestBuiltTag(t *testing.T) {
	for tag, want := range map[string]bool{"git-0123456789ab": true, "zip-0123456789ab": true, "git-0123": false, "latest": false, "git-0123456789AB": false} {
		if builtTagRe.MatchString(tag) != want {
			t.Errorf("%s: want %v", tag, want)
		}
	}
}
