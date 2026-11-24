package nodeagent

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"time"
)

// AppArmor for BuildKit on a build server. Docker's default AppArmor profile refuses
// the mounts rootless BuildKit makes inside its own user namespace, so the agent (which
// runs on the host as root) loads its own profile when it starts: Docker's default
// rules, plus mount, umount and pivot_root (and userns, on AppArmor 4 hosts, which
// otherwise restrict unprivileged user namespaces). Hosts without AppArmor (SELinux
// distributions, for example) have nothing to load; BuildKit then runs with seccomp
// and dropped capabilities only.

const buildAppArmorProfile = "staysa-buildkit"

const appArmorProfileText = `# Written by the Staysa node agent: AppArmor profile of rootless BuildKit builds.
#include <tunables/global>

profile staysa-buildkit flags=(attach_disconnected,mediate_deleted) {
  #include <abstractions/base>

  network,
  capability,
  file,
  mount,
  umount,
  pivot_root,
  @@USERNS@@
  signal (receive) peer=unconfined,
  signal (send,receive) peer=staysa-buildkit,
  ptrace (trace,read,tracedby,readby) peer=staysa-buildkit,

  deny @{PROC}/* w,
  deny @{PROC}/{[^1-9],[^1-9][^0-9],[^1-9s][^0-9y][^0-9s],[^1-9][^0-9][^0-9][^0-9/]*}/** w,
  deny @{PROC}/sys/[^k]** w,
  deny @{PROC}/sys/kernel/{?,??,[^s][^h][^m]**} w,
  deny @{PROC}/sysrq-trigger rwklx,
  deny @{PROC}/kcore rwklx,
  deny /sys/[^f]*/** wklx,
  deny /sys/f[^s]*/** wklx,
  deny /sys/fs/[^c]*/** wklx,
  deny /sys/fs/c[^g]*/** wklx,
  deny /sys/fs/cg[^r]*/** wklx,
  deny /sys/firmware/** rwklx,
  deny /sys/kernel/security/** rwklx,
}
`

// appArmorText is the profile, with the userns rule or without it (older parsers
// refuse the rule).
func appArmorText(userns bool) string {
	r := ""
	if userns {
		r = "userns,"
	}
	return strings.Replace(appArmorProfileText, "@@USERNS@@", r, 1)
}

// loadAppArmor loads the BuildKit profile when the host has AppArmor; it reports the
// security option builds use: the profile, or unconfined when there is no AppArmor
// (or it could not be loaded, which is logged).
func (a *Agent) loadAppArmor(ctx context.Context) string {
	if b, err := os.ReadFile("/sys/module/apparmor/parameters/enabled"); err != nil || strings.TrimSpace(string(b)) != "Y" {
		return "apparmor=unconfined" // no AppArmor on this host
	}
	parser, err := exec.LookPath("apparmor_parser")
	if err != nil {
		for _, p := range []string{"/sbin/apparmor_parser", "/usr/sbin/apparmor_parser"} {
			if _, e := os.Stat(p); e == nil {
				parser = p
				break
			}
		}
	}
	if parser == "" {
		a.log.Warn("AppArmor is on but apparmor_parser is missing: builds run without an AppArmor profile")
		return "apparmor=unconfined"
	}
	var lastErr string
	for _, userns := range []bool{true, false} {
		cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		cmd := exec.CommandContext(cctx, parser, "-r")
		cmd.Stdin = strings.NewReader(appArmorText(userns))
		out, err := cmd.CombinedOutput()
		cancel()
		if err == nil {
			return "apparmor=" + buildAppArmorProfile
		}
		lastErr = strings.TrimSpace(string(out))
	}
	a.log.Warn("could not load the BuildKit AppArmor profile: builds run without one", "err", lastErr)
	return "apparmor=unconfined"
}

// appArmorOpt is the AppArmor option of BuildKit containers.
func (a *Agent) appArmorOpt() string {
	if a.buildAppArmor == "" {
		return "apparmor=unconfined"
	}
	return a.buildAppArmor
}
