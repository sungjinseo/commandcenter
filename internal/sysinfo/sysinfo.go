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
