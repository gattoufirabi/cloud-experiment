//go:build linux

package worker

import "syscall"

// diskSpaceMB returns the size and free space of the file system holding path (inside
// the agent container, "/" is backed by the host's Docker data disk).
func diskSpaceMB(path string) (int64, int64) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0
	}
	bs := int64(st.Bsize)
	return int64(st.Blocks) * bs >> 20, int64(st.Bavail) * bs >> 20
}
