package nodeagent

import (
	"encoding/json"
	"slices"
	"testing"
)

func TestBuildSeccomp(t *testing.T) {
	var p struct {
		DefaultAction string `json:"defaultAction"`
		Syscalls      []struct {
			Names  []string `json:"names"`
			Action string   `json:"action"`
		} `json:"syscalls"`
	}
	if err := json.Unmarshal([]byte(buildSeccomp), &p); err != nil {
		t.Fatal(err)
	}
	if p.DefaultAction != "SCMP_ACT_ALLOW" || len(p.Syscalls) != 1 || p.Syscalls[0].Action != "SCMP_ACT_ERRNO" {
		t.Fatalf("profile: %+v", p)
	}
	for _, must := range []string{"bpf", "init_module", "kexec_load", "io_uring_setup", "userfaultfd", "open_by_handle_at"} {
		if !slices.Contains(p.Syscalls[0].Names, must) {
			t.Errorf("%s is not refused", must)
		}
	}
	for _, needed := range []string{"mount", "unshare", "clone", "pivot_root", "setns", "keyctl"} {
		if slices.Contains(p.Syscalls[0].Names, needed) {
			t.Errorf("%s is refused but rootless BuildKit needs it", needed)
		}
	}
}
