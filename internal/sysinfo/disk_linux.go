//go:build linux

package sysinfo

import "syscall"

// DiskUsage reports the total and free bytes of the filesystem holding path.
// ok is false when the filesystem cannot be queried. CGO is off, but
// syscall.Statfs is pure Go on Linux.
func DiskUsage(path string) (total, free uint64, ok bool) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0, false
	}
	bsize := uint64(st.Bsize)
	return st.Blocks * bsize, st.Bavail * bsize, true
}
