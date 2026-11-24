package nodeagent

import "encoding/json"

// buildSeccomp is the seccomp profile of the BuildKit container on a build server.
// Rootless BuildKit creates user and mount namespaces and mounts file systems inside
// them, which Docker's default profile refuses without CAP_SYS_ADMIN; so this profile
// allows by default and refuses (EPERM) the system calls a build never needs: loading
// kernel code or modules, rebooting, changing the clock, swap, accounting, raw I/O
// ports, kernel tracing and eBPF, io_uring, userfaultfd and file handles. These are
// the usual ways out of a container through a kernel bug. Every capability but
// SETUID and SETGID is dropped as well.
var buildSeccomp = func() string {
	denied := []string{
		"acct", "add_key", "bpf", "clock_adjtime", "clock_settime", "create_module", "delete_module",
		"finit_module", "fanotify_init", "get_kernel_syms", "init_module", "io_uring_enter", "io_uring_register",
		"io_uring_setup", "ioperm", "iopl", "kcmp", "kexec_file_load", "kexec_load", "lookup_dcookie",
		"move_pages", "nfsservctl", "open_by_handle_at", "perf_event_open", "process_vm_readv",
		"process_vm_writev", "quotactl", "quotactl_fd", "reboot", "request_key", "settimeofday", "stime",
		"swapoff", "swapon", "_sysctl", "sysfs", "syslog", "uselib", "userfaultfd", "ustat", "vm86", "vm86old",
	}
	profile := map[string]any{
		"defaultAction": "SCMP_ACT_ALLOW",
		"syscalls":      []map[string]any{{"names": denied, "action": "SCMP_ACT_ERRNO", "errnoRet": 1}},
	}
	b, _ := json.Marshal(profile)
	return string(b)
}()
