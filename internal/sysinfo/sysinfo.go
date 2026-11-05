// Package sysinfo reports process-level statistics shared by the metrics and
// status endpoints.
package sysinfo

import (
	"bufio"
	"os"
	"strconv"
	"strings"
)

// RSSBytes reads the resident set size on Linux; it returns 0 elsewhere.
func RSSBytes() int64 {
	f, err := os.Open("/proc/self/status")
	if err != nil {
		return 0
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), "VmRSS:"); ok {
			kb, _ := strconv.ParseInt(strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(v), "kB")), 10, 64)
			return kb * 1024
		}
	}
	return 0
}

// MemoryLimit returns the memory limit of the process's cgroup (v2 or v1) in
// bytes, or 0 when there is none or it cannot be read.
func MemoryLimit() int64 {
	for _, f := range []string{"/sys/fs/cgroup/memory.max", "/sys/fs/cgroup/memory/memory.limit_in_bytes"} {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		// "max", or (v1) a huge number standing for none.
		n, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
		if err != nil || n <= 0 || n >= 1<<60 {
			return 0
		}
		return n
	}
	return 0
}
