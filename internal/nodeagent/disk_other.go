//go:build !linux

package nodeagent

func diskMB(string) (int64, int64) { return 0, 0 }
