package agent

import "testing"

func TestNeedsNativeSnapshotter(t *testing.T) {
	cases := map[string]bool{
		// dpkg upgrading libc6 in a rootless BuildKit build under gVisor
		"#9 8.878  unable to make backup link of './lib/x86_64-linux-gnu/ld-linux-x86-64.so.2' before installing new version: No such device": true,
		"ln: failed to create hard link 'a' => 'b': No such device":                                                                           true,
		"npm ERR! code E404": false,
		"error: failed to solve: process did not complete successfully: exit code: 1": false,
	}
	for out, want := range cases {
		if got := needsNativeSnapshotter(out); got != want {
			t.Errorf("needsNativeSnapshotter(%q) = %v, want %v", out, got, want)
		}
	}
}
