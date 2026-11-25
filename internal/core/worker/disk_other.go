//go:build !linux

package worker

// diskSpaceMB is only implemented on Linux (where the agent runs).
func diskSpaceMB(string) (int64, int64) { return 0, 0 }
