//go:build !linux

package sysinfo

// DiskUsage is unavailable off Linux; ok is false.
func DiskUsage(path string) (total, free uint64, ok bool) { return 0, 0, false }
